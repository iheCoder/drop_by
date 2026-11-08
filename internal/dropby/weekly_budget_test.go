package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	cfg.WeeklyBudgetMicros = 20_000_000
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
	cfg.WeeklyBudgetMicros = 1_000_000
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
	// 自主和前台共享跨日总预算，不设置第二个隐含门槛。
	if _, err = s.reserveContextAt(ctx, cfg, 150_000, monday.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("自主额度应独立受限: %v", err)
	}
	if _, err = s.reserveContextAt(context.Background(), cfg, 250_000, monday.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("自主额度不足不能挡住真人可用的总额度: %v", err)
	}
}

func TestWeeklyBudgetExhaustionAndReservationAreDistinct(t *testing.T) {
	// 已经达到金额上限、仍有余额但下一次放不下、调用次数到顶，是三个不同事实。
	for _, tc := range []struct {
		name       string
		budget     int64
		calls      int
		wantPhrase string
	}{
		{"spent", 100, 10, "运行预算已用完"},
		{"reservation", 150, 10, "本次调用需预留"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg := testConfig()
			cfg.WeeklyBudgetMicros = tc.budget
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
	if cfg.WeeklyBudgetMicros != 20_000_000 {
		t.Fatal("默认总预算应为每周 20 美元")
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

func TestParseWeeklyBudgetUSD(t *testing.T) {
	for _, dollars := range []string{"10", "20", "30", "50", "80", "100", "20.0"} {
		got, err := parseWeeklyBudgetUSD(json.RawMessage(dollars))
		if err != nil || got != int64(mustInt(dollars))*1_000_000 && dollars != "20.0" {
			t.Fatalf("%s: got %d err=%v", dollars, got, err)
		}
		if dollars == "20.0" && (err != nil || got != 20_000_000) {
			t.Fatalf("整数档位的小数写法应接受: %d %v", got, err)
		}
	}
	for _, raw := range []string{"12.5", "25", "0", "1000", "20.5", `"20"`, "null", "", "-10"} {
		if _, err := parseWeeklyBudgetUSD(json.RawMessage(raw)); err == nil || !strings.Contains(err.Error(), "选一档") {
			t.Fatalf("%s 应拒绝: %v", raw, err)
		}
	}
}

func mustInt(raw string) int64 {
	if raw == "20.0" {
		return 20
	}
	var n int64
	for _, c := range raw {
		n = n*10 + int64(c-'0')
	}
	return n
}

func TestUserWeeklyBudgetReplacesConfiguredLimit(t *testing.T) {
	// 临时上限只在用户还没选定档位时生效。选定之后按那一档预留，已经花掉的账不改写。
	s, c, m := fixture(t)
	cfg := testConfig()
	cfg.WeeklyBudgetMicros = 20_000_000
	cfg.TemporaryBudgetMicros = 80_000_000
	cfg.TemporaryBudgetUntil = time.Now().Add(time.Hour)
	limit, source, err := budgetLimit(s.db, cfg, time.Now())
	if err != nil || limit != 80_000_000 || source != "temporary" {
		t.Fatalf("未保存时应沿用临时上限: %d %s %v", limit, source, err)
	}
	if _, err = s.Reserve(cfg, 18_000_000); err != nil {
		t.Fatal(err)
	}
	if err = s.setWeeklyBudget(30_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reserve(cfg, 10_000_000); err != nil {
		t.Fatalf("30 美元档位下还应剩 12 美元: %v", err)
	}
	if _, err = s.Reserve(cfg, 3_000_000); !errors.Is(err, ErrBudget) || !strings.Contains(err.Error(), "$2.000") || strings.Contains(err.Error(), "已用完") {
		t.Fatalf("应说明还剩 2 美元，而不是已经用完: %v", err)
	}
	var spent int64
	if err = s.db.QueryRow(`SELECT coalesce(sum(cost_micros),0) FROM usage`).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if err = s.setWeeklyBudget(10_000_000); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err = s.db.QueryRow(`SELECT coalesce(sum(cost_micros),0) FROM usage`).Scan(&after); err != nil || after != spent || spent != 28_000_000 {
		t.Fatalf("调低档位不应改写账本: before=%d after=%d err=%v", spent, after, err)
	}
	if _, err = s.Reserve(cfg, 1); !errors.Is(err, ErrBudget) || !strings.Contains(err.Error(), "已用完") {
		t.Fatalf("已花 28 美元、档位 10 美元时应视为用完: %v", err)
	}

	waiting := func(required int64) Work {
		t.Helper()
		job := addWork(t, s, c, m, "research")
		if claimed, err := s.Claim(job.ID); err != nil || !claimed {
			t.Fatal(err)
		}
		job.BudgetRequired = required
		if err := s.Finish(job, "waiting_budget", "额度暂时不足", nextBudgetWeek()); err != nil {
			t.Fatal(err)
		}
		return job
	}
	small := waiting(5_000_000)
	large := waiting(23_000_000)
	if err = s.recheckBudgetWait(cfg); err != nil {
		t.Fatal(err)
	}
	for _, job := range []Work{small, large} {
		got, err := s.ReadWork(c.ID, job.ID)
		if err != nil || got.Status != "waiting_budget" {
			t.Fatalf("剩余为零时不应恢复等待中的工作: %+v %v", got, err)
		}
	}
	if err = s.setWeeklyBudget(50_000_000); err != nil {
		t.Fatal(err)
	}
	if err = s.recheckBudgetWait(cfg); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ReadWork(c.ID, small.ID)
	if err != nil || resumed.Status != "pending" {
		t.Fatalf("调高到放得下的档位后应恢复: %+v %v", resumed, err)
	}
	held, err := s.ReadWork(c.ID, large.ID)
	if err != nil || held.Status != "waiting_budget" {
		t.Fatalf("仍然放不下的工作应继续等待: %+v %v", held, err)
	}
}

func TestSavedWeeklyBudgetSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.WeeklyBudgetMicros = 20_000_000
	cfg.TemporaryBudgetMicros = 80_000_000
	cfg.TemporaryBudgetUntil = time.Now().Add(time.Hour)
	if err = s.setWeeklyBudget(50_000_000); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	limit, source, err := budgetLimit(reopened.db, cfg, time.Now())
	if err != nil || limit != 50_000_000 || source != "custom" {
		t.Fatalf("重启后仍应使用保存的档位，而不是临时上限: %d %s %v", limit, source, err)
	}
	if _, err = reopened.Reserve(cfg, 40_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Reserve(cfg, 20_000_000); !errors.Is(err, ErrBudget) {
		t.Fatalf("50 美元档位不应放进 60 美元: %v", err)
	}
}

func TestWeeklyBudgetStopsInState(t *testing.T) {
	s, _, _ := fixture(t)
	cfg := testConfig()
	cfg.WeeklyBudgetMicros = 20_000_000
	cfg.TemporaryBudgetMicros = 80_000_000
	cfg.TemporaryBudgetUntil = time.Now().Add(time.Hour)
	h := NewHandler(NewEngine(s, nil, cfg))
	before := preferenceRequest(h, "GET", "/api/state", map[string]any{})
	if before.Code != 200 {
		t.Fatal(before.Body.String())
	}
	var state struct {
		WeeklyBudgetMicros  int64   `json:"weekly_budget_micros"`
		BudgetSource        string  `json:"budget_source"`
		WeeklyBudgetOptions []int64 `json:"weekly_budget_options"`
	}
	if err := json.Unmarshal(before.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.WeeklyBudgetMicros != 80_000_000 || state.BudgetSource != "temporary" || len(state.WeeklyBudgetOptions) != 6 || state.WeeklyBudgetOptions[0] != 10_000_000 || state.WeeklyBudgetOptions[5] != 100_000_000 {
		t.Fatalf("状态应给出临时上限和滚轮档位: %+v", state)
	}
	rejected := preferenceRequest(h, "PATCH", "/api/budget", map[string]any{"weekly_budget_usd": 12.5})
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "选一档") {
		t.Fatalf("滚轮以外的金额应拒绝: %d %s", rejected.Code, rejected.Body.String())
	}
	saved := preferenceRequest(h, "PATCH", "/api/budget", map[string]any{"weekly_budget_usd": 50})
	if saved.Code != 200 || !strings.Contains(saved.Body.String(), `"budget_source":"custom"`) || !strings.Contains(saved.Body.String(), `"weekly_budget_micros":50000000`) {
		t.Fatalf("保存档位失败: %d %s", saved.Code, saved.Body.String())
	}
	profile := preferenceRequest(h, "PATCH", "/api/profile", map[string]any{"name": "你", "avatar": "", "avatar_icon": "orbit", "color": "blue"})
	if profile.Code != 200 {
		t.Fatal(profile.Body.String())
	}
	after := preferenceRequest(h, "GET", "/api/state", map[string]any{})
	if err := json.Unmarshal(after.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if after.Code != 200 || state.WeeklyBudgetMicros != 50_000_000 || state.BudgetSource != "custom" {
		t.Fatalf("修改资料不应清掉每周档位: %d %+v", after.Code, state)
	}
	if _, err := s.db.Exec(`UPDATE user_settings SET weekly_budget_micros=123 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := budgetLimit(s.db, cfg, time.Now()); err == nil || !strings.Contains(err.Error(), "已保存的每周限额无效") {
		t.Fatalf("损坏的档位不能当成一个新上限: %v", err)
	}
}
