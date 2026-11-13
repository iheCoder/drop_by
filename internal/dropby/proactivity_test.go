package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutonomousUsageSharesTotalBudgetAndSettles(t *testing.T) {
	// 自主与前台共用总预算；保留自主用量统计，但不再设置隐藏子额度。
	s, _, _ := fixture(t)
	cfg := testConfig()
	cfg.WeeklyBudgetMicros = 300
	ctx := context.WithValue(context.Background(), autonomyKey{}, "root")
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ReserveContext(ctx, cfg, 10); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrBudget) {
				t.Errorf("unexpected reservation: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 20 {
		t.Fatalf("autonomous reservations=%d", successes.Load())
	}
	if _, err := s.Reserve(cfg, 100); err != nil {
		t.Fatalf("foreground blocked by background quota: %v", err)
	}
	if err := s.SettleContext(ctx, budgetDay(), 10, 4, 2, 1); err != nil {
		t.Fatal(err)
	}
	u, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if u.AutonomousCostMicros != 194 || u.CostMicros != 294 {
		t.Fatalf("settlement mixed budgets: %+v", u)
	}
	// 总预算不足时不能谎称只有后台额度不足、真人仍然一定能调用。
	if _, err := s.ReserveContext(ctx, cfg, 200); !errors.Is(err, ErrBudget) {
		t.Fatalf("wrong exhaustion reason: %v", err)
	}
}

func TestPersonalClockDoesNotBackoffOrMultiplyByRooms(t *testing.T) {
	// 同一个人加入两群仍只有一份活动机会。沉默和后来的真人消息都不改已经排好的下一次。
	s, c, m := fixture(t)
	s.CreateConversation("另一个群", "group", []string{m.ID}, time.Hour)
	for range 3 {
		s.db.Exec(`UPDATE personal_activity SET next_at=0`)
		before := time.Now()
		if err := s.ScheduleExploration(); err != nil {
			t.Fatal(err)
		}
		var due int64
		var n int
		s.db.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&due)
		s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND status='pending'`).Scan(&n)
		if n != 1 || due <= before.UnixMilli() {
			t.Fatalf("个人时钟按房间重复或没有排下一次: n=%d due=%d", n, due)
		}
		s.db.Exec(`UPDATE work SET status='done' WHERE kind='explore'`)
	}
	var prior int64
	s.db.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&prior)
	s.AddUserMessage(userMessage(c, "我回来了"), time.Hour)
	var current int64
	s.db.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&current)
	if prior != current {
		t.Fatal("真人消息重置了成员自己的活动时钟")
	}
}

func TestFollowupKeepsHumanSourceButUsesAutonomousBudgetAndSurvivesNewMessage(t *testing.T) {
	s, c, m := fixture(t)
	msg := userMessage(c, "反攻略这个想法挺有趣")
	if err := s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	parent := latestChatWork(t, s, c.ID)
	if _, err := s.ScheduleFollowup(c, m, parent, "稍后再考虑反攻略的结尾", "由刚才的话产生的好奇", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var follow Work
	jobs, _ := s.Works(c.ID)
	for _, w := range jobs {
		if w.Kind == "revisit" {
			follow = w
		}
	}
	if follow.SourceID != msg.ID || !follow.Autonomous || follow.RootID != parent.ID {
		t.Fatalf("source/budget/lineage lost: %+v", follow)
	}
	if err := s.AddUserMessage(userMessage(c, "先聊聊别的"), time.Hour); err != nil {
		t.Fatal(err)
	}
	var status string
	s.db.QueryRow(`SELECT status FROM work WHERE id=?`, follow.ID).Scan(&status)
	if status != "pending" {
		t.Fatalf("new message erased followup: %s", status)
	}
	// 明确停止同时取消真正研究与稍后考虑，不依赖模型成功。
	if err := s.AddUserMessage(userMessage(c, "不用研究了"), time.Hour); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(`SELECT status FROM work WHERE id=?`, follow.ID).Scan(&status)
	if status != "stopped" {
		t.Fatalf("stop did not cancel followup: %s", status)
	}
}

func TestFollowupOnlyWakesOwnerAndCanStaySilent(t *testing.T) {
	s, c, members := groupWithMembers(t, 3)
	w := addWork(t, s, c, members[1], "revisit")
	var calls atomic.Int32
	e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls.Add(1)
		var input struct {
			Self Member `json:"self"`
		}
		json.Unmarshal([]byte(latestSceneContent(t, ms)), &input)
		if input.Self.ID != members[1].ID {
			t.Errorf("wrong owner woke: %s", input.Self.ID)
		}
		if autonomyRoot(ctx) != w.ID {
			t.Errorf("missing autonomous cost context")
		}
		return jsonReply(Outcome{Outcome: "现在已经没有继续的必要"}), nil
	}), testConfig())
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("followup woke whole room: %d", calls.Load())
	}
	msgs, _ := s.Messages(c.ID, 0, 20)
	if len(msgs) != 0 {
		t.Fatalf("silent followup published: %+v", msgs)
	}
}

func TestQuietMemberCanScheduleReconsiderationWithoutPosting(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "explore")
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if len(ms) == 3 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("schedule_followup", map[string]any{"instruction": "晚点再想卡片的结尾", "delay_seconds": 3600})}}, nil
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='revisit' AND root_id=? AND autonomous=1`, w.ID).Scan(&count)
	if count != 1 {
		t.Fatalf("quiet followup count=%d", count)
	}
	msgs, _ := s.Messages(c.ID, 0, 20)
	if len(msgs) != 0 {
		t.Fatal("scheduling manufactured speech")
	}
}

func TestIndependentBackgroundWorkDoesNotOccupyHumanResponse(t *testing.T) {
	// 三位不同成员的研究都卡在外部请求时，第四位仍可完成真人回应。
	// 检查实际消息，不把“进入控制判断”误当作用户已经得到回复。
	s, _, members := groupWithMembers(t, 4)
	for _, m := range members[:3] {
		c, err := s.CreateConversation(m.Name, "direct", []string{m.ID}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		addWork(t, s, c, m, "research")
	}
	foreground, err := s.CreateConversation("真人房间", "direct", []string{members[3].ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE personal_activity SET next_at=?`, time.Now().Add(time.Hour).UnixMilli())
	started := make(chan struct{}, 3)
	e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if autonomyRoot(ctx) != "" {
			started <- struct{}{}
			<-ctx.Done()
			return ChatMessage{}, ctx.Err()
		}
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{}), nil
		}
		if len(ms) == 3 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("send_message", map[string]string{"message": "我在，接着聊。"})}}, nil
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for range 3 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("不同成员被全局执行位挡住")
		}
	}
	if err = e.Submit(userMessage(foreground, "现在聊一聊")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if testMemberSpoke(s, foreground.ID, members[3].ID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("只有控制判断成功，实际回复仍被其他人的工作挡住")
}

func TestRunningFollowupContinuesWithoutHumanInterruption(t *testing.T) {
	// 成员正在推进挂念时，普通真人补充不能取消这次推理；原行动自然结束。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "revisit")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if autonomyRoot(ctx) == w.ID && calls.Add(1) == 1 {
			close(started)
			select {
			case <-ctx.Done():
				return ChatMessage{}, ctx.Err()
			case <-release:
			}
		}
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{Discussion: "keep", Research: "keep"}), nil
		}
		return jsonReply(Outcome{Outcome: "读完新消息后暂时不继续"}), nil
	}), testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("followup did not start")
	}
	if err := e.Submit(userMessage(c, "先补充一点情况")); err != nil {
		t.Fatal(err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := s.db.QueryRow(`SELECT status FROM work WHERE id=?`, w.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "done" && calls.Load() == 1 {
			return
		}
		if status == "superseded" {
			t.Fatal("human interruption erased pending concern")
		}
		time.Sleep(20 * time.Millisecond)
	}
	jobs, _ := s.Works(c.ID)
	t.Fatalf("followup did not reconsider after interruption: calls=%d jobs=%+v", calls.Load(), jobs)
}

func TestDelegatedFollowupDoesNotDuplicateFinishedResearch(t *testing.T) {
	// 真实验收曾出现作品完成、复看已安排，但模型仍返回 unfinished=true。
	// 本次成功委托的实际记录应防止原研究与新后续同时重做一件事。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	if _, err := s.Claim(w.ID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("schedule_followup", map[string]any{"instruction": "稍后复看已完成的卡片", "reason": "留一点距离再判断", "delay_seconds": 60})}}, nil
		}
		return jsonReply(Outcome{Outcome: "卡片已完成，稍后复看已安排", Unfinished: true}), nil
	}), testConfig())
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	var status string
	s.db.QueryRow(`SELECT status FROM work WHERE id=?`, w.ID).Scan(&status)
	if status != "done" {
		t.Fatalf("delegated research requeued: %s", status)
	}
	var revisits int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='revisit' AND status='pending'`).Scan(&revisits)
	if revisits != 1 {
		t.Fatalf("delegated followups=%d", revisits)
	}
}

func TestSharingFinishedWorkDoesNotCreateGenericResearch(t *testing.T) {
	// 分享已经保存的作品时，模型可能把未来复看误当成本次未完成。
	// 单纯的分享阶段不应再创建“向用户告知已完成”的无效研究。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "share")
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {

		var input struct {
			Messages []Message `json:"latest_messages"`
		}
		json.Unmarshal([]byte(latestSceneContent(t, ms)), &input)
		if len(input.Messages) > 0 {
			return jsonReply(Outcome{Outcome: "已告知"}), nil
		}

		return jsonReply(Outcome{Message: "卡片已保存，稍后再复看。", Unfinished: true}), nil
	}), testConfig())
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	var researches int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='research'`).Scan(&researches)
	if researches != 0 {
		t.Fatalf("sharing generated %d unnecessary jobs", researches)
	}
}

func TestExplicitResumeGetsNewExecutionRootWithoutOrdinaryReset(t *testing.T) {
	s, c, m := fixture(t)
	old := addWork(t, s, c, m, "revisit")
	if _, err := s.db.Exec(`UPDATE work SET status='paused',root_id=?,autonomous=1 WHERE id=?`, old.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	msg := userMessage(c, "请继续刚才的复看")
	if err := s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	trigger := latestChatWork(t, s, c.ID)
	e := NewEngine(s, modelFunc(func(_ context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		return jsonReply(Control{Discussion: "keep", Research: "keep", ResumeIDs: []string{old.ID}}), nil
	}), testConfig())
	var control Control
	response, _ := e.Model.Complete(context.Background(), nil, nil, true)
	if err := parseJSON(response.Content, &control); err != nil {
		t.Fatal(err)
	}
	if err := e.applyControl(context.Background(), c, trigger, control); err != nil {
		t.Fatal(err)
	}
	resumed, err := scanWork(s.db.QueryRow(`SELECT `+workColumns+` FROM work WHERE id=?`, old.ID))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.RootID != trigger.ID || resumed.SourceID != msg.ID || resumed.Status != "pending" {
		t.Fatalf("resume lost new authority: %+v", resumed)
	}
}

// 执行窗口结束不等于讨论完成：最后的新消息有一次全群观察机会，
// 但它不会获得新的自主配额，也不会因为没人说话而自行无限重试。
