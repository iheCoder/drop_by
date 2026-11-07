package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 群聊超过旧五条上限后仍能自然推进；替身明确决定不再发言才结束。
func TestConversationEndsByChoiceBeyondOldLimit(t *testing.T) {
	s, c, _ := fixture(t)
	s.AddUserMessage(userMessage(c, "继续辩论"), time.Hour)
	c, _ = s.Conversation(c.ID)
	w := latestChatWork(t, s, c.ID)
	w.Kind = "followup"
	var spoken atomic.Int32
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.Contains(ms[0].Content, "此时只决定自己的下一步") {
			if spoken.Load() >= 12 {
				return jsonReply(Decision{Action: "pass", Reason: "本轮观点已经表达完"}), nil
			}
			return jsonReply(Decision{Action: "speak", Intent: "继续回应对方的论点"}), nil
		}
		return jsonReply(Outcome{Message: fmt.Sprintf("回应第 %d 个论点", spoken.Add(1))}), nil
	}), testConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.process(ctx, w); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow("select count(*) from messages where conversation_id=?", c.ID).Scan(&n)
	if n != 13 {
		t.Fatalf("讨论被数量限制截断: %d", n)
	}
}

// 已经发出问题卡，然后模型连接失败。新引擎从 SQLite 现场续接，问题只出现一次，
// 公开答复也只出现一次；随后再次处理只能根据最新现场判断，不重发已提交行动。
func TestActionRecoveryDoesNotReplayQuestionOrReply(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	var calls int
	failing := modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("ask_question", map[string]any{"text": "你想从哪一方开始？", "options": []string{"利", "弊"}})}}, nil
		}
		return ChatMessage{}, &requestFailure{Stage: "故障注入", Temporary: true}
	})
	e := NewEngine(s, failing, testConfig())
	if _, err := e.act(context.Background(), c, m, w, "参与辩论"); err == nil {
		t.Fatal("没有注入故障")
	}
	cp, err := s.loadAction(w.ID, m.ID)
	if err != nil || len(cp.Messages) < 4 {
		t.Fatalf("现场未保存: %+v %v", cp, err)
	}
	resumed := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.Contains(ms[0].Content, "此时只决定自己的下一步") {
			return jsonReply(Decision{Action: "pass"}), nil
		}
		return jsonReply(Outcome{Message: "我先从岗位机会的变化谈起。"}), nil
	}), testConfig())
	if err := resumed.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if err := resumed.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.Messages(c.ID, 0, 20)
	if len(msgs) != 2 || msgs[0].Question == nil || msgs[1].Body != "我先从岗位机会的变化谈起。" {
		t.Fatalf("恢复重复或丢失消息: %+v", msgs)
	}
	if _, err := s.loadAction(w.ID, m.ID); err == nil {
		t.Fatal("发布后仍保留可重放现场")
	}
	runs, _ := s.ToolRuns(c.ID, w.ID)
	if len(runs) != 1 {
		t.Fatalf("工具重复执行: %d", len(runs))
	}
}

// 同一个 tool_call 是故障重放；不同 ID 是新的意图。二者不能用内容相似度混淆。
func TestToolCallReplayUsesRecordedResult(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	box := Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	call := toolCall("ask_question", map[string]any{"text": "选一个切入点？", "options": []string{"岗位", "效率"}})
	first, err := box.Execute(context.Background(), c, m, w, call)
	if err != nil {
		t.Fatal(err)
	}
	second, err := box.Execute(context.Background(), c, m, w, call)
	if err != nil || first != second {
		t.Fatalf("未复用成功结果: %v", err)
	}
	msgs, _ := s.Messages(c.ID, 0, 20)
	if len(msgs) != 1 {
		t.Fatal("同一次调用重复发消息")
	}
	// 不确定的执行记录不能被当成失败并直接重做。
	s.db.Exec("UPDATE tool_runs SET status='running'")
	if _, err = box.Execute(context.Background(), c, m, w, call); !errors.Is(err, errUncertainTool) {
		t.Fatal(err)
	}
	// 相反，未落盘的只读查询可安全再读，不套用写操作的未知结果保护。
	read := toolCall("read_history", map[string]string{"query": ""})
	if _, err = box.Execute(context.Background(), c, m, w, read); err != nil {
		t.Fatal(err)
	}
	s.db.Exec("UPDATE tool_runs SET status='running' WHERE name='read_history'")
	if _, err = box.Execute(context.Background(), c, m, w, read); err != nil {
		t.Fatalf("只读恢复被拦截: %v", err)
	}
}

// 费用与连接错误留在运行详情，而非角色默认上下文和 read_work 的事项视图。
func TestRoleContextExcludesExecutionDiagnostics(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	secret := "预算不足 HTTP503 服务连接失败"
	s.db.Exec("UPDATE work SET status='waiting_budget',result=?,attempts=9 WHERE id=?", secret, w.ID)
	w, _ = s.ReadWork(c.ID, w.ID)
	e := NewEngine(s, nil, testConfig())
	input, err := e.contextFor(c, m, w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(asJSON(input), secret) {
		t.Fatal("执行错误混入角色上下文")
	}
	result, err := e.Tools.Execute(context.Background(), c, m, w, toolCall("read_work", map[string]string{"id": w.ID}))
	if err != nil || strings.Contains(result, secret) {
		t.Fatalf("read_work 泄漏诊断: %s %v", result, err)
	}
}

// 外部服务短暂失败时只重发该模型请求；参数错误不原样重试。
func TestModelRequestRetriesOnlyTransientFailure(t *testing.T) {
	for _, status := range []int{503, 400} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, _, _ := fixture(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": "{}"}}}}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.BaseURL = server.URL
			model := HTTPModel{Config: cfg, Store: s, Client: server.Client()}
			_, err := model.Complete(context.Background(), []ChatMessage{{Role: "user", Content: "test"}}, nil, true)
			if status == 503 && (err != nil || calls.Load() != 2) {
				t.Fatalf("短暂错误未恢复: %v %d", err, calls.Load())
			}
			if status == 400 && (err == nil || calls.Load() != 1) {
				t.Fatalf("错误请求被反复重放: %v %d", err, calls.Load())
			}
		})
	}
}

// 用户点击恢复只改变原工作状态，不插入“催促”聊天，也不允许重放已取消事项。
func TestResumeEndpointKeepsOriginalRequest(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "chat")
	s.db.Exec("UPDATE work SET status='failed',result='连接失败' WHERE id=?", w.ID)
	handler := NewHandler(NewEngine(s, nil, testConfig()))
	req := httptest.NewRequest("POST", "http://localhost/api/work/"+w.ID+"/resume", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	saved, _ := s.ReadWork(c.ID, w.ID)
	if saved.Status != "pending" || saved.Kind != "followup" {
		t.Fatalf("未恢复原请求: %+v", saved)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) != 0 {
		t.Fatal("恢复伪造了聊天消息")
	}
	s.db.Exec("UPDATE work SET status='stopped' WHERE id=?", w.ID)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatal("已取消的工作被复活")
	}
}

// 失败后尚未知道计费结果的预留与已返回用量分开显示，不能把预留冒充确认消费。
func TestUnknownRequestReservationRemainsVisible(t *testing.T) {
	s, _, _ := fixture(t)
	cfg := testConfig()
	day, err := s.Reserve(cfg, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Reserve(cfg, 100); err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(day, 100, 10, 1, 1); err != nil {
		t.Fatal(err)
	}
	u, _ := s.Usage()
	if u.UnsettledMicros != 100 || u.CostMicros != 110 {
		t.Fatalf("预留和用量混淆: %+v", u)
	}
}

// 参数错误持续交还成员判断，不按固定失败次数强行终止。
func TestRepeatedFailureDoesNotBlockChangedParameters(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	box := Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	for i := 0; i < 3; i++ {
		result, err := box.Execute(context.Background(), c, m, w, toolCall("read_tool_result", map[string]any{"id": "missing", "offset": 0}))
		if err != nil || !strings.Contains(result, "failed") {
			t.Fatalf("应先反馈实际参数错误: %s %v", result, err)
		}
	}
	if _, err := box.Execute(context.Background(), c, m, w, toolCall("read_tool_result", map[string]any{"offset": 0, "id": "missing"})); err != nil {
		t.Fatal(err)
	}
	if _, err := box.Execute(context.Background(), c, m, w, toolCall("read_tool_result", map[string]any{"offset": 1, "id": "missing"})); err != nil {
		t.Fatalf("参数已变化仍被次数阻止: %v", err)
	}
}

// 发布和清除现场必须原子完成；中途数据库失败不能留下半次提交。
func TestPublicationCheckpointIsAtomic(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	cp := actionCheckpoint{ID: "atomic_reply", WorkID: w.ID, MemberID: m.ID, Revision: c.Revision}
	if err := s.saveAction(cp); err != nil {
		t.Fatal(err)
	}
	s.db.Exec("CREATE TRIGGER fail_checkpoint BEFORE DELETE ON action_checkpoints BEGIN SELECT RAISE(ABORT,'injected'); END")
	msg := Message{ID: cp.ID, ConversationID: c.ID, AuthorID: m.ID, Body: "已接上讨论", CreatedAt: now()}
	if err := s.Publish(msg, c.Revision, cp.ID); err == nil {
		t.Fatal("故障未触发")
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) != 0 {
		t.Fatal("半次事务已经发布")
	}
	s.db.Exec("DROP TRIGGER fail_checkpoint")
	if err := s.Publish(msg, c.Revision, cp.ID); err != nil {
		t.Fatal(err)
	}
	messages, _ = s.Messages(c.ID, 0, 10)
	if len(messages) != 1 {
		t.Fatal("恢复后消息不唯一")
	}
	if _, err := s.loadAction(w.ID, m.ID); err == nil {
		t.Fatal("提交后现场未清除")
	}
}
