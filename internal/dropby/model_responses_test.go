package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// 三个正在使用的模型统一发 Responses 请求；检查实际路径、现代参数和结果。
// 不为未使用的型号或旧提供商建立兼容用例。
func TestSelectedModelsUseResponses(t *testing.T) {
	for _, tc := range []struct{ model, reasoning string }{
		{"gpt-6-luna", "none"}, {"gpt-6.1-terra", "none"}, {"gpt-6.1-sol", "low"}, {"gpt-6.1-sol", "high"},
	} {
		t.Run(tc.model+"/"+tc.reasoning, func(t *testing.T) {
			s, _, _ := fixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" {
					t.Errorf("请求走错接口: %s", r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body["model"] != tc.model {
					t.Error("选定模型未传递")
				}
				if body["reasoning"].(map[string]any)["effort"] != tc.reasoning {
					t.Error("推理强度未传递")
				}
				if body["max_output_tokens"] != float64(400) {
					t.Error("输出额度未传递")
				}
				for _, key := range []string{"max_tokens", "max_completion_tokens", "reasoning_effort", "response_format"} {
					if _, exists := body[key]; exists {
						t.Errorf("Responses 混入旧参数 %s", key)
					}
				}
				w.Write([]byte(`{"status":"completed","output":[{"type":"message","phase":"final_answer","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`))
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.Model = tc.model
			cfg.Reasoning = tc.reasoning
			cfg.BaseURL = server.URL
			model := HTTPModel{Config: cfg, Store: s, Client: server.Client()}
			result, err := model.Complete(context.Background(), []ChatMessage{{Role: "user", Content: "Reply OK"}}, nil, false)
			if err != nil || result.Content != "OK" {
				t.Fatalf("请求未完成: %+v %v", result, err)
			}
		})
	}
}

// 同一个 HTTPModel 可以同时处理判断、聊天和研究；职责配置只属于当前请求。
// 特别检查普通后续发言不会因为旧的深入标记而误用研究模型。
func TestModelRolesStayWithRequest(t *testing.T) {
	s, _, _ := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		input := body["input"].([]any)
		expected := input[0].(map[string]any)["content"].(string)
		actual := body["model"].(string) + "/" + body["reasoning"].(map[string]any)["effort"].(string)
		if r.URL.Path != "/responses" || actual != expected {
			t.Errorf("请求配置串线: %s %s，期望 %s", r.URL.Path, actual, expected)
		}
		if _, exists := body["max_tokens"]; exists {
			t.Error("Responses 混入旧参数")
		}
		w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.BaseURL = server.URL
	cfg.Model = "gpt-6.1-sol"
	cfg.ChatModel = "gpt-6.1-terra"
	cfg.Reasoning = "low"
	cfg.DeepReasoning = "high"
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	var wg sync.WaitGroup
	for _, tc := range []struct {
		role, expected string
		deep           bool
	}{
		{"chat", "gpt-6.1-terra/none", false},
		{"chat", "gpt-6.1-terra/none", true}, {"", "gpt-6.1-sol/high", true}, {"", "gpt-6.1-sol/low", false},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), modelRoleKey{}, tc.role)
			ctx = context.WithValue(ctx, deepReasoningKey{}, tc.deep)
			if _, err := model.Complete(ctx, []ChatMessage{{Role: "user", Content: tc.expected}}, nil, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if model.Config.Model != cfg.Model || model.Config.Reasoning != cfg.Reasoning {
		t.Fatal("共享模型配置被覆盖")
	}
}

// 真实 6 系列响应出现过 commentary 与 final_answer 同时返回；不能把
// 中间说明拼进判断 JSON，但所有项目必须留在 ResponseItems 供原生续接。
func TestResponsesUsesFinalAnswerPhase(t *testing.T) {
	s, _, _ := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"completed","output":[{"type":"message","phase":"commentary","content":[{"type":"output_text","text":"中间说明，不是判断 JSON"}]},{"type":"message","phase":"final_answer","content":[{"type":"output_text","text":"{\"action\":\"pass\"}"}]}],"usage":{"input_tokens":10,"output_tokens":10}}`))
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Model = "gpt-6-luna"
	cfg.Reasoning = "none"
	cfg.BaseURL = server.URL
	model := HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	result, err := model.Complete(context.Background(), []ChatMessage{{Role: "user", Content: "返回判断 JSON"}}, nil, true)
	if err != nil || result.Content != `{"action":"pass"}` || len(result.ResponseItems) != 2 {
		t.Fatalf("阶段处理破坏最终答案或续接项目: %+v %v", result, err)
	}
}
