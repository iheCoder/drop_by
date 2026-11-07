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

func TestIdleBackoffResetsOnlyForHumanAndPrioritizesDueWork(t *testing.T) {
	s, c, m := fixture(t)
	for round := 1; round <= 2; round++ {
		if _, err := s.db.Exec(`UPDATE conversations SET next_explore=0 WHERE id=?`, c.ID); err != nil {
			t.Fatal(err)
		}
		before := time.Now()
		if err := s.ScheduleExploration(time.Minute); err != nil {
			t.Fatal(err)
		}
		var due int64
		var rounds int
		if err := s.db.QueryRow(`SELECT next_explore,idle_rounds FROM conversations WHERE id=?`, c.ID).Scan(&due, &rounds); err != nil {
			t.Fatal(err)
		}
		want := time.Minute * time.Duration(1<<round)
		if rounds != round || time.UnixMilli(due).Sub(before) < want-time.Second {
			t.Fatalf("idle backoff round=%d due=%d", rounds, due)
		}
		if _, err := s.db.Exec(`UPDATE work SET status='done' WHERE kind='explore'`); err != nil {
			t.Fatal(err)
		}
		// BOT 新消息不能让闲时调度重新变得频繁。
		if err := s.Publish(Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: m.ID, Body: "一个小想法", CreatedAt: now()}, c.Revision); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddUserMessage(userMessage(c, "我回来了"), time.Minute); err != nil {
		t.Fatal(err)
	}
	var rounds int
	s.db.QueryRow(`SELECT idle_rounds FROM conversations WHERE id=?`, c.ID).Scan(&rounds)
	if rounds != 0 {
		t.Fatal("human did not reset idle cadence")
	}
	// 待执行的具体工作优先，低价值闲时检查不抢在前面。
	if _, err := s.db.Exec(`UPDATE conversations SET next_explore=0 WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleExploration(time.Minute); err != nil {
		t.Fatal(err)
	}
	var pending int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND status='pending'`).Scan(&pending)
	if pending != 0 {
		t.Fatal("idle work queued ahead of foreground")
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
		json.Unmarshal([]byte(ms[1].Content), &input)
		if input.Self.ID != members[1].ID {
			t.Errorf("wrong owner woke: %s", input.Self.ID)
		}
		if autonomyRoot(ctx) != w.ID {
			t.Errorf("missing autonomous cost context")
		}
		return jsonReply(Decision{Action: "pass", Reason: "现在已经没有继续的必要"}), nil
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
		if !strings.Contains(ms[0].Content, "此时只决定自己的下一步") {
			t.Fatal("quiet scheduling invoked public generation")
		}
		return jsonReply(Decision{Action: "revisit", Intent: "晚点再想卡片的结尾", Reason: "还在意刚才的创作", DelaySeconds: 3600}), nil
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

func TestBusyAutonomyLeavesAWorkerForNewHumanMessage(t *testing.T) {
	// 两个房间都有到期自主机会，第一个模型请求一直阻塞。
	// 第二个自主机会必须等待，第三个房间的新真人输入仍应进入前台执行位。
	s, first, m := fixture(t)
	second, err := s.CreateConversation("第二个房间", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	foreground, err := s.CreateConversation("真人房间", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	addWork(t, s, first, m, "explore")
	addWork(t, s, second, m, "explore")
	started := make(chan struct{})
	responded := make(chan struct{})
	var once sync.Once
	var frontOnce sync.Once
	var autoCalls atomic.Int32
	e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if autonomyRoot(ctx) != "" {
			autoCalls.Add(1)
			once.Do(func() { close(started) })
			<-ctx.Done()
			return ChatMessage{}, ctx.Err()
		}
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			frontOnce.Do(func() { close(responded) })
			return jsonReply(Control{Discussion: "keep", Research: "keep"}), nil
		}
		return jsonReply(Decision{Action: "pass", Reason: "本次没有要补充的"}), nil
	}), testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("autonomous work did not start")
	}
	if err := e.Submit(userMessage(foreground, "现在聊一聊")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-responded:
	case <-time.After(3 * time.Second):
		t.Fatal("foreground blocked behind background work")
	}
	if autoCalls.Load() != 1 {
		t.Fatalf("both worker slots occupied by autonomy: %d", autoCalls.Load())
	}
}

func TestRunningFollowupSurvivesInterruptionByHuman(t *testing.T) {
	// 模拟成员刚醒来、用户又插话：旧推理应取消，具体挂念仍保留并重读新消息。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "revisit")
	started := make(chan struct{})
	var calls atomic.Int32
	e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if autonomyRoot(ctx) != "" && calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return ChatMessage{}, ctx.Err()
		}
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{Discussion: "keep", Research: "keep"}), nil
		}
		return jsonReply(Decision{Action: "pass", Reason: "读完新消息后暂时不继续"}), nil
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
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := s.db.QueryRow(`SELECT status FROM work WHERE id=?`, w.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "done" && calls.Load() >= 2 {
			return
		}
		if status == "superseded" {
			t.Fatal("human interruption erased pending concern")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("followup did not reconsider after interruption")
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
		if strings.Contains(ms[0].Content, "此时只决定自己的下一步") {
			var input struct {
				Messages []Message `json:"latest_messages"`
			}
			json.Unmarshal([]byte(ms[1].Content), &input)
			if len(input.Messages) > 0 {
				return jsonReply(Decision{Action: "pass", Reason: "已告知"}), nil
			}
			return jsonReply(Decision{Action: "speak", Intent: "告知已保存", Reason: "成果可查看"}), nil
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
	if err := e.applyControl(context.Background(), c, trigger); err != nil {
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
