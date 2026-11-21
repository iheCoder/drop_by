package dropby

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"
)

const autonomousHydrogenOutputTokens = 8192

// 探索额度跟着当前生效的周额度走。未填写时用产品默认，测试里显式写成 1 表示不另设更紧的上限。
func (c Config) explorationWindow() time.Duration {
	if c.ExplorationWindow <= 0 {
		return 8 * time.Hour
	}
	return c.ExplorationWindow
}

func (c Config) explorationShares() (weekly, window float64) {
	weekly, window = c.ExplorationShare, c.ExplorationWindowShare
	if weekly <= 0 {
		weekly = 0.4
	}
	if window <= 0 {
		window = 0.25
	}
	return weekly, window
}

func (c Config) explorationAllowances(siteWeekly int64) (weekly, window int64) {
	share, windowShare := c.explorationShares()
	weekly = shareMicros(siteWeekly, share)
	window = shareMicros(weekly, windowShare)
	return weekly, window
}

func shareMicros(total int64, share float64) int64 {
	if total <= 0 || share <= 0 {
		return 0
	}
	millionths := int64(math.Round(share * 1_000_000))
	if millionths <= 0 {
		return 0
	}
	return (total*millionths + 500_000) / 1_000_000
}

type windowCharge struct {
	at     time.Time
	amount int64
}

type sqlQuery interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func autonomousWeekSpend(q queryRower, at time.Time) (int64, error) {
	start, end := budgetWeek(at)
	var spent int64
	err := q.QueryRow(`SELECT coalesce(sum(autonomous_cost_micros),0) FROM usage WHERE day>=? AND day<?`, start.Format(usageDate), end.Format(usageDate)).Scan(&spent)
	return spent, err
}

// 窗口按每笔调用的开始时间向后看。已结算用实际金额，在途和结果未知保留预留。
func autonomousWindowCharges(q sqlQuery, at time.Time, window time.Duration) (int64, []windowCharge, error) {
	if window <= 0 {
		window = 8 * time.Hour
	}
	cutoff := at.Add(-window)
	rows, err := q.Query(`SELECT started_at, status, reserved_micros, cost_micros FROM usage_calls WHERE autonomous=1 AND day>=?`, cutoff.Format(usageDate))
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var spent int64
	var charges []windowCharge
	for rows.Next() {
		var startedRaw, status string
		var reserved, cost int64
		if err = rows.Scan(&startedRaw, &status, &reserved, &cost); err != nil {
			return 0, nil, err
		}
		started, parseErr := time.Parse(time.RFC3339Nano, startedRaw)
		if parseErr != nil {
			started = at // 读不出时刻的记录继续占用窗口，不能把它当成已经滑出。
		}
		if !started.After(cutoff) {
			continue
		}
		amount := reserved
		if status == "settled" {
			amount = cost
		}
		if amount < 0 {
			amount = 0
		}
		spent += amount
		charges = append(charges, windowCharge{at: started, amount: amount})
	}
	return spent, charges, rows.Err()
}

func (s *Store) explorationBalances(q sqlQuery, cfg Config, at time.Time, siteLimit int64) (siteRemaining, weekRemaining, windowRemaining, weeklyAllowance, windowAllowance int64, err error) {
	weeklyAllowance, windowAllowance = cfg.explorationAllowances(siteLimit)
	var weekSpent int64
	weekSpent, err = autonomousWeekSpend(q, at)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	var windowSpent int64
	windowSpent, _, err = autonomousWindowCharges(q, at, cfg.explorationWindow())
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	usage, err := s.weeklyUsageAt(at)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	return max(int64(0), siteLimit-usage.CostMicros), max(int64(0), weeklyAllowance-weekSpent), max(int64(0), windowAllowance-windowSpent), weeklyAllowance, windowAllowance, nil
}

// 站点周额度已经在同一事务里检查过。这里只拒绝自主探索放不进探索周额度或滚动窗口的预留。
func rejectExploration(tx *sql.Tx, c Config, at time.Time, limit, cost int64) error {
	weeklyAllowance, windowAllowance := c.explorationAllowances(limit)
	weekSpent, err := autonomousWeekSpend(tx, at)
	if err != nil {
		return err
	}
	spent, charges, err := autonomousWindowCharges(tx, at, c.explorationWindow())
	if err != nil {
		return err
	}
	weekRemaining := max(int64(0), weeklyAllowance-weekSpent)
	windowRemaining := max(int64(0), windowAllowance-spent)
	if cost <= weekRemaining && cost <= windowRemaining {
		return nil
	}
	return explorationRejection(c, at, cost, weekRemaining, weeklyAllowance, windowRemaining, windowAllowance, spent, charges)
}

func explorationRejection(c Config, at time.Time, cost, weekRemaining, weeklyAllowance, windowRemaining, windowAllowance, spent int64, charges []windowCharge) *budgetRejection {
	window := c.explorationWindow()
	_, weekEnd := budgetWeek(at)
	label := durationLabel(window)
	if cost > windowAllowance {
		return &budgetRejection{
			required: cost, skipWindowRetry: true,
			message: fmt.Sprintf("本次调用需预留 $%.3f，超过每%s的自主探索额度 $%.3f，未完工作已保留。", float64(cost)/1e6, label, float64(windowAllowance)/1e6),
		}
	}
	if cost > weeklyAllowance {
		return &budgetRejection{
			required: cost, skipWindowRetry: true,
			message: fmt.Sprintf("本次调用需预留 $%.3f，超过本周自主探索额度 $%.3f，未完工作已保留。", float64(cost)/1e6, float64(weeklyAllowance)/1e6),
		}
	}
	windowShort := cost > windowRemaining
	weekShort := cost > weekRemaining
	var slide time.Time
	if windowShort {
		resume, ok := windowResume(charges, spent, cost, windowAllowance, window)
		if ok && resume.After(at) {
			slide = resume
		}
	}
	if windowShort && slide.IsZero() {
		// 账面对不上时不要按窗口紧凑重试；等到周额度恢复再判断一次。
		slide = weekEnd
	}
	if windowShort && (!weekShort || !slide.Before(weekEnd)) {
		when := slide.Format("1月2日 15:04 MST")
		if windowRemaining == 0 {
			return &budgetRejection{required: cost, resumeAt: slide, message: fmt.Sprintf("最近%s的自主探索额度已用完，未完工作已保留，%s 后再判断。", label, when)}
		}
		return &budgetRejection{required: cost, resumeAt: slide, message: fmt.Sprintf("最近%s的自主探索额度还剩 $%.3f，本次调用需预留 $%.3f，未完工作已保留，%s 后再判断。", label, float64(windowRemaining)/1e6, float64(cost)/1e6, when)}
	}
	reset := weekEnd.Format("1月2日 15:04 MST")
	if weekRemaining == 0 {
		return &budgetRejection{required: cost, resumeAt: weekEnd, message: fmt.Sprintf("本周自主探索额度已用完，未完工作已保留，%s 恢复。", reset)}
	}
	return &budgetRejection{required: cost, resumeAt: weekEnd, message: fmt.Sprintf("本周自主探索额度还剩 $%.3f，本次调用需预留 $%.3f，暂时无法开始；未完工作已保留，%s 恢复。", float64(weekRemaining)/1e6, float64(cost)/1e6, reset)}
}

// 从最旧的一笔开始释放。返回的时刻是“刚好够本次预留”的那一笔滑出窗口的时间。
func windowResume(charges []windowCharge, spent, cost, allowance int64, window time.Duration) (time.Time, bool) {
	if cost > allowance {
		return time.Time{}, false
	}
	need := spent + cost - allowance
	if need <= 0 {
		return time.Time{}, false
	}
	sort.Slice(charges, func(i, j int) bool { return charges[i].at.Before(charges[j].at) })
	var freed int64
	var resume time.Time
	for _, charge := range charges {
		freed += charge.amount
		resume = wakeAfter(charge.at.Add(window))
		if freed >= need {
			return resume, true
		}
	}
	return time.Time{}, false
}

func wakeAfter(boundary time.Time) time.Time {
	ms := boundary.UnixMilli()
	if time.UnixMilli(ms).Before(boundary) {
		return time.UnixMilli(ms + 1)
	}
	return time.UnixMilli(ms)
}

func durationLabel(d time.Duration) string {
	if d > 0 && d%time.Hour == 0 {
		return fmt.Sprintf("%d小时", int(d/time.Hour))
	}
	if d > 0 && d%time.Minute == 0 {
		return fmt.Sprintf("%d分钟", int(d/time.Minute))
	}
	return d.String()
}

func explorationHoldDue() time.Time {
	return time.Date(9999, time.January, 1, 0, 0, 0, 0, time.Local)
}
