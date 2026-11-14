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
	cfg.setModelEndpoint(server.URL)
	cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.Model = "gpt-6.1-sol"; s.setReasoning("high") })
	cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.ContextTokenBudget = 2000 })
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
	profile := "我喜欢亲手试出问题在哪里，也会主动分享有意思的发现。"
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
			if strings.Contains(body, profile) || !strings.Contains(body, "刚交付的原话") || strings.Contains(body, `"instructions":`) {
				t.Error("压缩应处理已交付的原话，当前人格单独保留，沿用供应商默认续接")
			}
			if !strings.Contains(body, "saved_call") || !strings.Contains(body, "saved_artifact") || !strings.Contains(body, "已保存的完整作品") {
				t.Error("压缩输入必须包含完整调用和已保存结果")
			}
			json.NewEncoder(w).Encode(map[string]any{"output": returned, "usage": map[string]any{"input_tokens": 9000, "output_tokens": 40, "input_tokens_details": map[string]any{"cached_tokens": 3000, "cache_write_tokens": 2000,
				"cache_write_tokens_details": map[string]int{"ttl_30m_tokens": 1000, "ttl_12h_tokens": 1000}}}})
		case "/responses":
			generations++
			if strings.Count(body, profile) != 1 {
				t.Error("压缩后人物说明缺失或重复")
			}
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
	cfg.setModelEndpoint(server.URL)
	cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.Model = "gpt-6.1-sol"; s.setReasoning("high") })
	cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.ContextTokenBudget = 3000 })
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	chain := largeArtifactChain()
	chain[0].Content += "\n" + profile
	scene := ChatMessage{Role: "user", Content: asJSON(map[string]any{"self": map[string]any{"profile_revision": 3}, "trigger": map[string]string{"id": "work"}, "time": now(), "latest_messages": []Message{{ID: "scene_msg", Body: "刚交付的原话"}}, "new_message_ids": []string{"scene_msg"}})}
	chain = append(chain[:1], append([]ChatMessage{scene}, chain[1:]...)...)
	if _, err := completeModel(context.Background(), model, &chain, nil, false); err != nil {
		t.Fatal(err)
	}
	if len(chain) != 3 || chain[0].Content != "身份与当前目标必须保留\n"+profile || len(chain[1].ResponseItems) != 2 || strings.Contains(chain[2].Content, "刚交付的原话") {
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
	// 压缩为 $0.0162（1000 个短缓存写，1000 个长缓存写），两次生成各 $0.0014；缓存输入不能再按普通输入收费。
	if usage.Calls != 3 || usage.InputTokens != 10200 || usage.CostMicros != 19000 {
		t.Fatalf("压缩和生成都应结算，计数不当作生成: %+v", usage)
	}
	start, end := budgetWeek(time.Now())
	report, err := s.readUsageReport(context.Background(), start, end, time.Now())
	if err != nil || len(report.Entries) != 3 || report.Entries[2].Purpose != "compact" || report.Entries[2].CostMicros != 16200 {
		t.Fatalf("应能单独查到压缩费用，计数不进入明细: %+v %v", report, err)
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
	cfg.setModelEndpoint(server.URL)
	cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.Model = "gpt-6.1-sol"; s.setReasoning("high") })
	cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.ContextTokenBudget = 2000 })
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	chain := largeArtifactChain()
	_, err := completeModel(context.Background(), model, &chain, nil, false)
	if errors.Is(err, ErrContextCapacity) || err == nil || len(chain) != 4 {
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
	cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.ContextTokenBudget = 8000; s.setOutputLimit(2000); s.setReasoning("high") })
	cfg.WeeklyBudgetMicros = 900000
	model := &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 3 * time.Minute}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	chain := largeArtifactChain()
	chain[0].Content = "这是接口兼容性测试。你是记录员，目标是保留已经完成的作品证据。只返回 JSON，不调用工具。"
	chain[1].Content = "已有作品无需再做。请在 JSON 中写出已保存的 artifact_id、version，以及刚读回的 tail。"
	// 旧作品需要原生压缩，刚回来的页尾则应在下一次真实生成里原样可见。
	chain = append(chain,
		ChatMessage{Role: "assistant", ResponseItems: []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"latest_read","name":"read_tool_result","arguments":"{\"id\":\"latest\"}"}`)}},
		ChatMessage{Role: "tool", ToolCallID: "latest_read", Content: `{"tool_run_id":"latest","status":"succeeded","result":{"tail":"版次核对完毕-39"}}`, PendingDelivery: true})
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
		Tail    string `json:"tail"`
	}
	if err := parseJSON(result.Content, &value); err != nil {
		t.Fatal(err)
	}
	if value.ID != "saved_artifact" || value.Version != 1 || value.Tail != "版次核对完毕-39" {
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
	t.Log("真实 API 完成计数、原生压缩和续接，保留已保存作品的 ID、版本与新读取的页尾")
}
