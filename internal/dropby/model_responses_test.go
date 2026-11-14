package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			cfg.editScenes([]modelScene{sceneMemberSettings}, func(s *modelSettings) {
				s.Model = tc.model
				s.setReasoning(tc.reasoning)
			})
			cfg.setModelEndpoint(server.URL)
			model := HTTPModel{Config: cfg, Store: s, Client: server.Client()}
			result, err := model.Complete(context.Background(), []ChatMessage{{Role: "user", Content: "Reply OK"}}, nil, false)
			if err != nil || result.Content != "OK" {
				t.Fatalf("请求未完成: %+v %v", result, err)
			}
		})
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
	cfg.editScenes([]modelScene{sceneMemberSettings}, func(s *modelSettings) { s.Model = "gpt-6-luna"; s.setReasoning("none") })
	cfg.setModelEndpoint(server.URL)
	model := HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	result, err := model.Complete(context.Background(), []ChatMessage{{Role: "user", Content: "返回判断 JSON"}}, nil, true)
	if err != nil || result.Content != `{"action":"pass"}` || len(result.ResponseItems) != 2 {
		t.Fatalf("阶段处理破坏最终答案或续接项目: %+v %v", result, err)
	}
}
