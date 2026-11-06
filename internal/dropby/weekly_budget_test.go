package dropby

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWeeklyBudgetIncludesExistingDailyLedger(t *testing.T) {
	// 升级前已经按日记录费用；升级不能把周一以来的消费清零，也不能混入上周费用。
	s, _, _ := fixture(t)
	at := time.Date(2026, time.November, 4, 12, 0, 0, 0, time.Local)
	for _, entry := range []struct {
		day        string
		cost, auto int64
	}{
		{"2026-11-01", 19_000_000, 6_000_000}, // 上周日，应排除。
		{"2026-11-02", 5_000_000, 1_000_000},
		{"2026-11-03", 6_000_000, 2_000_000},
	} {
		if _, err := s.db.Exec(`INSERT INTO usage(day,calls,cost_micros,autonomous_cost_micros,input_tokens,output_tokens) VALUES(?,2,?,?,100,20)`, entry.day, entry.cost, entry.auto); err != nil {
			t.Fatal(err)
		}
	}

	cfg := testConfig()
	cfg.WeeklyBudgetMicros, cfg.AutonomousBudgetMicros = 20_000_000, 7_000_000
	// 今天还有 9 美元，预留 8 美元成功；下一天的 2 美元请求仍超过本周仅剩的 1 美元。
	day, err := s.reserveContextAt(context.Background(), cfg, 8_000_000, at)
	if err != nil || day != "2026-11-04" {
		t.Fatalf("本周可用费用未正确继承: day=%s err=%v", day, err)
	}
	if _, err = s.reserveContextAt(context.Background(), cfg, 2_000_000, at.AddDate(0, 0, 1)); !errors.Is(err, ErrBudget) || strings.Contains(err.Error(), "已用完") || !strings.Contains(err.Error(), "$1.000") {
		t.Fatalf("应说明不足以预留，而非谎称用完: %v", err)
	}
	u, err := s.weeklyUsageAt(at)
	if err != nil || u.CostMicros != 19_000_000 || u.AutonomousCostMicros != 3_000_000 || u.Calls != 5 || u.InputTokens != 200 || u.OutputTokens != 40 {
		t.Fatalf("周账本未正确汇总既有记录: %+v err=%v", u, err)
	}
	if !strings.HasPrefix(u.WeekStart, "2026-11-02T00:00:00") || !strings.HasPrefix(u.ResetAt, "2026-11-09T00:00:00") {
		t.Fatalf("周起止时间错误: %+v", u)
	}
}

func TestWeeklyBudgetRolloverAndLateSettlement(t *testing.T) {
	// 周日发出的请求在下周一才返回：结算必须回到周日，不能消耗下一周的新额度。
	s, _, _ := fixture(t)
	cfg := testConfig()
	cfg.WeeklyBudgetMicros, cfg.AutonomousBudgetMicros = 1_000_000, 700_000
	sunday := time.Date(2026, time.November, 8, 23, 59, 0, 0, time.Local)
	monday := sunday.Add(2 * time.Minute)
	ctx := context.WithValue(context.Background(), autonomyKey{}, "weekly-check")
	oldDay, err := s.reserveContextAt(ctx, cfg, 600_000, sunday)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.reserveContextAt(ctx, cfg, 600_000, monday); err != nil {
		t.Fatalf("周一应有新额度: %v", err)
	}
	if err = s.SettleContext(ctx, oldDay, 600_000, 250_000, 10, 5); err != nil {
		t.Fatal(err)
	}
	oldUsage, err := s.weeklyUsageAt(sunday)
	if err != nil {
		t.Fatal(err)
	}
	newUsage, err := s.weeklyUsageAt(monday)
	if err != nil || oldUsage.CostMicros != 250_000 || newUsage.CostMicros != 600_000 || newUsage.AutonomousCostMicros != 600_000 {
		t.Fatalf("跨周结算混入了新账期: old=%+v new=%+v err=%v", oldUsage, newUsage, err)
	}
	// 自主子上限跨日累加；不足时仍需保留前台预算通道。
	if _, err = s.reserveContextAt(ctx, cfg, 150_000, monday.AddDate(0, 0, 1)); !errors.Is(err, errAutonomyBudget) {
		t.Fatalf("自主额度应独立受限: %v", err)
	}
	if _, err = s.reserveContextAt(context.Background(), cfg, 350_000, monday.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("自主额度不足不能挡住真人可用的总额度: %v", err)
	}
}

func TestWeeklyBudgetExhaustionAndCallLimitAreDistinct(t *testing.T) {
	// 已经达到金额上限、仍有余额但下一次放不下、调用次数到顶，是三个不同事实。
	for _, tc := range []struct {
		name       string
		budget     int64
		calls      int
		wantPhrase string
	}{
		{"spent", 100, 10, "运行预算已用完"},
		{"reservation", 150, 10, "本次调用需预留"},
		{"calls", 1_000, 1, "调用上限"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg := testConfig()
			cfg.WeeklyBudgetMicros, cfg.WeeklyCalls = tc.budget, tc.calls
			if _, err := s.Reserve(cfg, 100); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reserve(cfg, 100); !errors.Is(err, ErrBudget) || !strings.Contains(err.Error(), tc.wantPhrase) {
				t.Fatalf("错误原因不准确: %v", err)
			}
		})
	}
}

func TestBudgetWeekUsesCalendarMondayAcrossDSTAndYear(t *testing.T) {
	// 时区内的自然周不能硬算 7*24 小时；跨夏令时的一周仍需在下周一午夜重置。
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at    time.Time
		start string
		end   string
	}{
		{time.Date(2026, time.March, 8, 12, 0, 0, 0, newYork), "2026-03-02T00:00:00-05:00", "2026-03-09T00:00:00-04:00"},
		{time.Date(2027, time.January, 1, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*3600)), "2026-12-28T00:00:00+08:00", "2027-01-04T00:00:00+08:00"},
	} {
		start, end := budgetWeek(tc.at)
		if start.Format(time.RFC3339) != tc.start || end.Format(time.RFC3339) != tc.end {
			t.Fatalf("自然周边界错误: %s -> %s", start, end)
		}
	}
}

func TestWeeklyConfigDefaultsAndExplicitLegacyMigration(t *testing.T) {
	// 清除额度环境以检查真正默认值；不读取、不打印环境中的 API 密钥。
	for _, key := range []string{"DROPBY_WEEKLY_BUDGET_USD", "DROPBY_WEEKLY_CALLS", "DROPBY_AUTONOMOUS_WEEKLY_BUDGET_USD", "DROPBY_DAILY_BUDGET_USD", "DROPBY_DAILY_CALLS", "DROPBY_AUTONOMOUS_BUDGET_USD"} {
		t.Setenv(key, "")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WeeklyBudgetMicros != 20_000_000 || cfg.AutonomousBudgetMicros != 7_000_000 || cfg.WeeklyCalls != 4200 {
		t.Fatal("默认预算应为每周 20 美元、其中自主 7 美元、4200 次调用")
	}
	// 旧的日配置必须明确迁移，不能悄悄把每天 3 美元解释成每周 3 美元。
	t.Setenv("DROPBY_DAILY_BUDGET_USD", "3")
	if _, err = LoadConfig(); err == nil || !strings.Contains(err.Error(), "DROPBY_WEEKLY_BUDGET_USD") {
		t.Fatalf("应提示旧配置迁移: %v", err)
	}
	t.Setenv("DROPBY_WEEKLY_BUDGET_USD", "20")
	cfg, err = LoadConfig()
	if err != nil || cfg.WeeklyBudgetMicros != 20_000_000 {
		t.Fatalf("明确新配置应优先于残留旧配置: %v", err)
	}
}
