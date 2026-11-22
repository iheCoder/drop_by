package dropby

import (
	"database/sql"
	"fmt"
	"math"
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

type queryRows interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func autonomousWeekSpend(q queryRower, at time.Time) (int64, error) {
	start, end := budgetWeek(at)
	var spent int64
	err := q.QueryRow(`SELECT coalesce(sum(autonomous_cost_micros),0) FROM usage WHERE day>=? AND day<?`, start.Format(usageDate), end.Format(usageDate)).Scan(&spent)
	return spent, err
}

// 窗口按每笔调用的开始时间向后看。已结算用实际金额，在途和结果未知保留预留。
func autonomousWindowSpend(q queryRows, at time.Time, window time.Duration) (int64, error) {
	cutoff := at.Add(-window)
	rows, err := q.Query(`SELECT started_at, status, reserved_micros, cost_micros FROM usage_calls WHERE autonomous=1 AND day>=?`, cutoff.Format(usageDate))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var spent int64
	for rows.Next() {
		var startedRaw, status string
		var reserved, cost int64
		if err = rows.Scan(&startedRaw, &status, &reserved, &cost); err != nil {
			return 0, err
		}
		if started, err := time.Parse(time.RFC3339Nano, startedRaw); err == nil && !started.After(cutoff) {
			continue
		}
		// 读不出时刻的旧记录继续占用窗口。
		amount := reserved
		if status == "settled" {
			amount = cost
		}
		spent += max(int64(0), amount)
	}
	return spent, rows.Err()
}

func explorationRemaining(q queryRows, cfg Config, at time.Time, siteLimit, weekSpent int64) (week, window int64, err error) {
	windowSpent, err := autonomousWindowSpend(q, at, cfg.explorationWindow())
	if err != nil {
		return 0, 0, err
	}
	weeklyAllowance, windowAllowance := cfg.explorationAllowances(siteLimit)
	// 异常的旧汇总不能把可用余额抬到配置额度以上。
	return max(int64(0), weeklyAllowance-max(int64(0), weekSpent)), max(int64(0), windowAllowance-windowSpent), nil
}

// 站点周额度已经在同一事务里检查过。
func rejectExploration(tx *sql.Tx, cfg Config, at time.Time, limit, cost int64) error {
	weekSpent, err := autonomousWeekSpend(tx, at)
	if err != nil {
		return err
	}
	week, window, err := explorationRemaining(tx, cfg, at, limit, weekSpent)
	if err != nil {
		return err
	}
	if cost <= week && cost <= window {
		return nil
	}
	label, remaining := "本周自主探索额度", week
	if window < week {
		label, remaining = "最近"+durationLabel(cfg.explorationWindow())+"的自主探索额度", window
	}
	return &budgetRejection{required: cost, message: fmt.Sprintf("%s还剩 $%.3f，本次调用需预留 $%.3f，未完工作已保留。", label, float64(remaining)/1e6, float64(cost)/1e6)}
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
