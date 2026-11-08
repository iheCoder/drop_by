package dropby

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// 从真人消息进入控制解释，使用真实存储与权限；模型替身只负责已经确认的语义。
// 语义识别本身另在真实模型验收中跑，不能用此测试宣称所有自然语言都能识别。
func responsibilityStep(t *testing.T, s *Store, c Conversation, text string, control Control) (Conversation, Work) {
	t.Helper()
	if err := s.AddUserMessage(userMessage(c, text), time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	w := latestChatWork(t, s, c.ID)
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		return jsonReply(control), nil
	}), testConfig())
	if err := e.applyControl(context.Background(), c, w); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	w, _ = s.ReadWork(c.ID, w.ID)
	return c, w
}

func TestConversationalResponsibilitiesHandoffScopeAndCancellation(t *testing.T) {
	s, c, m := groupWithMembers(t, 3)
	// 长期 A、临时 B 同时存在。B 只得到这一件事的组织权限，其他事项仍由 A 协调。
	c, _ = responsibilityStep(t, s, c, "A，以后需要组织事情时帮我张罗", Control{ResponsibilityChanges: []ResponsibilityChange{{MemberID: m[0].ID, Body: "群里共同研究时分工和跟进"}}})
	if len(c.Responsibilities) != 1 || c.CoordinatorID != "" {
		t.Fatal("对话任命不应退回旧开关")
	}
	c, matter := responsibilityStep(t, s, c, "这次活动由 B 组织", Control{ResponsibilityChanges: []ResponsibilityChange{{MemberID: m[1].ID, ScopeID: "this", Body: "只组织这次周末活动"}}})
	if !s.CanDelegate(c, m[1], matter) || s.CanDelegate(c, m[0], matter) {
		t.Fatal("临时事项没有优先于长期职责")
	}
	unrelated := addWork(t, s, c, m[0], "explore")
	if !s.CanDelegate(c, m[0], unrelated) || s.CanDelegate(c, m[1], unrelated) {
		t.Fatal("临时职责泄漏到无关事项")
	}
	child, err := s.DelegateWork(c, m[1], matter, m[2].ID, "核对场馆开放时间", "你关注细节")
	if err != nil {
		t.Fatal(err)
	}
	// 完成一个具体分工，产生结果回报；任务的既有结果必须在交接后仍在。
	if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE work SET status='done',result='开放时间已核对' WHERE id=?`, child.ID); err != nil {
			return err
		}
		return queueCoordinationReview(tx, child, "done", "开放时间已核对")
	}); err != nil {
		t.Fatal(err)
	}
	var tempID string
	for _, r := range c.Responsibilities {
		if r.ScopeID == matter.ID {
			tempID = r.ID
		}
	}
	c, handoff := responsibilityStep(t, s, c, "这次活动还是交给 A 接着组织，已有结果别重做", Control{RelatedWorkID: matter.ID, ResponsibilityChanges: []ResponsibilityChange{{ID: tempID, MemberID: m[0].ID}}})
	review, _ := s.ReadWork(c.ID, "coordination_"+child.ID)
	saved, _ := s.ReadWork(c.ID, child.ID)
	if review.MemberID != m[0].ID || saved.Result != "开放时间已核对" || saved.Status != "done" {
		t.Fatal("交接丢失成果或回报仍归旧负责人")
	}
	if handoff.ParentID != matter.ID || !s.CanDelegate(c, m[0], handoff) || s.CanDelegate(c, m[1], handoff) {
		t.Fatal("新消息没有沿用真实事项范围")
	}
	next, err := s.DelegateWork(c, m[0], handoff, m[2].ID, "根据已知开放时间核对交通", "沿用已有材料继续下一步")
	if err != nil {
		t.Fatal(err)
	}
	// 只撤销临时职责不取消研究；原长期角色与仍在做的工作继续存在。
	c, _ = responsibilityStep(t, s, c, "撤销这次临时分工职责，研究先保留", Control{ResponsibilityChanges: []ResponsibilityChange{{ID: tempID, Revoke: true}}})
	saved, _ = s.ReadWork(c.ID, next.ID)
	if saved.Status != "pending" {
		t.Fatal("撤销职责误取消了任务")
	}
	// 明确取消整个事项才收束其树。迟到 Finish 不能再次唤醒人或改回 done。
	c, _ = responsibilityStep(t, s, c, "周末不出去了，把活动这件事停下", Control{StopIDs: []string{matter.ID}})
	saved, _ = s.ReadWork(c.ID, next.ID)
	if saved.Status != "stopped" {
		t.Fatal("取消事项漏掉交接后的后续")
	}
	if err := s.Finish(next, "done", "晚到的完成结果", time.Now()); err != nil {
		t.Fatal(err)
	}
	saved, _ = s.ReadWork(c.ID, next.ID)
	if saved.Status != "stopped" {
		t.Fatal("晚到结果复活了已取消事项")
	}
	if s.CanDelegate(c, m[0], handoff) {
		t.Fatal("长期职责不能绕过事项取消")
	}
}

func TestTemporarySubcoordinationEndsWithoutChangingPersonality(t *testing.T) {
	s, c, m := groupWithMembers(t, 3)
	c, root := responsibilityStep(t, s, c, "A 组织这次制作", Control{ResponsibilityChanges: []ResponsibilityChange{{MemberID: m[0].ID, ScopeID: "this", Body: "组织这次制作"}}})
	original := m[1].Profile
	sub, err := s.DelegateWork(c, m[0], root, m[1].ID, "组织卡片制作", "你更了解文案与样式", true)
	if err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	if !s.CanDelegate(c, m[1], sub) {
		t.Fatal("被委托组织子事项的人没有真实委托能力")
	}
	job, err := s.DelegateWork(c, m[1], sub, m[2].ID, "核对卡片文字", "你更关注用词")
	if err != nil {
		t.Fatal(err)
	}
	var roleID string
	for _, r := range c.Responsibilities {
		if r.ScopeID == sub.ID {
			roleID = r.ID
		}
	}
	if err := s.CloseResponsibility(c, m[1], sub, roleID); err == nil {
		t.Fatal("不能把未完成子研究当作已经收齐结果")
	}
	s.db.Exec(`UPDATE work SET status='done',result='文字已核对' WHERE id=?`, job.ID)
	if err := s.CloseResponsibility(c, m[1], sub, roleID); err != nil {
		t.Fatal(err)
	}
	fresh, _ := s.Member(m[1].ID)
	if fresh.Profile != original || s.CanDelegate(c, m[1], sub) {
		t.Fatal("临时职责结束不应改变人物或永久保留组织权")
	}
	// 无关成员、外群来源和旧真人消息不能给自己任命；失败不改变原职责。
	fake := root
	fake.SourceID = "not-a-human-message"
	err = s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		return applyResponsibilities(tx, c, fake, []ResponsibilityChange{{MemberID: m[2].ID, Body: "自己任命自己"}})
	})
	if err == nil || errors.Is(err, ErrBudget) {
		t.Fatal("缺少真人来源的任命被接受")
	}
}

func TestExplicitFirstSpeakerScopeDoesNotMuteOtherTopics(t *testing.T) {
	// 顺序要求作为用户原话保留，不翻译成全体成员的轮流发言规则。
	s, c, m := groupWithMembers(t, 2)
	c, w := responsibilityStep(t, s, c, "A 先说，其他人先别说", Control{})
	e := NewEngine(s, nil, testConfig())
	for _, member := range m {
		input, err := e.contextFor(c, member, w)
		if err != nil || !strings.Contains(asJSON(input["latest_messages"]), "A 先说") || strings.Contains(asJSON(input["trigger"]), "first_speakers") {
			t.Fatalf("原指示丢失或变成统一排序规则: %v", err)
		}
	}
}

func TestLegacyWorkRootIsMigratedForCancellation(t *testing.T) {
	// 模拟升级前只有 root_id 的真实后续。迁移不应让这些旧任务成为取消范围外的孤儿。
	s, c, m := fixture(t)
	root := addWork(t, s, c, m, "chat")
	child := addWork(t, s, c, m, "research")
	if _, err := s.db.Exec(`UPDATE work SET root_id=?,parent_id='' WHERE id=?`, root.ID, child.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.initResponsibilities(); err != nil {
		t.Fatal(err)
	}
	child, err := s.ReadWork(c.ID, child.ID)
	if err != nil || child.ParentID != root.ID {
		t.Fatalf("旧工作来源没有迁移: %+v %v", child, err)
	}
	if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error { return stopWorkFamily(tx, c.ID, root.ID) }); err != nil {
		t.Fatal(err)
	}
	child, _ = s.ReadWork(c.ID, child.ID)
	if child.Status != "stopped" {
		t.Fatal("旧版后续仍能在事项取消后运行")
	}
}

func TestResponsibilityRetryReusesTheSameHumanAgreement(t *testing.T) {
	s, c, m := fixture(t)
	if err := s.AddUserMessage(userMessage(c, "以后共同研究时由你组织"), time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	w := latestChatWork(t, s, c.ID)
	changes := []ResponsibilityChange{{MemberID: m.ID, Body: "共同研究时帮助分工和跟进"}}
	// 同一条真人消息在执行恢复后重读两次，不能重复制造长期职责。
	for i := 0; i < 2; i++ {
		if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error { return applyResponsibilities(tx, c, w, changes) }); err != nil {
			t.Fatal(err)
		}
	}
	roles, err := s.Responsibilities(c.ID)
	if err != nil || len(roles) != 1 || roles[0].SourceID != w.SourceID {
		t.Fatalf("同一个约定被重复建立: %+v %v", roles, err)
	}
}
