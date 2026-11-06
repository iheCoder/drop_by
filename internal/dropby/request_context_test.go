package dropby

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRequestCapacityCompactsToolTextWithoutLosingIdentityOrProtocol(t *testing.T) {
	// 长工具结果触发请求总量保护；角色设定、状态和目标是受保护的原始输入，
	// 工具正文可以折叠，但回读 ID、函数调用配对和加密推理必须仍在真实 HTTP 请求中。
	s, _, _ := fixture(t)
	core := "角色：好奇但克制；当前目标：核对引用；约定：不要重复刚才的答案。"
	opaque := strings.Repeat("encrypted-state", 20000)
	first := asJSON(map[string]any{"tool_run_id": "tool_older", "status": "succeeded", "result": strings.Repeat("旧资料有很多细节。", 4000)})
	second := asJSON(map[string]any{"tool_run_id": "tool_newer", "status": "succeeded", "result": strings.Repeat("最新资料也很长。", 3000)})
	messages := []ChatMessage{
		{Role: "system", Content: core},
		{Role: "user", Content: "保持原来目标，核对两个资料有哪些不同。"},
		{Role: "assistant", OutputTokens: 50, ResponseItems: []json.RawMessage{
			json.RawMessage(asJSON(map[string]any{"type": "reasoning", "id": "rs_preserved", "encrypted_content": opaque, "summary": []any{}})),
			json.RawMessage(`{"type":"function_call","id":"fc_older","call_id":"call_older","name":"read_history","arguments":"{}"}`),
			json.RawMessage(`{"type":"function_call","id":"fc_newer","call_id":"call_newer","name":"read_history","arguments":"{}"}`),
		}},
		{Role: "tool", ToolCallID: "call_older", Content: first},
		{Role: "tool", ToolCallID: "call_newer", Content: second},
	}
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		raw, _ := io.ReadAll(r.Body)
		for _, mustKeep := range []string{core, opaque, "tool_older", "tool_newer", "call_older", "call_newer", "function_call_output", "read_tool_result"} {
			if !bytes.Contains(raw, []byte(mustKeep)) {
				t.Errorf("压缩丢失身份、工具回读线索或协议: %s", clip(mustKeep, 80))
			}
		}
		if bytes.Contains(raw, []byte(strings.Repeat("旧资料有很多细节。", 1000))) || bytes.Contains(raw, []byte(strings.Repeat("最新资料也很长。", 1000))) {
			t.Error("真实请求仍带着过长工具正文")
		}
		io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"已核对"}]}],"usage":{"input_tokens":200,"output_tokens":10}}`)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Model, cfg.Reasoning, cfg.BaseURL = "gpt-5.6-sol", "high", server.URL
	cfg.ContextTokenBudget = 8000
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	if _, err := model.Complete(context.Background(), messages, nil, false); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 || messages[3].Content != first || messages[4].Content != second {
		t.Fatal("没有发送真实请求，或者压缩修改了行动层持有的原始消息")
	}
}

func TestRequestCapacityCountsCoreToolsReasoningAndOutputBeforeSpending(t *testing.T) {
	// 这四类内容都不能被聊天消息的局部字数限制漏算；超过配置时不得先调用、扣费，
	// 也不能静默丢掉角色设定或工具定义来制造表面成功。
	for _, kind := range []string{"core", "tool_schema", "reported_reasoning", "output_reservation"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := fixture(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				io.WriteString(w, `{"status":"completed","output":[],"usage":{}}`)
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.Model, cfg.Reasoning, cfg.BaseURL = "gpt-5.6-sol", "high", server.URL
			cfg.ContextTokenBudget = 1000
			messages := []ChatMessage{{Role: "system", Content: "稳定身份"}, {Role: "user", Content: "当前问题"}}
			var defs []ToolDefinition
			switch kind {
			case "core":
				messages[0].Content = strings.Repeat("稳定身份和目标", 300)
			case "tool_schema":
				defs = []ToolDefinition{definition("read_history", strings.Repeat("完整工具定义", 300), map[string]any{})}
			case "reported_reasoning":
				messages = append(messages, ChatMessage{Role: "assistant", OutputTokens: 900, ResponseItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_counted","encrypted_content":"small-encoding","summary":[]}`)}})
			case "output_reservation":
				cfg.MaxOutputTokens = 1000
			}
			model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
			if _, err := model.Complete(context.Background(), messages, defs, false); err == nil || !strings.Contains(err.Error(), "上下文容量") {
				t.Fatalf("应说明内部容量边界: %v", err)
			}
			u, err := s.Usage()
			if err != nil || requests.Load() != 0 || u.Calls != 0 || u.CostMicros != 0 {
				t.Fatalf("容量检查应发生在请求和预算预留之前: requests=%d usage=%+v err=%v", requests.Load(), u, err)
			}
		})
	}
}
