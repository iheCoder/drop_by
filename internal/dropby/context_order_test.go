package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 测试读取实际当前现场，不把“永远在第二个协议位置”写成需求。
func latestSceneContent(t *testing.T, messages []ChatMessage) string {
	t.Helper()
	for i := len(messages) - 1; i >= 0; i-- {
		if prefix, _, ok := messageBlock(messages[i]); ok && prefix == "" {
			return messages[i].Content
		}
	}
	t.Fatal("请求中没有当前现场")
	return ""
}

// 三次完整成员行动逐次保存、重新载入窗口。验证实际模型输入中的交错顺序、
// 已发消息的唯一正文、旧触发原话不重播，以及完整工具结果仍能回读。
func TestActionTimelineAndMessageSources(t *testing.T) {
	s, c, m := fixture(t)
	var first Message
	var sentCall ToolCall
	for round := 1; round <= 3; round++ {
		incoming := userMessage(c, fmt.Sprintf("本轮原话%d", round))
		if round == 1 {
			first = incoming
		}
		if err := s.Publish(incoming, c.Revision); err != nil {
			t.Fatal(err)
		}
		w := addWork(t, s, c, m, "followup")
		// 多轮跟聊仍带着最初的 SourceID，不能因此反复渲染最初的批评或任务。
		w.SourceID, w.Instruction = first.ID, first.Body
		calls := 0
		e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
			calls++
			raw := asJSON(ms)
			if strings.Count(raw, m.Profile) != 1 || !strings.Contains(ms[0].Content, m.Profile) {
				t.Fatal("当前人物不是唯一系统身份")
			}
			last := -1
			for previous := 1; previous <= round; previous++ {
				original := fmt.Sprintf("本轮原话%d", previous)
				at := strings.Index(raw, original)
				if at <= last || strings.Count(raw, original) != 1 {
					t.Fatal("原话倒序或重复", round, original)
				}
				last = at
				if previous < round || calls > 1 {
					reply := fmt.Sprintf("本轮回应%d", previous)
					at = strings.Index(raw, reply)
					if at <= last || strings.Count(raw, reply) != 1 {
						t.Fatal("回应错位或被回执、回流重复", round, reply)
					}
					last = at
				}
			}
			if calls == 1 {
				sentCall = toolCall("send_message", map[string]string{"message": fmt.Sprintf("本轮回应%d", round)})
				return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{sentCall}}, nil
			}
			return jsonReply(Outcome{Outcome: "本次完成"}), nil
		}), testConfig())
		if _, err := e.memberAction(context.Background(), c, m, w); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatal("意外的模型调用数", calls)
		}
		id := "tool_" + sentMessageID(w, m, sentCall)[4:]
		reread, err := e.Tools.perform(context.Background(), c, m, w, toolCall("read_tool_result", map[string]any{"id": id, "length": 10000}))
		if err != nil || !strings.Contains(asJSON(reread), fmt.Sprintf("本轮回应%d", round)) {
			t.Fatal("上下文去重损坏了原始工具记录", err)
		}
	}
	current, err := s.Member(m.ID)
	if err != nil || current.Profile != m.Profile || current.ProfileRevision != m.ProfileRevision {
		t.Fatal("日常交流自动改写了人物", err)
	}
}

// 原生工具协议不能按正文去重。只移除已确认发送的重复回执正文与自动回流，
// 同伴的相同措辞和失败调用对应的消息仍交付，未知扩展字段原样保留。
func TestMessageDedupKeepsProtocolAndDistinctSpeakers(t *testing.T) {
	call := toolCall("send_message", map[string]string{"message": "我喜欢这个结尾"})
	item := json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`)
	function := json.RawMessage(asJSON(map[string]string{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments}))
	history := []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{call}, ResponseItems: []json.RawMessage{item, function}},
		{Role: "tool", ToolCallID: call.ID, Content: `{"status":"succeeded","tool_run_id":"run","result":{"message_id":"own","message":"我喜欢这个结尾","conversation_id":"room","extra":"保留扩展"}}`},
		{Role: "user", Content: "接续收到的聊天原文（author_id 标明说话者）：" + asJSON([]Message{{ID: "own", AuthorID: "self", Body: "我喜欢这个结尾"}, {ID: "peer", AuthorID: "other", Body: "我喜欢这个结尾"}})},
	}
	got := distinctMessageSources(history)
	if asJSON(got[0].ResponseItems) != asJSON(history[0].ResponseItems) || asJSON(got[0].ToolCalls) != asJSON(history[0].ToolCalls) || got[1].ToolCallID != call.ID {
		t.Fatal("去重改写了原生协议或调用参数")
	}
	// 内部 ChatMessage 同时保存可执行调用与原生项目，实际 HTTP 只发送原生
	// 版本。核对真实序列化输入，避免把内部两份表示误算成模型收到两次。
	request, err := (&HTTPModel{Config: testConfig()}).responseBody(got, nil, false)
	if err != nil || strings.Count(string(request), "我喜欢这个结尾") != 2 || !strings.Contains(got[1].Content, "保留扩展") || !strings.Contains(got[2].Content, "peer") {
		t.Fatal("不同来源被合并，或回执字段丢失", got)
	}
	if asJSON(distinctMessageSources(got)) != asJSON(got) {
		t.Fatal("重复恢复继续改变已去重窗口")
	}
	history[1].Content = strings.Replace(history[1].Content, "succeeded", "failed", 1)
	if len(withoutDelivered([]Message{{ID: "own", Body: "我喜欢这个结尾"}}, history[:2])) != 1 {
		t.Fatal("失败的发送被当成实际交付")
	}
}

// 重现旧版“原话3、原话2、原话1、行动1、行动2、行动3”。已有配对完整
// 时恢复历史交错顺序；缺少来由的窗口保持原样，不尝试推测被压缩掉的经历。
func TestLegacySceneOrderRestoredBeforeCompletedCausesAreRemoved(t *testing.T) {
	s, c, m := fixture(t)
	var blocks, actions []ChatMessage
	for i := 1; i <= 3; i++ {
		blocks = append(blocks, ChatMessage{Role: "user", Content: "已交付的聊天原文：" + asJSON([]Message{{ID: fmt.Sprintf("message%d", i), Seq: int64(i), Body: fmt.Sprintf("原话%d", i)}})})
		actions = append(actions, ChatMessage{Role: "user", Content: "本次行动的来由：" + asJSON(map[string]any{"trigger": Work{ID: fmt.Sprintf("work%d", i)}})}, ChatMessage{Role: "assistant", Content: fmt.Sprintf("回应%d", i)})
	}
	legacy := []ChatMessage{{Role: "system", Content: "旧身份"}, blocks[2], blocks[1], blocks[0]}
	legacy = append(legacy, actions...)
	if _, err := s.db.Exec(`INSERT INTO working_contexts VALUES(?,?,?,?)`, c.ID, m.ID, 3, asJSON(legacy)); err != nil {
		t.Fatal(err)
	}
	window, err := s.loadWorkingContext(c.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, last := asJSON(window.Messages), -1
	for i := 1; i <= 3; i++ {
		for _, text := range []string{fmt.Sprintf("原话%d", i), fmt.Sprintf("回应%d", i)} {
			at := strings.Index(raw, text)
			if at <= last {
				t.Fatal("旧窗口没有恢复真实顺序", text)
			}
			last = at
		}
	}
	if strings.Contains(raw, "本次行动的来由") || asJSON(completedHistory(restoreSceneOrder(window.Messages))) != raw {
		t.Fatal("旧来由重复，或升级不幂等")
	}
	ambiguous := legacy[:len(legacy)-2]
	if asJSON(restoreSceneOrder(ambiguous)) != asJSON(ambiguous) {
		t.Fatal("猜测了缺少证据的历史")
	}
	// 正式旧库还保留着更早版本的首轮：它有回应，但尚没有独立来由。
	// 首轮原话应放在这个回应之前，后两轮仍与自己的来由对应。
	older := append(append([]ChatMessage{}, legacy[:4]...), legacy[5:]...)
	restored := restoreSceneOrder(older)
	if len(restored) != len(older) || !strings.Contains(restored[1].Content, "原话1") || restored[2].Content != "回应1" || !strings.Contains(restored[3].Content, "原话2") {
		t.Fatal("更早首轮的原话被移到后来的行动中")
	}
}
