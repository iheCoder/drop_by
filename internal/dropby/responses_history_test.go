package dropby

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 聊天和研究共用成员的工作窗口。把历史落盘后还原，再用两个场景的
// 请求构造器检查同源工具配对以及其他接入点读到的可见历史。
func TestResponsesContinuationStaysWithItsSource(t *testing.T) {
	s, c, m := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"status":"completed","output":[{"type":"reasoning","encrypted_content":"old-opaque-state"},{"type":"function_call","call_id":"old_call","name":"read_history","arguments":"{}"}],"usage":{"input_tokens":10,"output_tokens":10}}`)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.setModelEndpoint(server.URL)
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	messages := []ChatMessage{{Role: "user", Content: "接续原话"}}
	reply, err := model.Complete(t.Context(), messages, nil, false)
	if err != nil || reply.NativeSource == "" || len(reply.ToolCalls) != 1 {
		t.Fatalf("没有保留可供续接的工具响应: %+v %v", reply, err)
	}
	messages = append(messages,
		ChatMessage{Role: "user", NativeSource: reply.NativeSource, ResponseItems: []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"压缩里保留的原话"}]}`),
			json.RawMessage(`{"type":"compaction","encrypted_content":"old-compact-state"}`),
		}},
		reply, ChatMessage{Role: "tool", ToolCallID: "old_call", Content: "来自本地的执行结果"})
	if _, err := s.db.Exec("INSERT INTO working_contexts(conversation_id,member_id,seen,messages) VALUES(?,?,0,?)", c.ID, m.ID, asJSON(messages)); err != nil {
		t.Fatal(err)
	}
	window, err := s.loadWorkingContext(c.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	same := responsesForTest(cfg)
	sameRaw, err := same.responseBody(window.Messages, nil, false)
	if err != nil || !strings.Contains(string(sameRaw), "old-opaque-state") || !strings.Contains(string(sameRaw), "old-compact-state") ||
		!strings.Contains(string(sameRaw), `"call_id":"old_call","output":"来自本地的执行结果","type":"function_call_output"`) {
		t.Fatalf("同源续接丢失原生项或配对: %s %v", sameRaw, err)
	}

	// 即使模型 ID 相同，换另一接入点也不能复用旧接入点的原生状态。
	cfg.setModelEndpoint("https://other.example.com/v1")
	other := responsesForTest(cfg)
	otherRaw, err := other.responseBody(window.Messages, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"old-opaque-state", "old-compact-state", "function_call_output"} {
		if strings.Contains(string(otherRaw), forbidden) {
			t.Errorf("跨接入点携带了 %s: %s", forbidden, otherRaw)
		}
	}
	for _, visible := range []string{"接续原话", "压缩里保留的原话", "read_history", "来自本地的执行结果"} {
		if !strings.Contains(string(otherRaw), visible) {
			t.Errorf("切换接入点后丢失 %s: %s", visible, otherRaw)
		}
	}
	// 旧库无来源标记。只有默认接入点允许继续使用旧的 Responses 状态。
	legacy := append([]ChatMessage(nil), window.Messages...)
	for i := range legacy {
		legacy[i].NativeSource = ""
	}
	settings := cfg.Models.Scenes[sceneMemberSettings]
	settings.Provider = "another-provider"
	cfg.Models.Providers[settings.Provider] = cfg.Models.Providers[defaultModelProvider]
	cfg.Models.Scenes[sceneMemberSettings] = settings
	legacyRaw, err := responsesForTest(cfg).responseBody(legacy, nil, false)
	if err != nil || strings.Contains(string(legacyRaw), "old-opaque-state") ||
		!strings.Contains(string(legacyRaw), "来自本地的执行结果") {
		t.Fatalf("非默认接入点应只收到旧库可见历史: %s %v", legacyRaw, err)
	}
	again, err := same.responseBody(window.Messages, nil, false)
	if err != nil || string(again) != string(sameRaw) {
		t.Fatal("编码其他接入点的输入时改动了原始窗口")
	}
}
