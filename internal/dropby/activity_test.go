package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParticipationFailureStaysWithMember(t *testing.T) {
	// 两位成员，一人安静、一人引用无效。错误只保留给本人；同伴仍记录为自愿沉默，但此次处理不能假装全部完成。
	//一次纠正仍无效后必须退出，且失败不推进已读位置。
	s, c, people := groupWithMembers(t, 2)
	if err := s.AddUserMessage(userMessage(c, "下雨天想说就说"), time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	c, _ = s.Conversation(c.ID)
	var corrections atomic.Int32
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, defs []ToolDefinition, _ bool) (ChatMessage, error) {
		var input struct {
			Self       Member `json:"self"`
			Correction string `json:"correction"`
		}
		json.Unmarshal([]byte(ms[1].Content), &input)
		if input.Self.ID == people[0].ID {
			return jsonReply(Decision{Action: "pass", MessageIDs: []string{"not-a-message"}}), nil
		}
		if input.Correction != "" {
			corrections.Add(1)
			if len(defs) != 0 {
				t.Error("纠正引用不应重放工具")
			}
		}
		return jsonReply(Decision{Action: "speak", Intent: "回应雨声", MessageIDs: []string{"not-a-message"}}), nil
	}), testConfig())
	candidates, _, err := e.observeMembers(context.Background(), c, w, 0)
	if !isParticipationFailure(err) || len(candidates) != 0 || corrections.Load() != 1 {
		t.Fatalf("局部故障扩大或无限纠正: %v, %d", err, corrections.Load())
	}
	records, _ := s.Participations(c.ID)
	actions := map[string]string{}
	for _, p := range records {
		actions[p.MemberID] = p.Action
	}
	if actions[people[0].ID] != "pass" || actions[people[1].ID] != "error" {
		t.Fatalf("故障被冒充为沉默: %+v", records)
	}
	seen, _, _ := s.observation(c.ID, people[1].ID)
	if seen != 0 {
		t.Fatal("失败判断推进了已读位置")
	}
}

func TestInvalidSelectionCanBeCorrectedWithoutChangingScope(t *testing.T) {
	// 先选择外群消息，再纠正到真实本群消息。不能擅自替换，也不能读取外群正文。
	s, c, m := fixture(t)
	original := userMessage(c, "想聊雨声")
	s.AddUserMessage(original, time.Hour)
	c, _ = s.Conversation(c.ID)
	w := latestChatWork(t, s, c.ID)
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		var input map[string]any
		json.Unmarshal([]byte(ms[1].Content), &input)
		if calls == 1 {
			return jsonReply(Decision{Action: "speak", Intent: "聊雨声", MessageIDs: []string{"msg_other_room"}}), nil
		}
		if input["correction"] == nil {
			t.Error("没有把引用问题交还本人")
		}
		return jsonReply(Decision{Action: "speak", Intent: "聊雨声", MessageIDs: []string{original.ID}}), nil
	}), testConfig())
	choice := e.observeMember(context.Background(), c, w, m.ID, 0, nil)
	if choice.Err != nil || calls != 2 || len(choice.Decision.MessageIDs) != 1 || choice.Decision.MessageIDs[0] != original.ID {
		t.Fatalf("纠正没有保留真实选择: %+v", choice)
	}
}

func TestLaterDecisionFailurePreservesPublishedConversation(t *testing.T) {
	// 真调度与数据库：先成功发布一条回复，后续观察的全部成员发生服务错误。
	// 这次交流正常退出；回复仍在，失败判断仍在，不能靠重跑整轮复制消息。
	s, c, m := fixture(t)
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{}), nil
		}
		if strings.Contains(ms[0].Content, "此时只决定自己的下一步") {
			var input struct {
				AlreadySpoke bool `json:"already_spoke"`
			}
			json.Unmarshal([]byte(ms[1].Content), &input)
			if input.AlreadySpoke {
				return ChatMessage{}, errors.New("模型本次请求失败")
			}
			return jsonReply(Decision{Action: "speak", Intent: "回应雨声"}), nil
		}
		return jsonReply(Outcome{Message: "听雨也不错。"}), nil
	}), testConfig())
	if err := e.Submit(userMessage(c, "下雨天做什么？")); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	e.dispatch(context.Background())
	e.wg.Wait()
	saved, _ := s.ReadWork(c.ID, w.ID)
	messages, _ := s.Messages(c.ID, 0, 10)
	records, _ := s.Participations(c.ID)
	if saved.Status != "failed" || len(messages) != 2 || messages[1].AuthorID != m.ID {
		t.Fatalf("应保留已发布交流并记录后续故障: %+v %+v", saved, messages)
	}
	if records[0].Action != "error" {
		t.Fatal("局部错误记录丢失")
	}
}

func TestParticipationSharedErrorsStillStopWork(t *testing.T) {
	// 即使同伴愿意说话，预算、取消和存储故障也不能被局部容错吞掉。
	for _, cause := range []error{ErrBudget, ErrStale, context.Canceled, ErrContextCapacity, &modelAccountingError{errors.New("记账存储失败")}} {
		t.Run(cause.Error(), func(t *testing.T) {
			s, c, people := groupWithMembers(t, 2)
			w := addWork(t, s, c, people[0], "chat")
			e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
				var input struct {
					Self Member `json:"self"`
				}
				json.Unmarshal([]byte(ms[1].Content), &input)
				if input.Self.ID == people[0].ID {
					return ChatMessage{}, cause
				}
				return jsonReply(Decision{Action: "speak", Intent: "愿意继续"}), nil
			}), testConfig())
			_, _, err := e.observeMembers(context.Background(), c, w, 0)
			if !errors.Is(err, cause) {
				t.Fatalf("共享错误被吞掉: %v", err)
			}
		})
	}
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "chat")
	s.Close()
	e := NewEngine(s, nil, testConfig())
	_, _, err := e.observeMembers(context.Background(), c, w, 0)
	if err == nil || isParticipationFailure(err) {
		t.Fatalf("存储故障被局部化: %v", err)
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
