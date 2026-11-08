package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestUnreadBurstAndWorkingContextContinueAcrossActions(t *testing.T) {
	// 三十条短消息一次完整交付；生成时再到一条，结束这一轮不能顺便标为已读。
	s, c, m := fixture(t)
	for i := 0; i < 30; i++ {
		if err := s.Publish(userMessage(c, fmt.Sprintf("补充 %d", i)), c.Revision); err != nil {
			t.Fatal(err)
		}
	}
	w := addWork(t, s, c, m, "followup")
	var delivered int64
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.Contains(asJSON(ms), "已有理解延续") {
			if err := s.Publish(userMessage(c, "生成过程中追加的第三十一条"), c.Revision); err != nil {
				t.Fatal(err)
			}
			return jsonReply(Outcome{Outcome: "本轮安静"}), nil
		}
		var input struct {
			Messages []Message `json:"latest_messages"`
		}
		if err := json.Unmarshal([]byte(ms[1].Content), &input); err != nil {
			t.Fatal(err)
		}
		if len(input.Messages) != 30 {
			t.Fatalf("只交付 %d 条", len(input.Messages))
		}
		delivered = input.Messages[29].Seq

		return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("organize_context", map[string]string{"note": "已有理解延续"})}}, nil
	}), testConfig())
	if _, err := e.memberAction(context.Background(), c, m, w); err != nil {
		t.Fatal(err)
	}
	seen, _, _ := s.observation(c.ID, m.ID)
	if seen != delivered {
		t.Fatal("未交付的新消息被标记已读")
	}
	if _, err := s.loadAction(w.ID, m.ID); err == nil {
		t.Fatal("已完成行动仍有执行断点")
	}
	window, err := s.loadWorkingContext(c.ID, m.ID)
	if err != nil || !strings.Contains(asJSON(window.Messages), "已有理解延续") {
		t.Fatalf("静默结束丢失上下文：%v", err)
	}
	// 新 Engine 不依赖内存旧会话；第二次请求只有新增消息，旧理解由持久窗口接续。
	next := addWork(t, s, c, m, "followup")
	resumed := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if !strings.Contains(asJSON(ms), "已有理解延续") {
			t.Error("新行动未接续理解")
		}
		var input struct {
			Messages []Message `json:"latest_messages"`
		}
		if err := json.Unmarshal([]byte(ms[1].Content), &input); err != nil {
			t.Fatal(err)
		}
		if len(input.Messages) != 1 || !strings.Contains(input.Messages[0].Body, "第三十一条") {
			t.Fatal("未增量交付")
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	if _, err := resumed.memberAction(context.Background(), c, m, next); err != nil {
		t.Fatal(err)
	}
}

func TestMemberOrganizesBeforeCapacityAndRetainsOriginals(t *testing.T) {
	// 工具正文尚能放下时成员主动整理。验证实际下一次输入、续接存储、完整回读，
	// 而不是只检查工具声明。便笺保留不同人的观点和更正，不改变成员资料。
	s, c, m := fixture(t)
	source := userMessage(c, "今晚只想聊，不做计划。")
	if err := s.Publish(source, c.Revision); err != nil {
		t.Fatal(err)
	}
	w := addWork(t, s, c, m, "followup")
	raw := asJSON(map[string]string{"content": strings.Repeat("旧节目资料", 5000) + "原文尾部"})
	if err := s.RecordTool(ToolRun{ID: "old_page", WorkID: w.ID, ConversationID: c.ID, MemberID: m.ID, Name: "read_page", Arguments: json.RawMessage(`{"url":"https://example.com"}`), Result: raw, Status: "succeeded", CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTool(ToolRun{ID: "new_page", WorkID: w.ID, ConversationID: c.ID, MemberID: m.ID, Name: "read_page", Arguments: json.RawMessage(`{}`), Result: strings.Repeat("新读取内容", 5000) + "新原文尾部", Status: "succeeded", CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	note := "我原来偏好甲；同伴偏好乙，尚未达成共识。用户今晚只想聊，不做计划。"
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		switch calls {
		case 1:
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("read_tool_result", map[string]any{"id": "old_page", "length": 100000})}}, nil
		case 2:
			if !strings.Contains(asJSON(ms), "原文尾部") {
				t.Error("主动读取正文被提前截断")
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("read_tool_result", map[string]any{"id": "new_page", "length": 100000}), toolCall("organize_context", map[string]any{"note": note, "keep_message_ids": []string{source.ID}}), toolCall("send_message", map[string]string{"message": "那就随便聊聊。"})}}, nil
		default:
			if strings.Contains(asJSON(ms), strings.Repeat("旧节目资料", 50)) {
				t.Error("主动整理后旧正文仍在请求里")
			}
			if !strings.Contains(asJSON(ms), "新原文尾部") {
				t.Error("同批刚读取的结果尚未看过就被丢弃")
			}
			if !strings.Contains(asJSON(ms), note) || !strings.Contains(asJSON(ms), source.ID) {
				t.Error("整理丢失分歧或原话")
			}
			// 整批完成后以回执交付，不能留下孤立工具协议或重复执行发言。
			var calls, results int
			for _, msg := range ms {
				calls += len(msg.ToolCalls)
				if msg.Role == "tool" {
					results++
				}
			}
			if calls != results {
				t.Errorf("整理破坏工具配对：%d/%d", calls, results)
			}
			return jsonReply(Outcome{}), nil
		}
	}), testConfig())
	if _, err := e.memberAction(context.Background(), c, m, w); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.loadWorkingContext(c.ID, m.ID)
	if !strings.Contains(asJSON(saved.Messages), note) || strings.Contains(asJSON(saved.Messages), strings.Repeat("旧节目资料", 50)) {
		t.Fatal("整理结果未持久保存")
	}
	reread, err := e.Tools.perform(context.Background(), c, m, w, toolCall("read_tool_result", map[string]any{"id": "old_page", "length": 100000}))
	if err != nil || !strings.Contains(asJSON(reread), "原文尾部") {
		t.Fatalf("整理删除了原始资料：%v", err)
	}
	current, _ := s.Member(m.ID)
	if current.Profile != m.Profile {
		t.Fatal("整理改变了特质")
	}
	messages, _ := s.Messages(c.ID, 0, -1)
	if len(messages) != 2 {
		t.Fatal("同批消息被丢失或重复")
	}
}

func TestOrganizeContextOwnershipAndToolFailure(t *testing.T) {
	s, c, people := groupWithMembers(t, 2)
	m, other := people[0], people[1]
	w := addWork(t, s, c, m, "followup")
	private, _ := s.CreateConversation("另一私聊", "direct", []string{m.ID}, time.Hour)
	secret := userMessage(private, "不能搬到群里的原话")
	s.Publish(secret, private.Revision)
	s.RecordTool(ToolRun{ID: "peer_note", WorkID: w.ID, ConversationID: c.ID, MemberID: other.ID, Name: "organize_context", Arguments: json.RawMessage(`{}`), Result: `{"note":"同伴自己的理解"}`, Status: "succeeded", CreatedAt: now()})
	box := NewEngine(s, nil, testConfig()).Tools
	for _, args := range []map[string]any{{"note": "x", "keep_tool_run_ids": []string{"peer_note"}}} {
		raw, err := box.Execute(context.Background(), c, m, w, toolCall("organize_context", args))
		if err != nil || !strings.Contains(raw, `"status":"failed"`) {
			t.Fatalf("越权整理应返回可修正的工具失败：%s %v", raw, err)
		}
	}
	got, err := s.organizeContext(c, m, asJSON(map[string]any{"note": "跨场景引用", "keep_message_ids": []string{secret.ID}}))
	if err != nil || len(got.Messages) != 1 || got.Messages[0].Body != secret.Body || got.Messages[0].ConversationID != private.ID {
		t.Fatalf("本人原话跨场景丢失: %+v %v", got, err)
	}
	// 本人的有效引用完整传递；无任意条数或字符上限。
	longNote := strings.Repeat("仍有用的不同看法。", 2000)
	result, err := box.Execute(context.Background(), c, m, w, toolCall("organize_context", map[string]string{"note": longNote}))
	if err != nil || !strings.Contains(result, longNote) {
		t.Fatal("续接便笺被截短")
	}
}
