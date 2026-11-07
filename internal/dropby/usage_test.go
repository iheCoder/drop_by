package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageReportKeepsHistoryAndReconcilesPerRequest(t *testing.T) {
	// 同一周既有升级前的汇总，又有不同成员、用途和处理状态的新调用。
	s, room, m := fixture(t)
	cfg := testConfig()
	ctx := context.Background()
	week, _ := budgetWeek(time.Now())
	at := week.Add(9 * time.Hour)
	oldDay, err := s.reserveContextAt(ctx, cfg, 200, at)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(oldDay, 200, 120, 10, 1); err != nil {
		t.Fatal(err)
	}
	work := addWork(t, s, room, m, "research")
	call := workUsage("compact", m.ID, room, work)
	call.provider, call.model = "OpenAI", cfg.Model
	reserved, err := s.reserveBudget(ctx, cfg, 1000, at.Add(4*time.Hour), &call)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		// 重复回执只结算一次。另一笔金额不能被同日的重复回执误扣。
		if err = s.settleCall(reserved, 350, responseUsage{InputTokens: 100, OutputTokens: 50}, ""); err != nil {
			t.Fatal(err)
		}
	}
	call = workUsage("control", "", room, work)
	call.provider, call.model = "OpenAI", cfg.Model
	pending, err := s.reserveBudget(ctx, cfg, 250, at, &call)
	if err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(NewEngine(s, nil, cfg))
	read := func(period, date string) usageReport {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost/api/usage?period="+period+"&date="+date, nil))
		var report usageReport
		if rec.Code != 200 {
			t.Fatalf("额度接口: %d %s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	weekly := read("week", oldDay)
	if weekly.Start != oldDay || weekly.End != week.AddDate(0, 0, 7).Format(usageDate) || len(weekly.Legacy) != 1 || len(weekly.Entries) != 2 {
		t.Fatalf("周边界或历史汇总不符: %+v", weekly)
	}
	// 周图按完整预算周汇总，周柱同时包含旧账和逐笔账；预留金额单独保留。
	if len(weekly.Trend) != 7 || weekly.Trend[5].Day != week.AddDate(0, 0, -7).Format(usageDate) ||
		weekly.Trend[6] != (dailyUsage{Day: oldDay, Calls: 3, CostMicros: 470, ReservedMicros: 250}) {
		t.Fatalf("每根周柱应对应一周: %+v", weekly.Trend)
	}
	if old := weekly.Legacy[0]; old.CostMicros != 120 || old.Calls != 1 || old.ReservedMicros != 0 {
		t.Fatalf("历史仍应保留在原有日期: %+v", old)
	}
	if u := weekly.Entries[1]; u.Purpose != "compact" || u.CostMicros != 350 || u.MemberID != m.ID || u.MemberName != m.Name || u.RootID != work.ID ||
		u.Conversation != room.Name || u.Status != "settled" || u.InputTokens != 100 || u.StartedAt[11:16] != "13:00" {
		t.Fatalf("压缩应按原请求时刻与所属事项结算: %+v", u)
	}
	if u := weekly.Entries[0]; u.Purpose != "control" || u.MemberID != "" || u.ReservedMicros != 250 || u.Status != "running" {
		t.Fatalf("会话共用判断仍在途: %+v", u)
	}
	s.finishUsageCall(pending)
	daily := read("day", oldDay)
	if daily.End != week.AddDate(0, 0, 1).Format(usageDate) || len(daily.Entries) != 2 || daily.Entries[0].Status != "unknown" ||
		daily.Entries[0].ReservedMicros != 250 || daily.Entries[0].CostMicros != 0 {
		t.Fatalf("未知结果应保留占用，并和已结算区分: %+v", daily)
	}
	// 日图每根柱恰好对应一天，选择同组中的历史日期不会移动其他柱。
	for i, bucket := range daily.Trend {
		want := time.Now().AddDate(0, 0, i-6).Format(usageDate)
		if bucket.Day != want {
			t.Fatalf("每天应恰有一根日柱: %+v", daily.Trend)
		}
		if bucket.Day == oldDay && (bucket.CostMicros != 470 || bucket.Calls != 3) {
			t.Fatalf("日柱应包含历史与逐笔费用: %+v", bucket)
		}
	}
	total, err := s.Usage()
	if err != nil || total.CostMicros != 720 || total.UnsettledMicros != 250 || total.Calls != 3 {
		t.Fatalf("逐笔与旧账合计应等于预算总账: %+v %v", total, err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost/api/usage?period=bad", nil))
	if rec.Code != 400 {
		t.Fatalf("未知周期应被拒绝: %d", rec.Code)
	}
}

func TestUsageCallPreservesReservationDateAndRestartsUnknown(t *testing.T) {
	// 跨周返回的旧调用仍计入开始那周；本周未收到回执的预留在重启后继续占用。
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	cfg := testConfig()
	start, _ := budgetWeek(time.Now())
	ctx := context.Background()
	call := usageCall{purpose: "chat", provider: "OpenAI", model: cfg.Model}
	old, err := s.reserveBudget(ctx, cfg, 800000, start.Add(-time.Minute), &call)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.reserveBudget(ctx, cfg, 400000, start, &call); err != nil {
		t.Fatal(err)
	}
	if _, err = s.reserveBudget(ctx, cfg, 650000, start, &call); !errors.Is(err, ErrBudget) {
		t.Fatalf("请求应受同一周总预算约束: %v", err)
	}
	if err = s.settleCall(old, 150000, responseUsage{InputTokens: 10, OutputTokens: 1}, "no_message"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.readUsageReport(ctx, start.AddDate(0, 0, -7), start, time.Now())
	if err != nil || len(before.Entries) != 1 || before.Entries[0].CostMicros != 150000 || before.Entries[0].Outcome != "no_message" {
		t.Fatalf("旧周只含归属它的已结算请求: %+v %v", before, err)
	}
	after, err := s.readUsageReport(ctx, start, start.AddDate(0, 0, 7), time.Now())
	if err != nil || len(after.Entries) != 1 || after.Entries[0].Status != "unknown" || after.Entries[0].ReservedMicros != 400000 || len(after.Legacy) != 0 {
		t.Fatalf("拒绝的请求不能入账，重启后只有未结算的新周预留: %+v %v", after, err)
	}
	if len(after.Trend) != 7 || after.Trend[5].CostMicros != 150000 || after.Trend[6].CostMicros != 0 || after.Trend[6].ReservedMicros != 400000 {
		t.Fatalf("跨周应分别汇入相邻两根周柱: %+v", after.Trend)
	}
}
