package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDelegationToolCompletesAndReopensCoordinatorJudgment(t *testing.T) {
	// 从工具入口走完“分工→同伴按自己的身份执行→完成事件→协调者重新判断”。
	// 模型是可控替身，验证调用链与边界；不把这项测试冒充真人般效果验收。
	s, c, members := groupWithMembers(t, 2)
	c, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	human := userMessage(c, "请你们协作，先让阿苇核对出处，再决定是否做卡片。")
	if err := s.AddUserMessage(human, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	root := latestChatWork(t, s, c.ID)
	reviewed := false
	e := NewEngine(s, modelFunc(func(ctx context.Context, messages []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		var input struct {
			Self Member `json:"self"`
		}
		if err := json.Unmarshal([]byte(latestSceneContent(t, messages)), &input); err != nil {
			t.Fatal(err)
		}

		if input.Self.ID == members[0].ID {
			if autonomyRoot(ctx) != root.ID {
				return ChatMessage{}, fmt.Errorf("completion changed execution root")
			}
			reviewed = true
			return jsonReply(Outcome{Outcome: "出处暂不可用，现在先停下，不催同伴重做"}), nil
		}

		if input.Self.ID != members[1].ID || !strings.Contains(messages[0].Content, members[1].Profile) {
			t.Fatalf("specialist lost their own character: %+v", input.Self)
		}
		return jsonReply(Outcome{Outcome: "核对所需的原文目前没有提供，不能假装查实；先向协调者说明阻碍。"}), nil
	}), testConfig())
	call := toolCall("delegate_work", map[string]string{"member_id": members[1].ID, "instruction": "核对引用出处；缺少原文则说明阻碍，不做推测性背书", "reason": "你关注原始材料，先完成这一项再决定下一步"})
	if _, err := e.Tools.Execute(context.Background(), c, members[1], root, call); err == nil {
		t.Fatal("ordinary group member gained delegation tool")
	}
	if _, err := e.Tools.Execute(context.Background(), c, members[0], root, call); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.OpenWork(c.ID)
	var assigned Work
	for _, job := range jobs {
		if job.AssignedBy != "" {
			assigned = job
		}
	}
	if assigned.ID == "" {
		t.Fatal("delegation tool did not schedule concrete work")
	}
	if _, err := s.Claim(assigned.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.process(context.Background(), assigned); err != nil {
		t.Fatal(err)
	}
	finished, _ := s.ReadWork(c.ID, assigned.ID)
	if finished.Status != "done" || !strings.Contains(finished.Result, "阻碍") {
		t.Fatalf("specialist could not finish with a truthful blocker: %+v", finished)
	}
	review, err := s.ReadWork(c.ID, "coordination_"+assigned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(review.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.process(context.Background(), review); err != nil {
		t.Fatal(err)
	}
	if !reviewed {
		t.Fatal("coordinator never reconsidered after actual result")
	}
	msgs, err := s.Messages(c.ID, 0, 20)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("quiet coordination manufactured chat messages: %d %v", len(msgs), err)
	}
}

func TestDelegatedPeerWorkDoesNotRequeueCoordinatorResearch(t *testing.T) {
	// 协调者已把具体后续成功交给同伴，模型却把等待同伴误写成 unfinished。
	// 实际工具返回的工作引用必须让引擎识别交接，不能把协调者原工作再跑一遍。
	s, c, members := groupWithMembers(t, 2)
	c, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := addWork(t, s, c, members[0], "research")
	if _, err := s.Claim(parent.ID); err != nil {
		t.Fatal(err)
	}
	var calls int
	e := NewEngine(s, modelFunc(func(_ context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("delegate_work", map[string]string{"member_id": members[1].ID, "instruction": "核对现有资料里的两个日期", "reason": "你之前看过原始材料，先完成这一步即可"})}}, nil
		}
		return jsonReply(Outcome{Outcome: "核对已交给同伴，等待他的结果再判断", Unfinished: true}), nil
	}), testConfig())
	if err := e.process(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	stored, err := s.ReadWork(c.ID, parent.ID)
	if err != nil || stored.Status != "done" {
		t.Fatalf("successful handoff duplicated coordinator research: %+v %v", stored, err)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE assigned_by=?`, members[0].ID).Scan(&count)
	if count != 1 {
		t.Fatalf("delegated peer jobs=%d, want one", count)
	}
}

func TestCoordinatorIsAnOptionalRoomRole(t *testing.T) {
	// 同一成员可以在一个群负责协调，在另一个群只是普通同伴；私聊没有群职责。
	s, c, members := groupWithMembers(t, 2)
	other, err := s.CreateConversation("另一个群", "group", c.MemberIDs, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := s.CreateConversation("私聊", "direct", []string{members[0].ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CoordinatorID != members[0].ID || updated.Revision != c.Revision+1 {
		t.Fatalf("role update did not invalidate old context: %+v", updated)
	}
	unchanged, _ := s.Conversation(other.ID)
	if unchanged.CoordinatorID != "" {
		t.Fatal("role leaked across groups")
	}
	if _, err := s.SetCoordinator(direct, members[0].ID); err == nil {
		t.Fatal("private conversation accepted a coordinator")
	}
	if _, err := s.SetCoordinator(updated, "not_in_this_group"); err == nil {
		t.Fatal("foreign member accepted as coordinator")
	}
	if _, err := s.SetCoordinator(c, members[1].ID); !errors.Is(err, ErrStale) {
		t.Fatalf("stale role edit accepted: %v", err)
	}
	cleared, err := s.SetCoordinator(updated, "")
	if err != nil || cleared.CoordinatorID != "" {
		t.Fatalf("could not remove optional role: %+v %v", cleared, err)
	}
}

func TestDelegationUsesActualPermissionAndPreservesCharacter(t *testing.T) {
	// 委托只是工作的真实来源。接收者的长期人物设定、当前状态不被协调者覆盖，
	// 而且普通同伴、外群成员和已卸任的协调者都不能越权安排。
	s, c, members := groupWithMembers(t, 3)
	c, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := addWork(t, s, c, members[0], "explore")
	before := members[1].Profile
	delegated, err := s.DelegateWork(c, members[0], parent, members[1].ID,
		"核对两个来源，给出相互矛盾的地方；不替大家写结论", "你更愿意找原始材料，这一步只核对事实")
	if err != nil {
		t.Fatal(err)
	}
	if delegated.AssignedBy != members[0].ID || delegated.MemberID != members[1].ID || delegated.RootID != parent.ID || !delegated.Autonomous {
		t.Fatalf("delegation provenance lost: %+v", delegated)
	}
	still, err := s.Member(members[1].ID)
	if err != nil || still.Profile != before {
		t.Fatalf("delegation rewrote character: %+v %v", still, err)
	}
	duplicate, err := s.DelegateWork(c, members[0], parent, members[1].ID, delegated.Instruction, delegated.Reason)
	if err != nil || duplicate.ID == delegated.ID {
		t.Fatalf("new delegation was swallowed: %+v %v", duplicate, err)
	}
	for _, candidate := range []struct {
		actor     Member
		recipient string
	}{
		{members[1], members[2].ID}, {members[0], "foreign"}, {members[0], members[0].ID},
	} {
		if _, err := s.DelegateWork(c, candidate.actor, parent, candidate.recipient, "一个具体行动", "真实的选择理由"); err == nil {
			t.Fatalf("unauthorized assignment accepted: %+v", candidate)
		}
	}
	if _, err := s.SetCoordinator(c, members[2].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DelegateWork(c, members[0], parent, members[1].ID, "旧任命中的计划", "此前的职责"); !errors.Is(err, ErrStale) {
		t.Fatalf("previous coordinator retained permission: %v", err)
	}
}

func TestDelegationAllowsDistinctWorkBeyondOldCap(t *testing.T) {
	// 已有真实职责下，十二项不同工作不因旧六次配额被截断；新调用仍可独立安排。
	s, c, members := groupWithMembers(t, 4)
	c, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	root := addWork(t, s, c, members[0], "explore")
	for i := 0; i < 12; i++ {
		if _, err := s.DelegateWork(c, members[0], root, members[1+i%3].ID, fmt.Sprintf("核对第%d份材料", i), "这个部分与你的兴趣有关"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DelegateWork(c, members[0], root, members[1].ID, "又一份材料", "新的明确任务"); err != nil {
		t.Fatalf("legitimate followup rejected: %v", err)
	}
	// 即使指令相同，新安排也不因数量限制被拒绝；调用重放另由工具入口测试。
	if _, err := s.DelegateWork(c, members[0], root, members[1].ID, "核对第0份材料", "重试实际安排"); err != nil {
		t.Fatalf("idempotent retry failed at cap: %v", err)
	}
}

func TestCompletedDelegationWakesCurrentCoordinatorOnce(t *testing.T) {
	// 完成事件只通知现在的协调者，使用原来的执行根和自主额度；没有新真人
	// 消息也能继续推进，但重新提交同一次完成不会反复催醒。
	s, c, members := groupWithMembers(t, 3)
	c, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	root := addWork(t, s, c, members[0], "explore")
	assigned, err := s.DelegateWork(c, members[0], root, members[1].ID, "比较两个方案的约束", "你已经核对过材料")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetCoordinator(c, members[2].ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`UPDATE work SET status='done',result='两个方案各有一个约束' WHERE id=?`, assigned.ID); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = queueCoordinationReview(tx, assigned, "done", "两个方案各有一个约束"); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	review, err := s.ReadWork(c.ID, "coordination_"+assigned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review.MemberID != members[2].ID || review.RootID != root.ID || review.Kind != "revisit" || !review.Autonomous || review.AssignedBy != "" {
		t.Fatalf("completion did not retain role/root/autonomous boundary: %+v", review)
	}
	if !strings.Contains(review.Instruction, assigned.ID) {
		t.Fatal("completion wakeup omitted actual work reference")
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='revisit'`).Scan(&count)
	if count != 1 {
		t.Fatalf("duplicate completion wakeups: %d", count)
	}
	// 回报已入队后再次换人，也要把尚未处理的机会交给新职责人。
	c, _ = s.Conversation(c.ID)
	c, err = s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	review, err = s.ReadWork(c.ID, review.ID)
	if err != nil || review.MemberID != members[0].ID || review.Status != "pending" {
		t.Fatalf("queued report did not follow role change: %+v %v", review, err)
	}
	if _, err := s.SetCoordinator(c, ""); err != nil {
		t.Fatal(err)
	}
	review, _ = s.ReadWork(c.ID, review.ID)
	if review.Status != "stopped" {
		t.Fatal("removed coordinator retained a pending coordination wakeup")
	}
}

func TestCoordinatorIsToldFailureAndOldWorkRemainsDiscoverable(t *testing.T) {
	// 一项委托执行失败后，协调者应读到真实失败原因，而不是被当作完成。
	// 很早安排的任务也不能被几十条较新的已完成记录挤出工作索引。
	s, c, members := groupWithMembers(t, 2)
	c, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	root := addWork(t, s, c, members[0], "chat")
	assigned, err := s.DelegateWork(c, members[0], root, members[1].ID, "核对材料", "你已了解来龙去脉")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(assigned.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(assigned, "failed", "来源没有返回可用正文，尚未完成核对", time.Now()); err != nil {
		t.Fatal(err)
	}
	review, err := s.ReadWork(c.ID, "coordination_"+assigned.ID)
	if err != nil || !strings.Contains(review.Instruction, "failed") || !strings.Contains(review.Instruction, "尚未完成") {
		t.Fatalf("coordinator lost blocker: %+v %v", review, err)
	}
	old := addWork(t, s, c, members[1], "research")
	for range 55 {
		completed := addWork(t, s, c, members[1], "share")
		if _, err := s.db.Exec(`UPDATE work SET status='done' WHERE id=?`, completed.ID); err != nil {
			t.Fatal(err)
		}
	}
	open, err := s.OpenWork(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range open {
		found = found || w.ID == old.ID
	}
	if !found {
		t.Fatal("active work disappeared behind completed history")
	}
	other, err := s.CreateConversation("私聊", "direct", []string{members[0].ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadWork(other.ID, assigned.ID); err == nil {
		t.Fatal("private conversation could read a different room's work")
	}
}

func TestCompletionSurvivesExhaustedCoordinatorChain(t *testing.T) {
	// 没有继续协调的运行名额，也必须提交同伴已经做完的成果和状态。
	s, c, members := groupWithMembers(t, 2)
	c, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	root := addWork(t, s, c, members[0], "chat")
	assigned, err := s.DelegateWork(c, members[0], root, members[1].ID, "最后一份核对", "已有准备")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		w := Work{ID: newID("work"), RootID: root.ID, ConversationID: c.ID, MemberID: members[0].ID, Kind: "revisit", Status: "done", CreatedAt: now()}
		if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error { return insertWork(tx, w) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE work SET status='done',result='已完成' WHERE id=?`, assigned.ID); err != nil {
			return err
		}
		return queueCoordinationReview(tx, assigned, "done", "已完成")
	}); err != nil {
		t.Fatal(err)
	}
	finished, err := s.ReadWork(c.ID, assigned.ID)
	if err != nil || finished.Status != "done" || finished.Result != "已完成" {
		t.Fatalf("completion rolled back at coordination cap: %+v %v", finished, err)
	}
}

func TestCoordinationSurvivesStoreReopen(t *testing.T) {
	// 任命和委托都是持久事实。重启不清空职责，也不能绕过原有委托去再派一份。
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	members := []Member{{ID: "coordinator", Name: "小满"}, {ID: "specialist", Name: "阿苇"}}
	for _, m := range members {
		if err := s.AddMember(m); err != nil {
			t.Fatal(err)
		}
	}
	c, err := s.CreateConversation("试验群", "group", []string{members[0].ID, members[1].ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	root := addWork(t, s, c, members[0], "explore")
	assigned, err := s.DelegateWork(c, members[0], root, members[1].ID, "查证这个引用", "你擅长核对出处")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err = s.Conversation(c.ID)
	if err != nil || c.CoordinatorID != members[0].ID {
		t.Fatalf("coordinator lost on reopen: %+v %v", c, err)
	}
	stored, err := scanWork(s.db.QueryRow(`SELECT `+workColumns+` FROM work WHERE id=?`, assigned.ID))
	if err != nil || stored.ID != assigned.ID || stored.AssignedBy != members[0].ID || stored.RootID != root.ID {
		t.Fatalf("delegation lost on reopen: %+v %v", stored, err)
	}
}
