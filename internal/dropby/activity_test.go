package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// 辅助引用写错时忽略无效项，保持本人发言意图和有效的本群引用；
// 不为一个可选 ID 发起另一轮纠正，也不读取外群内容。
func TestInvalidSelectionDoesNotBlockParticipation(t *testing.T) {
	// 可选引用输错不应吞掉作者正文，正文仍走实际发布流程。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		return jsonReply(Outcome{Message: "我喜欢雨声。", ReplyTo: "not_in_this_room"}), nil
	}), testConfig())
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.Messages(c.ID, 0, 10)
	if len(msgs) != 1 || msgs[0].Body != "我喜欢雨声。" || msgs[0].ReplyTo != "" {
		t.Fatalf("可选引用阻断发言: %+v", msgs)
	}
}

func TestLaterDecisionFailurePreservesPublishedConversation(t *testing.T) {
	// 行动中先发消息、继续思考时断线；已发消息与故障分别保存。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	calls := 0
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("send_message", map[string]string{"message": "听雨也不错。"})}}, nil
		}
		return ChatMessage{}, errors.New("模型本次请求失败")
	}), testConfig())
	e.dispatch(context.Background())
	e.wg.Wait()
	saved, _ := s.ReadWork(c.ID, w.ID)
	messages, _ := s.Messages(c.ID, 0, 10)
	records, _ := s.Participations(c.ID)
	if saved.Status != "failed" || len(messages) != 1 || messages[0].AuthorID != m.ID || records[0].Action != "error" {
		t.Fatalf("已经发出的消息或故障丢失: %+v %+v %+v", saved, messages, records)
	}
}

func TestParticipationSharedErrorsStillStopWork(t *testing.T) {
	// 本人不能把预算、取消、上下文或记账故障装成自愿沉默；直接行动保留原错误。
	for _, cause := range []error{ErrBudget, ErrStale, context.Canceled, ErrContextCapacity, &modelAccountingError{errors.New("记账存储失败")}} {
		t.Run(cause.Error(), func(t *testing.T) {
			s, c, m := fixture(t)
			w := addWork(t, s, c, m, "followup")
			e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
				return ChatMessage{}, cause
			}), testConfig())
			if _, err := e.act(context.Background(), c, m, w, ""); !errors.Is(err, cause) {
				t.Fatalf("故障被吞掉: %v", err)
			}
		})
	}
}

func TestCloseWaitsForActualWorkAndCancellationIsOwned(t *testing.T) {
	s, c, people := groupWithMembers(t, 2)
	c, root := responsibilityStep(t, s, c, "这次请你组织", Control{ResponsibilityChanges: []ResponsibilityChange{{MemberID: people[0].ID, ScopeID: "this", Body: "为雨天活动取名"}}})
	child, err := s.DelegateWork(c, people[0], root, people[1].ID, "提出一个名字", "你关注用词")
	if err != nil {
		t.Fatal(err)
	}
	role := c.Responsibilities[0]
	toolbox := Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	raw, err := toolbox.Execute(context.Background(), c, people[0], root, toolCall("close_responsibility", map[string]string{"id": role.ID}))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Status string `json:"status"`
		Result struct {
			Pending []Work `json:"pending_work"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(raw), &response)
	if response.Status != "waiting" || len(response.Result.Pending) != 1 || response.Result.Pending[0].ID != child.ID {
		t.Fatalf("收尾没有说明在等谁: %s", raw)
	}
	// 协调者也不能取消同伴工作；失败的研究仍不能算结果已经收齐。
	if _, err = s.CancelOwnWork(c, people[0], child.ID); err == nil {
		t.Fatal("取消越过成员归属")
	}
	s.db.Exec(`UPDATE work SET status='failed' WHERE id=?`, child.ID)
	if err = s.CloseResponsibility(c, people[0], root, role.ID); err == nil {
		t.Fatal("失败工作被算成已完成")
	}
	// 真正结束后再收尾；重复确认不能改写已保存结果，也不影响其他职责。
	s.db.Exec(`UPDATE work SET status='done',result='提出了雨声留白' WHERE id=?`, child.ID)
	if _, err = s.CancelOwnWork(c, people[1], child.ID); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := s.ReadWork(c.ID, child.ID)
	if unchanged.Status != "done" || unchanged.Result != "提出了雨声留白" {
		t.Fatal("重复取消篡改了完成事实")
	}
	for i := 0; i < 2; i++ {
		raw, err = toolbox.Execute(context.Background(), c, people[0], root, toolCall("close_responsibility", map[string]string{"id": role.ID}))
		if err != nil || !strings.Contains(raw, `"status":"succeeded"`) {
			t.Fatalf("重复确认职责结束被当成故障: %v %s", err, raw)
		}
	}
	if _, err = s.CancelOwnWork(c, people[1], "missing"); err == nil {
		t.Fatal("未知工作被当作成功取消")
	}
	// 终态幂等不能扩大取消范围；另一个聊天和正在执行的本人工作仍不能取消。
	other, err := s.CreateConversation("另一个聊天", "direct", []string{people[1].ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	foreign := addWork(t, s, other, people[1], "research")
	if _, err = s.CancelOwnWork(c, people[1], foreign.ID); err == nil {
		t.Fatal("取消越过聊天边界")
	}
	running := addWork(t, s, c, people[1], "research")
	s.db.Exec(`UPDATE work SET status='running' WHERE id=?`, running.ID)
	if _, err = s.CancelOwnWork(c, people[1], running.ID); err == nil {
		t.Fatal("取消了在途工作")
	}
	stillRunning, _ := s.ReadWork(c.ID, running.ID)
	if stillRunning.Status != "running" {
		t.Fatal("失败的取消改变了工作状态")
	}
}

func TestActivitySummariesKeepExecutionAndOutcomeDistinct(t *testing.T) {
	// HTTP 展示读取实际记录，旧委托的工具成功不等于对方完成。
	// 后续状态变化后摘要更新；历史错误仍保留，已撤销职责不能继续显示为当前失败。
	s, c, people := groupWithMembers(t, 2)
	c, root := responsibilityStep(t, s, c, "请组织一个雨天活动", Control{ResponsibilityChanges: []ResponsibilityChange{{MemberID: people[0].ID, ScopeID: "this", Body: "临时组织雨天活动"}}})
	child, err := s.DelegateWork(c, people[0], root, people[1].ID, "想一个名字", "你喜欢文字")
	if err != nil {
		t.Fatal(err)
	}
	run := ToolRun{ID: newID("tool"), WorkID: root.ID, ConversationID: c.ID, MemberID: people[0].ID, Name: "delegate_work", Arguments: json.RawMessage(asJSON(map[string]string{"member_id": people[1].ID, "instruction": "想一个名字"})), Status: "succeeded", Result: asJSON(map[string]string{"work_id": child.ID}), CreatedAt: now()}
	s.RecordTool(run)
	d, err := s.actionSummary(run)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "已安排" || !strings.Contains(d.Title, people[1].Name) || strings.Contains(d.Detail, "已完成") {
		t.Fatalf("安排冒充完成: %+v", d)
	}
	s.db.Exec(`UPDATE work SET status='done' WHERE id=?`, child.ID)
	d, _ = s.actionSummary(run)
	if !strings.Contains(d.Detail, "当前进展：已结束") || d.Status != "已安排" {
		t.Fatalf("没有区分动作和当前结果: %+v", d)
	}
	role := c.Responsibilities[0]
	old := run
	old.ID = newID("tool")
	old.Name = "close_responsibility"
	old.Status = "failed"
	old.Arguments = json.RawMessage(asJSON(map[string]string{"id": role.ID}))
	old.Result = `{"error":"这件事还有未完研究；先核对"}`
	s.RecordTool(old)
	s.CloseResponsibility(c, people[0], root, role.ID)
	d, _ = s.actionSummary(old)
	if d.Status != "当时未结束 · 现已结束" || !strings.Contains(d.Detail, "现在已结束") {
		t.Fatalf("历史失败仍冒充当前状态: %+v", d)
	}
	internal := child
	internal.ID = "coordination_" + child.ID
	internal.Kind = "revisit"
	internal.Instruction = "先用 read_work 核对工作 work_internal，再按 member_hidden 的职责继续"
	internal.Result = ""
	d, _ = s.workSummary(internal)
	if strings.Contains(asJSON(d), "read_work") || strings.Contains(d.Detail, "member_hidden") {
		t.Fatalf("内部指令进入普通动态: %+v", d)
	}
	handler := NewHandler(NewEngine(s, nil, testConfig()))
	response := preferenceRequest(handler, "GET", "/api/conversations/"+c.ID, nil)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var data struct {
		Runs []ToolRun `json:"tool_runs"`
	}
	json.Unmarshal(response.Body.Bytes(), &data)
	if len(data.Runs) != 2 || data.Runs[0].Display == nil || data.Runs[0].Result != "" {
		t.Fatal("列表没有提供轻量摘要")
	}
	detail := preferenceRequest(handler, "GET", "/api/tool-runs/"+old.ID, nil)
	var actual ToolRun
	json.Unmarshal(detail.Body.Bytes(), &actual)
	if detail.Code != 200 || actual.Result != old.Result || actual.Status != "failed" || actual.Display == nil {
		t.Fatal("展示清理改写了原始诊断记录")
	}
}
