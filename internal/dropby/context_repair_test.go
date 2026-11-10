package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 更新系统现场只替换系统快照；原话及成员自己的理解不能被一起删除。
func TestRefreshScenePreservesDeliveredMessages(t *testing.T) {
	scene := func(text string) ChatMessage {
		return ChatMessage{Role: "user", Content: asJSON(map[string]any{"self": "a", "trigger": "work", "time": "now", "memory": "旧索引", "latest_messages": []Message{{ID: text, Body: text}}})}
	}
	history := []ChatMessage{scene("原话一"), {Role: "assistant", Content: "我的理解"}, scene("原话二")}
	got := refreshScene(history)
	raw := asJSON(got)
	if strings.Contains(raw, "旧索引") || !strings.Contains(raw, "原话一") || !strings.Contains(raw, "原话二") || !strings.Contains(raw, "我的理解") {
		t.Fatal(raw)
	}
	if asJSON(refreshScene(got)) != raw {
		t.Fatal("重复恢复改变了原话")
	}
	// 已 steering 的真人补充不能因为前面夹着 BOT 而重复交付。
	batch := []Message{{ID: "bot"}, {ID: "human"}}
	filtered := withoutDelivered(batch, []ChatMessage{{Role: "user", Content: "真人追加的消息：" + asJSON(batch[1:])}})
	if len(filtered) != 1 || filtered[0].ID != "bot" {
		t.Fatal(filtered)
	}
}

// 升级前的首轮断点可能接着一串安静结束的行动，因而没有 assistant。
// 恢复要分清已经读过的旧原话和这次还没送出的现场。
func TestLegacyFirstRequestCheckpointSeparatesOldAndUnseenMessages(t *testing.T) {
	s, c, m := fixture(t)
	work := addWork(t, s, c, m, "chat")
	messages := appendDeliveredMessages(nil, []Message{{ID: "old", ConversationID: c.ID, Body: "已结束的旧话题"}})
	scene := asJSON(map[string]any{
		"self": m.ID, "trigger": work.ID, "time": now(),
		"latest_messages": []Message{{ID: "new", ConversationID: c.ID, Body: "最新原话"}},
	})
	messages = append(messages, ChatMessage{Role: "user", Content: scene},
		ChatMessage{Role: "user", Content: actionCausePrefix + asJSON(map[string]any{"trigger": work})})
	if err := s.saveAction(actionCheckpoint{ID: "legacy", WorkID: work.ID, MemberID: m.ID, Messages: messages}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := s.loadAction(work.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, old, next := splitCompactionWindow(checkpoint.Messages)
	if len(old) != 1 || !strings.Contains(old[0].Content, "已结束的旧话题") || !strings.Contains(asJSON(next), "最新原话") {
		t.Fatalf("历史和待交付内容没有区分: %v / %v", old, next)
	}
}

// 压缩成功后即使紧接着的计数故障，落盘回调已经收到可恢复窗口。
func TestCompactionSavedBeforeFollowupNetworkFailure(t *testing.T) {
	s, _, _ := fixture(t)
	counts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/compact") {
			io.WriteString(w, `{"output":[{"type":"compaction","encrypted_content":"saved"}],"usage":{"input_tokens":100,"output_tokens":2}}`)
			return
		}
		counts++
		if counts == 1 {
			io.WriteString(w, `{"input_tokens":3000}`)
		} else {
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Model = "gpt-6.1-sol"
	cfg.BaseURL = server.URL
	cfg.ContextTokenBudget = 2000
	m := &HTTPModel{Store: s, Config: cfg, Client: server.Client()}
	var saved []ChatMessage
	ctx := context.WithValue(context.Background(), preparedContextKey{}, savePreparedContext(func(ms []ChatMessage) error { saved = ms; return nil }))
	_, _, _, err := m.prepareLargeResponse(ctx, largeArtifactChain(), nil, false)
	if err == nil || !strings.Contains(asJSON(saved), "saved") {
		t.Fatalf("成功压缩丢失: %v %v", saved, err)
	}
	if saved[len(saved)-1].Role == "assistant" {
		t.Fatal("恢复会误把压缩记录当作待执行响应")
	}
	raw, _ := json.Marshal(saved)
	var restored []ChatMessage
	json.Unmarshal(raw, &restored)
	if restored[len(restored)-1].OutputTokens != 2 {
		t.Fatal("输出用量未随窗口保存")
	}
}

type unsentTransport struct{}

func (unsentTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}
}

// 可证明连接未建立的失败释放该笔预留；不把它算作已消费或未知费用。
func TestUnsentRequestReleasesReservation(t *testing.T) {
	s, _, _ := fixture(t)
	cfg := testConfig()
	m := &HTTPModel{Store: s, Config: cfg, Client: &http.Client{Transport: unsentTransport{}}}
	call, err := s.reserveCall(context.Background(), cfg, 10000, "OpenAI", "gpt-6.1-sol", "")
	if err != nil {
		t.Fatal(err)
	}
	err = requestJSON(context.Background(), m.Client, "http://unused/responses", "", []byte(`{}`), &map[string]any{})
	if err == nil {
		t.Fatal("expected connection failure")
	}
	if err = m.releaseUnsent(call, err); err != nil {
		t.Fatal(err)
	}
	s.finishUsageCall(call)
	var status, outcome string
	var cost int64
	s.db.QueryRow("SELECT status,outcome,cost_micros FROM usage_calls WHERE id=?", call.id).Scan(&status, &outcome, &cost)
	if status != "settled" || outcome != "not_sent" || cost != 0 {
		t.Fatal(status, outcome, cost)
	}
	usage, _ := s.Usage()
	if usage.UnsettledMicros != 0 || usage.CostMicros != 0 {
		t.Fatal(usage)
	}
}

// 模型已应用补充消息、随后工具执行被取消：断点正文和 seen 必须同时前进。
func TestSteeredCursorSavedWithResponseBeforeToolCancellation(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testConfig()
	cfg.MidTurnSteering = true
	var delivered Message
	e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		delivered = userMessage(c, "生成中追加的补充")
		if err := s.Publish(delivered, c.Revision); err != nil {
			t.Fatal(err)
		}
		steering := ctx.Value(steeringKey{}).(*steeringInput)
		steering.Applied[delivered.ID] = true
		cancel()
		return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("read_history", map[string]any{})}}, nil
	}), cfg)
	if _, err := e.memberAction(ctx, c, m, w); err == nil {
		t.Fatal("预期工具前取消")
	}
	var saved, seq int64
	s.db.QueryRow("SELECT seen FROM action_checkpoints WHERE work_id=? AND member_id=?", w.ID, m.ID).Scan(&saved)
	s.db.QueryRow("SELECT seq FROM messages WHERE id=?", delivered.ID).Scan(&seq)
	if seq == 0 || saved != seq {
		t.Fatalf("断点游标滞后: %d / %d", saved, seq)
	}
}
