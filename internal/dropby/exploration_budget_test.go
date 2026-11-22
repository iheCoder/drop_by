package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"drop_by/internal/dropby/model/gemini"
)

func explorationConfig(site int64) Config {
	cfg := testConfig()
	cfg.WeeklyBudgetMicros = site
	cfg.ExplorationShare = 0.4
	cfg.ExplorationWindow = 8 * time.Hour
	cfg.ExplorationWindowShare = 0.25
	return cfg
}

func reserveExplorationAt(t *testing.T, s *Store, cfg Config, at time.Time, member, root, purpose string, cost int64) usageReservation {
	t.Helper()
	ctx := context.WithValue(context.Background(), autonomyKey{}, root)
	record := usageCall{purpose: purpose, member: member, root: root}
	got, err := s.reserveBudget(ctx, cfg, cost, at, &record)
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	return got
}

func TestExplorationAllowancesFollowTheBudgetInEffect(t *testing.T) {
	// 比例跟着当前生效的周额度走，不把 20 美元写进计算。
	base := explorationConfig(20_000_000)
	weekly, window := base.explorationAllowances(20_000_000)
	if weekly != 8_000_000 || window != 2_000_000 {
		t.Fatalf("$20 应为探索每周 $8、每 8 小时 $2，实际 %d/%d", weekly, window)
	}
	weekly, window = base.explorationAllowances(100_000_000)
	if weekly != 40_000_000 || window != 10_000_000 {
		t.Fatalf("$100 应为 $40/$10，实际 %d/%d", weekly, window)
	}
	changed := base
	changed.ExplorationShare = 0.5
	weekly, window = changed.explorationAllowances(100_000_000)
	if weekly != 50_000_000 || window != 12_500_000 {
		t.Fatalf("只改探索比例后应为 $50/$12.5，实际 %d/%d", weekly, window)
	}
	changed = base
	changed.ExplorationWindowShare = 0.5
	weekly, window = changed.explorationAllowances(100_000_000)
	if weekly != 40_000_000 || window != 20_000_000 {
		t.Fatalf("只改窗口比例后应为 $40/$20，实际 %d/%d", weekly, window)
	}

	s, _, _ := fixture(t)
	cfg, err := decodeConfig(strings.NewReader(`
server: {address: '127.0.0.1:8090'}
budget: {weekly_usd: 20}
models:
  providers:
    OpenAI: {protocol: openai_responses, base_url: 'https://example.test/v1', api_key: 'k'}
`), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.setWeeklyBudget(100_000_000); err != nil {
		t.Fatal(err)
	}
	limit, source, err := budgetLimit(s.db, cfg, time.Now())
	if err != nil || source != "custom" || limit != 100_000_000 {
		t.Fatalf("已保存档位应盖过文件里的 $20: %d %s %v", limit, source, err)
	}
	weekly, window = cfg.explorationAllowances(limit)
	if weekly != 40_000_000 || window != 10_000_000 {
		t.Fatalf("按生效的 $100 计算应为 $40/$10，实际 %d/%d", weekly, window)
	}

	cfg.TemporaryBudgetMicros = 80_000_000
	cfg.TemporaryBudgetUntil = time.Now().Add(time.Hour)
	if _, err = s.db.Exec(`DELETE FROM user_settings`); err != nil {
		t.Fatal(err)
	}
	limit, source, err = budgetLimit(s.db, cfg, time.Now())
	weekly, window = cfg.explorationAllowances(limit)
	if err != nil || source != "temporary" || weekly != 32_000_000 || window != 8_000_000 {
		t.Fatalf("临时 $80 应为探索 $32/$8: %d %s %d/%d %v", limit, source, weekly, window, err)
	}

	env := map[string]string{"DROPBY_EXPLORATION_SHARE": "0.2", "DROPBY_EXPLORATION_WINDOW": "4h", "DROPBY_EXPLORATION_WINDOW_SHARE": "0.5"}
	fileWins, err := decodeConfig(strings.NewReader(`
server: {address: '127.0.0.1:8090'}
budget: {weekly_usd: 20, exploration_share: 0.4, exploration_window: 8h, exploration_window_share: 0.25}
models:
  providers:
    OpenAI: {protocol: openai_responses, base_url: 'https://example.test/v1', api_key: 'k'}
`), func(key string) string { return env[key] })
	if err != nil || fileWins.ExplorationShare != 0.4 || fileWins.ExplorationWindow != 8*time.Hour || fileWins.ExplorationWindowShare != 0.25 {
		t.Fatalf("文件应盖过环境: %+v %v", fileWins, err)
	}
	fromEnv, err := decodeConfig(strings.NewReader(`
server: {address: '127.0.0.1:8090'}
budget: {weekly_usd: 20}
models:
  providers:
    OpenAI: {protocol: openai_responses, base_url: 'https://example.test/v1', api_key: 'k'}
`), func(key string) string { return env[key] })
	if err != nil || fromEnv.ExplorationShare != 0.2 || fromEnv.ExplorationWindow != 4*time.Hour || fromEnv.ExplorationWindowShare != 0.5 {
		t.Fatalf("省略的字段应使用环境: %+v %v", fromEnv, err)
	}
	for _, raw := range []string{"0", "1.5", "40", "-0.1"} {
		_, err = decodeConfig(strings.NewReader(fmt.Sprintf(`
server: {address: '127.0.0.1:8090'}
budget: {weekly_usd: 20, exploration_share: %s}
models:
  providers:
    OpenAI: {protocol: openai_responses, base_url: 'https://example.test/v1', api_key: 'k'}
`, raw)), func(string) string { return "" })
		if err == nil || !strings.Contains(err.Error(), "exploration_share") {
			t.Fatalf("比例 %s 应拒绝: %v", raw, err)
		}
	}
}

func TestExplorationWindowSlidesByEntryAndSurvivesTheWeek(t *testing.T) {
	s, _, _ := fixture(t)
	cfg := explorationConfig(20_000_000)
	monday := time.Date(2026, time.November, 9, 0, 0, 0, 0, time.Local)
	for monday.Weekday() != time.Monday {
		monday = monday.AddDate(0, 0, 1)
	}
	earlier := monday.Add(-4 * time.Hour) // 周日 20:00 左右，恰好跨过周一凌晨 8 小时
	later := monday.Add(-2 * time.Hour)
	morning := monday.Add(4 * time.Hour)
	first := reserveExplorationAt(t, s, cfg, earlier, "liangye", "root-early", "model", 600_000)
	second := reserveExplorationAt(t, s, cfg, later, "awei", "root-late", "model", 1_200_000)
	if err := s.settleCall(first, 600_000, responseUsage{}, "settled"); err != nil {
		t.Fatal(err)
	}
	if err := s.settleCall(second, 1_200_000, responseUsage{}, "settled"); err != nil {
		t.Fatal(err)
	}
	observe := later.Add(5 * time.Hour)
	_, longCharges, err := autonomousWindowCharges(s.db, observe, 8*time.Hour)
	_, shortCharges, shortErr := autonomousWindowCharges(s.db, observe, 4*time.Hour)
	if err != nil || shortErr != nil || !chargeHas(longCharges, 1_200_000) || chargeHas(shortCharges, 1_200_000) {
		t.Fatalf("5 小时前的账应留在 8 小时窗口、离开 4 小时窗口: %+v %+v %v %v", longCharges, shortCharges, err, shortErr)
	}
	spent, _, err := autonomousWindowCharges(s.db, morning, cfg.explorationWindow())
	if err != nil || spent != 1_200_000 {
		t.Fatalf("满 8 小时的旧账应滑出，较新的一笔仍占窗口，实际 %d %v", spent, err)
	}
	weekSpent, err := autonomousWeekSpend(s.db, morning)
	if err != nil || weekSpent != 0 {
		t.Fatalf("新的一周不应带着上周的探索账，实际 %d %v", weekSpent, err)
	}
	ctx := context.WithValue(context.Background(), autonomyKey{}, "root-monday")
	if _, err = s.reserveBudget(ctx, cfg, 900_000, morning, &usageCall{purpose: "model", member: "liangye", root: "root-monday"}); err == nil || !strings.Contains(err.Error(), "最近8小时") {
		t.Fatalf("窗口里的 $1.2 应挡住 $0.9，而不是因为换周放行: %v", err)
	}
	if _, err = s.reserveBudget(context.Background(), cfg, 900_000, morning, &usageCall{purpose: "research", member: "user-task", root: "task"}); err != nil {
		t.Fatalf("真人任务不应被探索窗口挡住: %v", err)
	}
	after := morning.Add(2*time.Hour + time.Minute)
	spent, _, err = autonomousWindowCharges(s.db, after, cfg.explorationWindow())
	if err != nil || spent != 0 {
		t.Fatalf("第二笔满 8 小时后窗口应清空，实际 %d %v", spent, err)
	}
	recovered := reserveExplorationAt(t, s, cfg, after, "liangye", "root-monday", "model", 900_000)
	if err = s.settleCall(recovered, 900_000, responseUsage{}, "settled"); err != nil {
		t.Fatal(err)
	}
	weekSpent, err = autonomousWeekSpend(s.db, after)
	sundaySpent, sundayErr := autonomousWeekSpend(s.db, earlier)
	if err != nil || sundayErr != nil || weekSpent != 900_000 || sundaySpent != 1_800_000 {
		t.Fatalf("两周的探索账应分开，窗口清空不等于上周消失: 周一 %d 周日 %d %v %v", weekSpent, sundaySpent, err, sundayErr)
	}
}

func chargeHas(charges []windowCharge, amount int64) bool {
	for _, charge := range charges {
		if charge.amount == amount {
			return true
		}
	}
	return false
}

func TestConcurrentExplorationCannotShareTheSameWindowBalance(t *testing.T) {
	s, _, _ := fixture(t)
	cfg := explorationConfig(20_000_000)
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, member := range []string{"liangye", "awei"} {
		wg.Add(1)
		go func(i int, member string) {
			defer wg.Done()
			<-start
			ctx := context.WithValue(context.Background(), autonomyKey{}, "root-"+member)
			_, errs[i] = s.reserveCall(withUsageCall(ctx, usageCall{purpose: "model", member: member, root: "root-" + member}), cfg, 1_500_000, "Gemini", gemini.Hydrogen, "model")
		}(i, member)
	}
	close(start)
	wg.Wait()
	var failed, passed int
	for _, err := range errs {
		if err == nil {
			passed++
			continue
		}
		failed++
		if !errors.Is(err, ErrBudget) || !strings.Contains(err.Error(), "最近8小时") {
			t.Fatalf("并发超额应被窗口拒绝: %v", err)
		}
	}
	if passed != 1 || failed != 1 {
		t.Fatalf("两笔 $1.5 只能有一笔进入 $2 窗口: %+v", errs)
	}
	spent, _, err := autonomousWindowCharges(s.db, time.Now(), cfg.explorationWindow())
	if err != nil || spent != 1_500_000 {
		t.Fatalf("窗口里应只留下获准的一笔: %d %v", spent, err)
	}
	week, err := autonomousWeekSpend(s.db, time.Now())
	if err != nil || week != 1_500_000 {
		t.Fatalf("探索周账也应只记获准的一笔: %d %v", week, err)
	}
}

func TestNamedResearchWithoutWorkOriginDoesNotUseExploration(t *testing.T) {
	s, _, _ := fixture(t)
	cfg := explorationConfig(20_000_000)
	reserveExplorationAt(t, s, cfg, time.Now(), "liangye", "root-explore", "model", 2_000_000)
	ctx := withUsageCall(context.Background(), usageCall{purpose: "research", member: "assigned", root: "task"})
	if _, err := s.reserveCall(ctx, cfg, 100_000, "OpenAI", "gpt-6.1-terra", "research"); err != nil {
		t.Fatalf("名为 research 的真人任务不应占用探索额度: %v", err)
	}
	if _, err := s.reserveCall(context.WithValue(ctx, autonomyKey{}, "root-explore"), cfg, 1, "OpenAI", "gpt-6.1-terra", "research"); err == nil || !strings.Contains(err.Error(), "最近8小时") {
		t.Fatalf("同一用途在自主来源下才受窗口限制: %v", err)
	}
	var autonomous int
	if err := s.db.QueryRow(`SELECT autonomous FROM usage_calls WHERE purpose='research' AND member_id='assigned'`).Scan(&autonomous); err != nil || autonomous != 0 {
		t.Fatalf("任务调用不应标成自主探索: %d %v", autonomous, err)
	}
}

func TestExplorationSettlementReleasesAndUnknownKeepsTheReservation(t *testing.T) {
	s, _, _ := fixture(t)
	cfg := explorationConfig(20_000_000)
	price, err := gemini.PriceForModel(gemini.Hydrogen, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reserved := price.ReserveMicros(20320, 65536)
	if reserved != 522_000 {
		t.Fatalf("旧预留规则下，20320 输入与 65536 输出应为 $0.522，实际 %d", reserved)
	}
	settledCharge := int64(35_000)
	capped := price.ReserveMicros(20320, autonomousHydrogenOutputTokens)
	if 3*reserved > 2_000_000 || 4*reserved <= 2_000_000 || 5*capped > 2_000_000 {
		t.Fatalf("旧预留第 4 笔应超过 $2，8192 的前 5 笔应放得下: old=%d capped=%d", reserved, capped)
	}
	var held []usageReservation
	for i := range 3 {
		held = append(held, reserveExplorationAt(t, s, cfg, time.Now(), "liangye", "root-history", "model", reserved))
		if i == 0 {
			continue
		}
	}
	ctx := context.WithValue(context.Background(), autonomyKey{}, "root-history")
	if _, err = s.reserveBudget(ctx, cfg, reserved, time.Now(), &usageCall{purpose: "model", member: "liangye", root: "root-history"}); err == nil || !strings.Contains(err.Error(), "最近8小时") {
		t.Fatalf("$2 窗口应在第 4 笔未释放的旧预留前挡住，实际 %v", err)
	}
	if err = s.settleCall(held[0], settledCharge, responseUsage{InputTokens: 20320, OutputTokens: 400}, "generated_message"); err != nil {
		t.Fatal(err)
	}
	spent, _, err := autonomousWindowCharges(s.db, time.Now(), cfg.explorationWindow())
	if err != nil || spent != settledCharge+2*reserved {
		t.Fatalf("结算后窗口应留下实际 $0.035，而不是继续占着 $0.522: %d %v", spent, err)
	}
	start, end := budgetWeek(time.Now())
	report, err := s.readUsageReport(context.Background(), start, end, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var sawSettled bool
	for _, entry := range report.Entries {
		if entry.ID != held[0].id {
			continue
		}
		sawSettled = true
		if entry.Status != "settled" || entry.ReservedMicros != 0 || entry.CostMicros != settledCharge {
			t.Fatalf("已结算的账单不能把预留显示成花费: %+v", entry)
		}
	}
	if !sawSettled {
		t.Fatal("账单里没有这笔已结算的调用")
	}
	unknown := held[1]
	s.finishUsageCall(unknown)
	spent, _, err = autonomousWindowCharges(s.db, time.Now(), cfg.explorationWindow())
	if err != nil || spent != settledCharge+2*reserved {
		t.Fatalf("结果未知的调用应继续占用预留: %d %v", spent, err)
	}

	fresh, _, _ := fixture(t)
	for range 5 {
		reserveExplorationAt(t, fresh, cfg, time.Now(), "liangye", "root-capped", "model", capped)
	}
	spent, _, err = autonomousWindowCharges(fresh.db, time.Now(), cfg.explorationWindow())
	if err != nil || spent != 5*capped || spent > 2_000_000 {
		t.Fatalf("8192 的前 5 笔预留应一起放进 $2 窗口: %d %v", spent, err)
	}
}

func TestExplorationRecoveryWaitsUntilBothLimitsCanPass(t *testing.T) {
	s, _, _ := fixture(t)
	cfg := explorationConfig(20_000_000)
	cfg.ExplorationShare = 0.025
	cfg.ExplorationWindowShare = 1
	monday := time.Date(2026, time.November, 9, 0, 0, 0, 0, time.Local)
	for monday.Weekday() != time.Monday {
		monday = monday.AddDate(0, 0, 1)
	}
	_, weekEnd := budgetWeek(monday.Add(-time.Hour))
	reserveExplorationAt(t, s, cfg, monday.Add(-4*time.Hour), "liangye", "root-both", "model", 500_000)
	ctx := context.WithValue(context.Background(), autonomyKey{}, "root-next")
	_, err := s.reserveBudget(ctx, cfg, 100_000, monday.Add(-time.Hour), &usageCall{purpose: "model", member: "awei", root: "root-next"})
	var rejection *budgetRejection
	if !errors.As(err, &rejection) || !rejection.resumeAt.Equal(wakeAfter(monday.Add(-4*time.Hour).Add(8*time.Hour))) || rejection.resumeAt.Before(weekEnd) {
		t.Fatalf("周额度和窗口都不足时，应等到较晚的那个时刻: %v，周恢复 %s", err, weekEnd)
	}
	afternoon := monday.Add(15 * time.Hour)
	reserveExplorationAt(t, s, cfg, afternoon.Add(-9*time.Hour), "liangye", "root-week", "model", 500_000)
	_, weekEnd = budgetWeek(afternoon)
	_, err = s.reserveBudget(ctx, cfg, 100_000, afternoon, &usageCall{purpose: "model", member: "awei", root: "root-week"})
	if !errors.As(err, &rejection) || rejection.skipWindowRetry || !rejection.resumeAt.Equal(weekEnd) || !strings.Contains(err.Error(), "本周自主探索额度") {
		t.Fatalf("只有探索周额度不足时应等到下周: %v", err)
	}
}

func TestExplorationShortfallWaitsUntilItCanFitAndDoesNotSpin(t *testing.T) {
	s, c, m := fixture(t)
	cfg := explorationConfig(20_000_000)
	at := time.Now()
	blocking := reserveExplorationAt(t, s, cfg, at.Add(-time.Hour), "liangye", "root-block", "model", 2_000_000)
	ctx := context.WithValue(context.Background(), autonomyKey{}, "root-next")
	_, err := s.reserveBudget(ctx, cfg, 100_000, at, &usageCall{purpose: "model", member: "awei", root: "root-next"})
	var rejection *budgetRejection
	slide := wakeAfter(blocking.idTime(t, s).Add(cfg.explorationWindow()))
	if !errors.As(err, &rejection) || rejection.skipWindowRetry || !rejection.resumeAt.Equal(slide) {
		t.Fatalf("窗口不足应等到挡住它的那笔滑出: %v，期望 %s", err, slide)
	}
	human := Work{ID: newID("work"), ConversationID: c.ID, MemberID: m.ID, Kind: "chat", SourceID: "msg-user", Status: "waiting_budget", BudgetRequired: 100_000, DueAt: at.Add(time.Hour).UnixMilli(), CreatedAt: now()}
	explore := Work{ID: newID("work"), MemberID: m.ID, Kind: "explore", Autonomous: true, Status: "waiting_budget", BudgetRequired: 100_000, DueAt: at.Add(time.Hour).UnixMilli(), CreatedAt: now()}
	impossible := Work{ID: newID("work"), MemberID: m.ID, Kind: "explore", Autonomous: true, Status: "waiting_budget", BudgetRequired: 3_000_000, DueAt: at.UnixMilli(), CreatedAt: now()}
	for _, w := range []Work{human, explore, impossible} {
		if err = insertWaiting(s, w); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.recheckBudgetWait(cfg); err != nil {
		t.Fatal(err)
	}
	if status := workStatus(t, s, human.ID); status != "pending" {
		t.Fatalf("探索窗口用尽时，真人等待仍应按站点余额恢复: %s", status)
	}
	if status := workStatus(t, s, explore.ID); status != "waiting_budget" {
		t.Fatalf("窗口仍放不下时不应把探索唤醒: %s", status)
	}
	if status := workStatus(t, s, impossible.ID); status != "waiting_budget" {
		t.Fatalf("空窗口也放不下的请求不能靠滑出旧账恢复: %s", status)
	}
	past := at.Add(-9 * time.Hour).Format(time.RFC3339Nano)
	if _, err = s.db.Exec(`UPDATE usage_calls SET started_at=? WHERE id=?`, past, blocking.id); err != nil {
		t.Fatal(err)
	}
	if err = s.recheckBudgetWait(cfg); err != nil {
		t.Fatal(err)
	}
	if status := workStatus(t, s, explore.ID); status != "pending" {
		t.Fatalf("旧账滑出后应给探索一次重新判断的机会: %s", status)
	}
	if status := workStatus(t, s, impossible.ID); status != "waiting_budget" {
		t.Fatalf("超过整个窗口的请求在额度变大前不能恢复: %s", status)
	}
	if err = s.setWeeklyBudget(100_000_000); err != nil {
		t.Fatal(err)
	}
	if err = s.recheckBudgetWait(cfg); err != nil {
		t.Fatal(err)
	}
	if status := workStatus(t, s, impossible.ID); status != "pending" {
		t.Fatalf("周额度提高到 $100 后，$3 的预留放得进 $10 窗口: %s", status)
	}
}

func insertWaiting(s *Store, w Work) error {
	if err := s.Guard(w.ConversationID, revisionOf(s, w.ConversationID), func(tx *sql.Tx) error {
		return insertWork(tx, w)
	}); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE work SET budget_required=? WHERE id=?`, w.BudgetRequired, w.ID)
	return err
}

func revisionOf(s *Store, room string) int64 {
	if room == "" {
		return 0
	}
	var revision int64
	_ = s.db.QueryRow(`SELECT revision FROM conversations WHERE id=?`, room).Scan(&revision)
	return revision
}

func workStatus(t *testing.T, s *Store, id string) string {
	t.Helper()
	var status string
	if err := s.db.QueryRow(`SELECT status FROM work WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func (r usageReservation) idTime(t *testing.T, s *Store) time.Time {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(`SELECT started_at FROM usage_calls WHERE id=?`, r.id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestScheduleKeepsTheWorkOrigin(t *testing.T) {
	s, c, m := fixture(t)
	e := NewEngine(s, nil, explorationConfig(20_000_000))
	revisit := Work{ID: newID("work"), ConversationID: c.ID, MemberID: m.ID, Kind: "revisit", SourceID: "human-msg", Autonomous: true, Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
	chat := Work{ID: newID("work"), ConversationID: c.ID, MemberID: m.ID, Kind: "chat", SourceID: "human-msg", Autonomous: false, Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
	for _, w := range []Work{revisit, chat} {
		if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error { return insertWork(tx, w) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Tools.Schedule(context.Background(), c, m, revisit, "继续看", "自己的安排", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Tools.Schedule(context.Background(), c, m, chat, "用户交代的研究", "用户要求", time.Now()); err != nil {
		t.Fatal(err)
	}
	var revisitChild, chatChild bool
	if err := s.db.QueryRow(`SELECT autonomous FROM work WHERE parent_id=?`, revisit.ID).Scan(&revisitChild); err != nil || !revisitChild {
		t.Fatalf("自主回访的续接应保持自主来源: %v %v", revisitChild, err)
	}
	if err := s.db.QueryRow(`SELECT autonomous FROM work WHERE parent_id=?`, chat.ID).Scan(&chatChild); err != nil || chatChild {
		t.Fatalf("真人聊天安排的研究不能因为叫做 research 变成探索: %v %v", chatChild, err)
	}
}

func TestExplorationOriginStaysOnContinuationsAndToolCalls(t *testing.T) {
	s, c, m := fixture(t)
	var searches atomic.Int32
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["query"] == "" {
			t.Errorf("搜索请求不完整: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"results":[{"url":"https://example.test/%d","summary":"一条记录"}]}`, searches.Load())
	}))
	defer search.Close()
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		text := `{"outcome":"看过了"}`
		if !strings.Contains(string(raw), "function_call_output") {
			query := fmt.Sprintf(`{"query":"origin-%d"}`, n)
			fmt.Fprintf(w, `{"status":"completed","output":[{"type":"function_call","call_id":"call_%d","name":"search","arguments":%s}],"usage":{"input_tokens":20,"output_tokens":8}}`, n, asJSON(query))
			return
		}
		fmt.Fprintf(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}],"usage":{"input_tokens":20,"output_tokens":8}}`, asJSON(text))
	}))
	defer model.Close()
	cfg := quietBudget(explorationConfig(20_000_000), model.URL, search.URL)
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: model.Client()}, cfg)
	explore := Work{ID: newID("work"), MemberID: m.ID, Kind: "explore", Autonomous: true, Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
	if err := s.Guard("", 0, func(tx *sql.Tx) error { return insertWork(tx, explore) }); err != nil {
		t.Fatal(err)
	}
	if err := e.process(context.Background(), explore); err != nil {
		t.Fatalf("自主探索: %v", err)
	}
	clearActionMemory(t, s)
	current, err := s.Conversation(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Tools.Schedule(context.Background(), current, m, explore, "接着查", "自己的安排", time.Now()); err != nil {
		t.Fatal(err)
	}
	child := childWork(t, s, explore.ID)
	if !child.Autonomous || child.Kind != "research" || child.RootID != explore.ID {
		t.Fatalf("探索的续接应沿用来源: %+v", child)
	}
	if err = e.process(context.Background(), child); err != nil {
		t.Fatalf("探索续接: %v", err)
	}
	clearActionMemory(t, s)
	beforeChat := usageRows(t, s)
	msg := userMessage(c, "帮我查一下这张旧照片")
	if err = s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	chat := latestChatWork(t, s, c.ID)
	if autonomousWork(chat) || chat.SourceID != msg.ID {
		t.Fatalf("真人消息不应是自主探索: %+v", chat)
	}
	if err = e.process(context.Background(), chat); err != nil {
		t.Fatalf("真人聊天: %v", err)
	}
	clearActionMemory(t, s)
	current, _ = s.Conversation(c.ID)
	if _, err = e.Tools.Schedule(context.Background(), current, m, chat, "用户交代的研究", "用户要求", time.Now()); err != nil {
		t.Fatal(err)
	}
	assigned := childWork(t, s, chat.ID)
	if assigned.Autonomous || assigned.Kind != "research" || assigned.SourceID != msg.ID {
		t.Fatalf("用户交代的续接不能标成探索: %+v", assigned)
	}
	if err = e.process(context.Background(), assigned); err != nil {
		t.Fatalf("用户交代的研究: %v", err)
	}
	rows := usageRows(t, s)
	if searches.Load() != 4 {
		t.Fatalf("四段工作都应真的发起搜索: %d", searches.Load())
	}
	var exploreCalls, chatCalls int
	for _, row := range rows {
		switch row.root {
		case explore.ID:
			exploreCalls++
			if !row.autonomous {
				t.Fatalf("探索及其续接、工具调用都应计入自主来源: %+v", row)
			}
		case chat.ID:
			chatCalls++
			if row.autonomous {
				t.Fatalf("真人消息和它安排的研究不应计入探索: %+v", row)
			}
		default:
			t.Fatalf("出现了意外的账本根: %+v", row)
		}
	}
	if exploreCalls == 0 || chatCalls == 0 || len(rows) <= len(beforeChat) {
		t.Fatalf("两边都应该有模型或搜索账: explore=%d chat=%d", exploreCalls, chatCalls)
	}
	var researchNamed int
	for _, row := range rows {
		if row.root == chat.ID && row.purpose == "research" && !row.autonomous {
			researchNamed++
		}
	}
	if researchNamed == 0 {
		t.Fatalf("用户交代的研究在账上仍叫 research，但不能算探索: %+v", rows)
	}
}

type usageProbe struct {
	purpose    string
	root       string
	autonomous bool
}

func usageRows(t *testing.T, s *Store) []usageProbe {
	t.Helper()
	rows, err := s.db.Query(`SELECT purpose, root_id, autonomous FROM usage_calls ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var list []usageProbe
	for rows.Next() {
		var row usageProbe
		var autonomous int
		if err = rows.Scan(&row.purpose, &row.root, &autonomous); err != nil {
			t.Fatal(err)
		}
		row.autonomous = autonomous == 1
		list = append(list, row)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return list
}

func clearActionMemory(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`DELETE FROM action_checkpoints`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM working_contexts`); err != nil {
		t.Fatal(err)
	}
}

func childWork(t *testing.T, s *Store, parent string) Work {
	t.Helper()
	w, err := scanWork(s.db.QueryRow(`SELECT `+workColumns+` FROM work WHERE parent_id=?`, parent))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func quietBudget(cfg Config, modelURL, searchURL string) Config {
	off := false
	cfg.SearchURL = searchURL
	cfg.setModelEndpoint(modelURL)
	cfg.editProviders(func(p *modelProvider) {
		p.CacheBreakpoints = &off
		p.MidTurnSteering = &off
	})
	cfg.editScenes([]modelScene{sceneChat, sceneResearch, sceneExploration, sceneMemberSettings}, func(s *modelSettings) {
		s.ContextTokenBudget = 1_000_000
	})
	return cfg
}

func TestHydrogenCapIsOnlyOnAutonomousExploration(t *testing.T) {
	s, _, _ := fixture(t)
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":generateContent") {
			t.Errorf("意外请求 %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		fmt.Fprint(w, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"{\"outcome\":\"记下了\"}"}]}}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":8,"thoughtsTokenCount":0,"totalTokenCount":28}}`)
	}))
	defer server.Close()
	cfg := explorationConfig(20_000_000)
	cfg.ExplorationShare = 1
	cfg.ExplorationWindowShare = 1
	shared := map[string]any{}
	cfg.Models.Providers["Gemini"] = modelProvider{Protocol: gemini.Protocol, BaseURL: server.URL, apiKey: "test-only"}
	cfg.Models.Scenes[sceneChat] = modelSettings{Provider: "Gemini", Model: gemini.Hydrogen, ContextTokenBudget: 200_000, maxOutputTokens: 65536, Options: shared}
	client := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	ordinary := context.WithValue(context.Background(), modelSceneKey{}, sceneChat)
	autonomous := context.WithValue(ordinary, autonomyKey{}, "explore-root")
	price, err := gemini.PriceForModel(gemini.Hydrogen, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	complete := func(ctx context.Context) {
		t.Helper()
		if _, err := client.Complete(ctx, []ChatMessage{{Role: "user", Content: "看看这张旧照片"}}, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	complete(autonomous)
	complete(ordinary)
	if _, leaked := shared["generationConfig"]; leaked || len(bodies) != 2 {
		t.Fatalf("自主调用不能改写普通场景的共享参数: leaked=%v bodies=%d", leaked, len(bodies))
	}
	assertOutputCap(t, s, price, bodies[0], 0, autonomousHydrogenOutputTokens, true)
	assertOutputCap(t, s, price, bodies[1], 1, 65536, false)
	if strings.Contains(string(bodies[1]), "8192") {
		t.Fatal("普通聊天的请求体被收成了 8192")
	}

	explicit := map[string]any{"generationConfig": map[string]any{"maxOutputTokens": 2048}}
	cfg.Models.Scenes[sceneChat] = modelSettings{Provider: "Gemini", Model: gemini.Hydrogen, ContextTokenBudget: 200_000, maxOutputTokens: 2048, Options: explicit}
	client = &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	complete(autonomous)
	complete(ordinary)
	assertOutputCap(t, s, price, bodies[2], 2, 2048, true)
	assertOutputCap(t, s, price, bodies[3], 3, 2048, false)

	full := map[string]any{"generationConfig": map[string]any{"maxOutputTokens": 65536}}
	cfg.Models.Scenes[sceneChat] = modelSettings{Provider: "Gemini", Model: gemini.Hydrogen, ContextTokenBudget: 200_000, maxOutputTokens: 65536, Options: full}
	client = &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	complete(autonomous)
	assertOutputCap(t, s, price, bodies[4], 4, 65536, true)
}

func assertOutputCap(t *testing.T, s *Store, price gemini.Price, body []byte, index, limit int, autonomous bool) {
	t.Helper()
	var decoded struct {
		GenerationConfig struct {
			MaxOutputTokens int `json:"maxOutputTokens"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if limit == 65536 && !strings.Contains(string(body), `"maxOutputTokens"`) {
		// 省略时供应商默认 65536，请求体不写这个字段。预留必须按同一个 65536。
	} else if decoded.GenerationConfig.MaxOutputTokens != limit {
		t.Fatalf("请求体的输出上限应为 %d，实际 %d，正文 %s", limit, decoded.GenerationConfig.MaxOutputTokens, body)
	}
	var reserved int64
	var auto int
	if err := s.db.QueryRow(`SELECT reserved_micros, autonomous FROM usage_calls ORDER BY id LIMIT 1 OFFSET ?`, index).Scan(&reserved, &auto); err != nil {
		t.Fatal(err)
	}
	want := price.ReserveMicros(len(body), limit)
	if reserved != want || (auto == 1) != autonomous {
		t.Fatalf("预留应使用请求里的输出上限 %d：reserved=%d want=%d autonomous=%d", limit, reserved, want, auto)
	}
}

func TestExplorationRecoveryDoesNotReplayOrBlockHumans(t *testing.T) {
	s, c, m := fixture(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		raw, _ := io.ReadAll(r.Body)
		text := `{"outcome":"先看看"}`
		if strings.Contains(string(raw), `"model":"gpt-6.1-terra"`) && strings.Contains(string(raw), "现在聊一聊") {
			text = `{"message":"我在，接着聊。","outcome":"回了"}`
		}
		fmt.Fprintf(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}],"usage":{"input_tokens":20,"output_tokens":8}}`, asJSON(text))
	}))
	defer server.Close()
	cfg := quietBudget(explorationConfig(20_000_000), server.URL, "")
	cfg.ExplorationWindow = 2 * time.Minute
	cfg.editScenes([]modelScene{sceneExploration}, func(scene *modelSettings) { scene.Model = "gpt-6.1-sol" })
	if _, err := s.db.Exec(`UPDATE personal_activity SET next_at=? WHERE member_id=?`, time.Now().Add(time.Hour).UnixMilli(), m.ID); err != nil {
		t.Fatal(err)
	}
	reserveExplorationAt(t, s, cfg, time.Now(), "other", "root-fill", "model", 2_000_000)
	explore := Work{ID: newID("work"), MemberID: m.ID, Kind: "explore", Autonomous: true, Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
	if err := s.Guard("", 0, func(tx *sql.Tx) error { return insertWork(tx, explore) }); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: server.Client()}, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, 5*time.Second, func() bool { return workStatus(t, s, explore.ID) == "waiting_budget" })
	if requests.Load() != 0 {
		t.Fatal("窗口不足时不应把请求发给模型")
	}
	var checkpoint int
	if err := s.db.QueryRow(`SELECT count(*) FROM action_checkpoints WHERE work_id=?`, explore.ID).Scan(&checkpoint); err != nil || checkpoint != 1 {
		t.Fatalf("额度不足应留下断点: %d %v", checkpoint, err)
	}
	if _, err := s.db.Exec(`UPDATE personal_activity SET next_at=? WHERE member_id=?`, time.Now().Add(-24*time.Hour).UnixMilli(), m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	var explores int
	if err := s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=?`, m.ID).Scan(&explores); err != nil || explores != 1 {
		t.Fatalf("错过的唤醒不能排队补跑: %d %v", explores, err)
	}
	if _, err := s.db.Exec(`UPDATE personal_activity SET next_at=? WHERE member_id=?`, time.Now().Add(time.Hour).UnixMilli(), m.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Submit(userMessage(c, "现在聊一聊")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return requests.Load() == 1 && testMemberSpoke(s, c.ID, m.ID)
	})
	var humanAuto int
	if err := s.db.QueryRow(`SELECT autonomous FROM usage_calls WHERE purpose='chat' ORDER BY id DESC LIMIT 1`).Scan(&humanAuto); err != nil || humanAuto != 0 {
		t.Fatalf("真人消息应走原来的站点预算: %d %v", humanAuto, err)
	}
	past := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`UPDATE usage_calls SET started_at=? WHERE autonomous=1`, past); err != nil {
		t.Fatal(err)
	}
	if err := s.recheckBudgetWait(cfg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return workStatus(t, s, explore.ID) == "done" })
	if requests.Load() != 2 {
		t.Fatalf("恢复后应只再判断一次: %d", requests.Load())
	}
	var replies int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id=? AND author_id=? AND body=?`, c.ID, m.ID, "我在，接着聊。").Scan(&replies); err != nil || replies != 1 {
		t.Fatalf("已经发出的回复不能重发: %d %v", replies, err)
	}
	time.Sleep(1200 * time.Millisecond)
	if requests.Load() != 2 || workStatus(t, s, explore.ID) != "done" {
		t.Fatalf("一次恢复之后不应重放错过的唤醒: requests=%d status=%s", requests.Load(), workStatus(t, s, explore.ID))
	}
}

func TestExplorationRequestThatCannotFitDoesNotSpin(t *testing.T) {
	s, c, m := fixture(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"message\":\"我在，接着聊。\",\"outcome\":\"回了\"}"}]}],"usage":{"input_tokens":20,"output_tokens":8}}`)
	}))
	defer server.Close()
	cfg := quietBudget(explorationConfig(20_000_000), server.URL, "")
	cfg.ExplorationWindowShare = 0.0001
	if _, err := s.db.Exec(`UPDATE personal_activity SET next_at=? WHERE member_id=?`, time.Now().Add(time.Hour).UnixMilli(), m.ID); err != nil {
		t.Fatal(err)
	}
	explore := Work{ID: newID("work"), MemberID: m.ID, Kind: "explore", Autonomous: true, Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
	if err := s.Guard("", 0, func(tx *sql.Tx) error { return insertWork(tx, explore) }); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: server.Client()}, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, 5*time.Second, func() bool { return workStatus(t, s, explore.ID) == "waiting_budget" })
	var due int64
	var result string
	if err := s.db.QueryRow(`SELECT due_at, result FROM work WHERE id=?`, explore.ID).Scan(&due, &result); err != nil {
		t.Fatal(err)
	}
	if time.UnixMilli(due).Year() != 9999 || !strings.Contains(result, "超过每8小时") || requests.Load() != 0 {
		t.Fatalf("空窗口也放不下时不应按窗口反复重试: due=%s result=%s requests=%d", time.UnixMilli(due), result, requests.Load())
	}
	if err := e.Submit(userMessage(c, "现在聊一聊")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return testMemberSpoke(s, c.ID, m.ID) })
	time.Sleep(1200 * time.Millisecond)
	if requests.Load() != 1 || workStatus(t, s, explore.ID) != "waiting_budget" {
		t.Fatalf("探索停住时真人聊天仍应完成，且探索不能空转: requests=%d status=%s", requests.Load(), workStatus(t, s, explore.ID))
	}
}

func waitFor(t *testing.T, limit time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待超时")
}

func TestBudgetRecoveryLetsMemberReconsiderUnsentSearch(t *testing.T) {
	for _, mode := range []string{"继续", "暂不需要", "停止研究"} {
		t.Run(mode, func(t *testing.T) {
			s, c, m := fixture(t)
			var searches, modelCalls atomic.Int32
			search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				searches.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.URL.Path != "/search" || body["query"] != "蓝色自行车" {
					t.Errorf("恢复后搜索实际请求不对: path=%s body=%v err=%v", r.URL.Path, body, err)
				}
				fmt.Fprint(w, `{"results":[{"url":"https://example.test/bike","summary":"检索结果已找到"}]}`)
			}))
			defer search.Close()
			cfg := quietBudget(explorationConfig(20_000_000), search.URL, search.URL)
			reserveExplorationAt(t, s, cfg, time.Now(), "other", "fill", "model", 2_000_000)
			if _, err := s.db.Exec(`UPDATE personal_activity SET next_at=-1`); err != nil {
				t.Fatal(err)
			}
			work := addWork(t, s, c, m, "research")
			send := toolCall("send_message", map[string]any{"message": "我先记下这件事"})
			oldSearch := toolCall("search", map[string]any{"query": "蓝色自行车"})
			newSearch := toolCall("search", map[string]any{"query": "蓝色自行车"})
			e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
				if autonomyRoot(ctx) != work.ID {
					return jsonReply(Outcome{}), nil
				}
				switch n := modelCalls.Add(1); n {
				case 1:
					return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{send, oldSearch}}, nil
				case 2:
					var sentReceipt, unsentReceipt, newMessage bool
					var scene string
					for _, msg := range ms {
						if msg.Role == "tool" && msg.ToolCallID == send.ID {
							sentReceipt = strings.Contains(msg.Content, "succeeded")
						}
						if msg.Role == "tool" && msg.ToolCallID == oldSearch.ID {
							unsentReceipt = strings.Contains(msg.Content, "not_sent") && strings.Contains(msg.Content, "上次额度不足")
						}
						newMessage = newMessage || (msg.Role == "user" && strings.Contains(msg.Content, "那辆车我已找到，暂不需要"))
						if sceneFields(msg) != nil {
							scene = msg.Content
						}
					}
					if !sentReceipt || !unsentReceipt || scene == "" {
						t.Errorf("没有把成功回执、历史未发送回执和最新现场交给模型: 成功=%v 未发=%v 现场=%v", sentReceipt, unsentReceipt, scene != "")
					}
					if mode == "暂不需要" {
						if !newMessage {
							t.Error("没有交付真人新消息")
						}
						return jsonReply(Outcome{Outcome: "不再搜索"}), nil
					}
					if mode != "继续" {
						t.Errorf("已取消的工作再次请求模型")
					}
					return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{newSearch}}, nil
				default:
					found := false
					for _, msg := range ms {
						found = found || (msg.Role == "tool" && msg.ToolCallID == newSearch.ID && strings.Contains(msg.Content, "检索结果已找到"))
					}
					if !found {
						t.Error("新搜索结果未返回给模型")
					}
					return jsonReply(Outcome{Outcome: "已核对"}), nil
				}
			}), cfg)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { e.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			waitFor(t, 5*time.Second, func() bool { return workStatus(t, s, work.ID) == "waiting_budget" })
			if searches.Load() != 0 || modelCalls.Load() != 1 {
				t.Fatalf("受阻的搜索不可发送: 搜索=%d 模型=%d", searches.Load(), modelCalls.Load())
			}
			switch mode {
			case "暂不需要":
				if err := e.Submit(userMessage(c, "那辆车我已找到，暂不需要")); err != nil {
					t.Fatal(err)
				}
			case "停止研究":
				if err := e.Submit(userMessage(c, "停止研究")); err != nil {
					t.Fatal(err)
				}
				if status := workStatus(t, s, work.ID); status != "stopped" {
					t.Fatalf("明确取消优先: %s", status)
				}
			}
			past := time.Now().Add(-9 * time.Hour).Format(time.RFC3339Nano)
			if _, err := s.db.Exec(`UPDATE usage_calls SET started_at=? WHERE root_id='fill'`, past); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantSearches, wantModels := "done", int32(1), int32(3)
			if mode == "暂不需要" {
				wantSearches, wantModels = 0, 2
			} else if mode == "停止研究" {
				wantStatus, wantSearches, wantModels = "stopped", 0, 1
				if err := s.recheckBudgetWait(cfg); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, 5*time.Second, func() bool { return workStatus(t, s, work.ID) == wantStatus })
			if searches.Load() != wantSearches || modelCalls.Load() != wantModels {
				t.Fatalf("恢复实际调用: 搜索=%d 想要 %d，模型=%d 想要 %d", searches.Load(), wantSearches, modelCalls.Load(), wantModels)
			}
			var sends int
			if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id=? AND body='我先记下这件事'`, c.ID).Scan(&sends); err != nil || sends != 1 {
				t.Fatalf("不得重复发送已成功消息: %d %v", sends, err)
			}
		})
	}
}

func TestBudgetRestartKeepsUnaffordableWorkParked(t *testing.T) {
	s, c, m := fixture(t)
	cfg := explorationConfig(20_000_000)
	reserveExplorationAt(t, s, cfg, time.Now(), "other", "fill", "model", 2_000_000)
	if _, err := s.db.Exec(`UPDATE personal_activity SET next_at=-1`); err != nil {
		t.Fatal(err)
	}
	job, oversized := addWork(t, s, c, m, "research"), addWork(t, s, c, m, "research")
	for _, item := range []struct {
		work Work
		cost int64
		due  time.Time
	}{{job, 1_000_000, time.Now().Add(8 * time.Hour)}, {oversized, 2_000_001, explorationHoldDue()}} {
		if _, err := s.Claim(item.work.ID); err != nil {
			t.Fatal(err)
		}
		item.work.BudgetRequired = item.cost
		if err := s.Finish(item.work, "waiting_budget", "预算不足", item.due); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	e := NewEngine(s, modelFunc(func(ctx context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls.Add(1)
		return jsonReply(Outcome{Outcome: "现在可以继续"}), nil
	}), cfg)
	start := func() func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		stop := func() { cancel(); <-done }
		go func() { e.Run(ctx); close(done) }()
		t.Cleanup(stop)
		return stop
	}
	stop := start()
	// 等一个完整调度周期，确认普通等待和永久占位都没有被启动改写。
	time.Sleep(1100 * time.Millisecond)
	stop()
	for _, w := range []Work{job, oversized} {
		got, err := s.ReadWork(c.ID, w.ID)
		if err != nil || got.Status != "waiting_budget" || got.Attempts != 1 || calls.Load() != 0 {
			t.Fatalf("相同配置重启不应重领: %+v calls=%d err=%v", got, calls.Load(), err)
		}
	}
	past := time.Now().Add(-9 * time.Hour).Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`UPDATE usage_calls SET started_at=? WHERE root_id='fill'`, past); err != nil {
		t.Fatal(err)
	}
	start()
	waitFor(t, 5*time.Second, func() bool { return workStatus(t, s, job.ID) == "done" })
	got, err := s.ReadWork(c.ID, oversized.ID)
	if err != nil || got.Status != "waiting_budget" || got.Attempts != 1 || calls.Load() != 1 {
		t.Fatalf("只应唤醒已经付得起的工作: %+v calls=%d err=%v", got, calls.Load(), err)
	}
}

func TestHumanDelegationAndTimedFollowupUseSiteBudget(t *testing.T) {
	s, c, members := groupWithMembers(t, 2)
	cfg := explorationConfig(20_000_000)
	reserveExplorationAt(t, s, cfg, time.Now(), "other", "fill", "model", 2_000_000)
	msg := userMessage(c, "@阿苇 请让同伴核对材料，收齐就反馈，并约定一小时后再看")
	if err := s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	c, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := latestChatWork(t, s, c.ID)
	calls := 0
	e := NewEngine(s, modelFunc(func(ctx context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		reservation, err := s.reserveCall(ctx, cfg, 1000, "test", "fake", "")
		if err != nil {
			return ChatMessage{}, err
		}
		if err := s.settleCall(reservation, 500, responseUsage{}, ""); err != nil {
			return ChatMessage{}, err
		}
		return jsonReply(Outcome{Outcome: "材料核对完成"}), nil
	}), cfg)
	assigned, err := s.DelegateWork(c, members[0], parent, members[1].ID, "核对材料", "按真人要求安排")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Claim(assigned.ID); err != nil || !ok {
		t.Fatalf("领取真人委托: %v %v", ok, err)
	}
	if err := e.process(context.Background(), assigned); err != nil {
		t.Fatalf("真人委托不应被自主窗口挡住: %v", err)
	}
	review, err := s.ReadWork(c.ID, "coordination_"+assigned.ID)
	if err != nil || autonomousWork(review) || review.RootID != parent.ID || review.SourceID != msg.ID {
		t.Fatalf("结果回报应继承真人来源: %+v %v", review, err)
	}
	if err := e.process(context.Background(), review); err != nil {
		t.Fatalf("真人结果回报不应被自主窗口挡住: %v", err)
	}
	due := time.Now().Add(time.Hour).Truncate(time.Second)
	follow := toolCall("schedule_followup", map[string]any{"instruction": "核对稍后有无变化", "due_at": due.Format(time.RFC3339)})
	if _, err := e.Tools.Execute(context.Background(), c, members[0], review, follow); err != nil {
		t.Fatalf("保存真人约定的明确时刻: %v", err)
	}
	timed := childWork(t, s, review.ID)
	if autonomousWork(timed) || timed.DueAt != due.UnixMilli() || timed.RootID != parent.ID || timed.SourceID != msg.ID {
		t.Fatalf("明确预约应沿用原任务的来源和指定时间: %+v", timed)
	}
	if err := e.process(context.Background(), timed); err != nil {
		t.Fatalf("真人预约到期时不应被自主窗口挡住: %v", err)
	}
	if calls != 3 {
		t.Fatalf("应执行委托、回报、到期复看，得到 %d 次", calls)
	}
	for _, row := range usageRows(t, s) {
		if row.root == parent.ID && row.autonomous {
			t.Fatalf("真人链误进自主账: %+v", row)
		}
	}
}
