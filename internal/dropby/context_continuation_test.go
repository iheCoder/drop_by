package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func largeArtifactChain() []ChatMessage {
	// 复现石粒的请求形状：工具结果已经很短，但 assistant 的作品参数很长。
	// 它是已完成的函数调用，不能直接删除参数或留下没有配对的 call_id。
	call := map[string]any{"type": "function_call", "call_id": "saved_call", "name": "save_artifact", "arguments": asJSON(map[string]string{"content": strings.Repeat("已保存的完整作品。", 4000), "title": "作品"})}
	raw, _ := json.Marshal(call)
	return []ChatMessage{{Role: "system", Content: "身份与当前目标必须保留"}, {Role: "user", Content: "请完成作品并说明真实进度"}, {Role: "assistant", OutputTokens: 12000, ResponseItems: []json.RawMessage{raw}}, {Role: "tool", ToolCallID: "saved_call", Content: `{"tool_run_id":"saved_tool","result":{"artifact_id":"saved_artifact","version":1}}`}}
}

func TestActualInputCountOverridesByteFalsePositive(t *testing.T) {
	s, _, _ := fixture(t)
	countCalls, generations := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/responses/input_tokens":
			countCalls++
			io.WriteString(w, `{"input_tokens":500}`)
		case "/responses":
			generations++
			if !strings.Contains(string(raw), "saved_call") || !strings.Contains(string(raw), "已保存的完整作品") {
				t.Error("可容纳的真实请求不应丢失已完成工具协议")
			}
			io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"完成"}]}],"usage":{"input_tokens":500,"output_tokens":10}}`)
		default:
			t.Errorf("不应压缩确实能放下的请求: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Model, cfg.Reasoning, cfg.BaseURL = "gpt-6.1-sol", "high", server.URL
	cfg.ContextTokenBudget = 2000
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	if _, err := model.Complete(context.Background(), largeArtifactChain(), nil, false); err != nil {
		t.Fatal(err)
	}
	usage, _ := s.Usage()
	if countCalls != 1 || generations != 1 || usage.Calls != 1 {
		t.Fatalf("应一次计数、一次实际生成: %d %d %+v", countCalls, generations, usage)
	}
}

func TestNativeCompactionPreservesCoreProtocolAndContinuesFromReturnedWindow(t *testing.T) {
	s, _, _ := fixture(t)
	compactCalls, countCalls, generations := 0, 0, 0
	returned := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"请完成作品并说明真实进度"}]}`), json.RawMessage(`{"type":"compaction","id":"cmp_1","encrypted_content":"opaque-compact-state"}`)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		switch r.URL.Path {
		case "/responses/input_tokens":
			countCalls++
			if strings.Contains(body, "opaque-compact-state") {
				io.WriteString(w, `{"input_tokens":600}`)
			} else {
				io.WriteString(w, `{"input_tokens":9000}`)
			}
		case "/responses/compact":
			compactCalls++
			if !strings.Contains(body, "saved_call") || !strings.Contains(body, "saved_artifact") || !strings.Contains(body, "已保存的完整作品") {
				t.Error("压缩输入必须包含完整调用和已保存结果")
			}
			json.NewEncoder(w).Encode(map[string]any{"output": returned, "usage": map[string]int{"input_tokens": 9000, "output_tokens": 40}})
		case "/responses":
			generations++
			if !strings.Contains(body, "身份与当前目标必须保留") || !strings.Contains(body, "opaque-compact-state") || strings.Contains(body, "已保存的完整作品") {
				t.Error("压缩后请求没有同时保留核心与完整压缩窗口")
			}
			io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"已完成并保留作品"}]}],"usage":{"input_tokens":600,"output_tokens":20}}`)
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Model, cfg.Reasoning, cfg.BaseURL = "gpt-6.1-sol", "high", server.URL
	cfg.ContextTokenBudget = 3000
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	chain := largeArtifactChain()
	if _, err := completeModel(context.Background(), model, &chain, nil, false); err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 || chain[0].Content != "身份与当前目标必须保留" || len(chain[1].ResponseItems) != 2 {
		t.Fatalf("行动层没有继承整个压缩窗口: %+v", chain)
	}
	for i := range returned {
		if string(chain[1].ResponseItems[i]) != string(returned[i]) {
			t.Error("原生输出被挑选或改写")
		}
	}
	// 下一次直接从新窗口继续，不再拿旧长参数重新压缩。
	if _, err := completeModel(context.Background(), model, &chain, nil, false); err != nil {
		t.Fatal(err)
	}
	if compactCalls != 1 || generations != 2 || countCalls != 2 {
		t.Fatalf("压缩应只发生一次: compact=%d generate=%d count=%d", compactCalls, generations, countCalls)
	}
	usage, _ := s.Usage()
	if usage.Calls != 3 || usage.InputTokens != 10200 {
		t.Fatalf("压缩和生成都应结算，计数不当作生成: %+v", usage)
	}
}

func TestCountingFailurePreservesChainAndIsRecoverable(t *testing.T) {
	s, _, _ := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/input_tokens" {
			t.Error("计数失败后不能盲目调用生成")
		}
		w.WriteHeader(503)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Model, cfg.Reasoning, cfg.BaseURL = "gpt-6.1-sol", "high", server.URL
	cfg.ContextTokenBudget = 2000
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	chain := largeArtifactChain()
	_, err := completeModel(context.Background(), model, &chain, nil, false)
	if !errors.Is(err, ErrContextCapacity) || len(chain) != 4 {
		t.Fatalf("不能把计数服务失败当成人物/用户输入过长: %v", err)
	}
	usage, _ := s.Usage()
	if usage.Calls != 0 || usage.CostMicros != 0 {
		t.Fatal("未生成不应预留生成费用")
	}
}

// 显式启动才使用真实额度。长工具调用为合成材料，检验实际 API 的计数、
// 压缩与续接兼容性；不把这份测试材料混入用户的聊天或作品。
func TestLiveResponsesCountCompactAndContinue(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_COMPACTION") != "1" {
		t.Skip("显式开启真实 Responses 压缩验收")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("DROPBY_COMPACTION_DATA_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.ContextTokenBudget, cfg.MaxOutputTokens, cfg.Reasoning = 8000, 2000, "high"
	cfg.WeeklyBudgetMicros = 900000
	model := &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 3 * time.Minute}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	chain := largeArtifactChain()
	chain[0].Content = "这是接口兼容性测试。你是记录员，目标是保留已经完成的作品证据。只返回 JSON，不调用工具。"
	chain[1].Content = "已有作品无需再做。请在 JSON 的 artifact_id 和 version 字段中写出已保存的真实 ID 和版本。"
	defer func() {
		u, _ := s.Usage()
		t.Logf("真实压缩验收：估算 $%.4f，生成/压缩 %d 次，输入 %d，输出 %d", float64(u.CostMicros)/1e6, u.Calls, u.InputTokens, u.OutputTokens)
	}()
	result, err := completeModel(ctx, model, &chain, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		ID      string `json:"artifact_id"`
		Version int    `json:"version"`
	}
	if err := parseJSON(result.Content, &value); err != nil {
		t.Fatal(err)
	}
	if value.ID != "saved_artifact" || value.Version != 1 {
		t.Fatalf("实际压缩丢失保存依据：%+v", value)
	}
	compacted := false
	for _, m := range chain {
		for _, item := range m.ResponseItems {
			var value struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(item, &value) == nil && value.Type == "compaction" {
				compacted = true
			}
		}
	}
	if !compacted {
		t.Fatal("实际请求没有经过原生压缩")
	}
	t.Log("真实 API 完成输入计数、原生压缩、继续生成，保留已保存作品 ID 与版本")
}
