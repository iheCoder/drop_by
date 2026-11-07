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
	cfg.Model, cfg.Reasoning, cfg.BaseURL = "gpt-6.1-sol", "high", server.URL
	cfg.ContextTokenBudget = 8000
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	if _, err := model.Complete(context.Background(), messages, nil, false); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 || messages[3].Content != first || messages[4].Content != second {
		t.Fatal("没有发送真实请求，或者压缩修改了行动层持有的原始消息")
	}
}

func TestInvalidOutputReservationDoesNotCallProvider(t *testing.T) {
	// 明显错误的配置无需计数/压缩：输出预留已经占满窗口，任何输入都放不下。
	s, _, _ := fixture(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	cfg := testConfig()
	cfg.Model, cfg.Reasoning, cfg.BaseURL = "gpt-6.1-sol", "high", server.URL
	cfg.ContextTokenBudget, cfg.MaxOutputTokens = 1000, 1000
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	_, err := model.Complete(context.Background(), []ChatMessage{{Role: "user", Content: "第一句话"}}, nil, false)
	u, _ := s.Usage()
	if err == nil || !strings.Contains(err.Error(), "上下文容量配置") || requests.Load() != 0 || u.Calls != 0 {
		t.Fatalf("无效配置不应调用或扣费: %v %d %+v", err, requests.Load(), u)
	}
}
