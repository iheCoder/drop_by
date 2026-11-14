package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 真正经过角色选择、HTTP 响应解析、预留与 SQLite 结算。相同输入输出
// 应依照实际发出的模型计价，不能一直使用主模型 Sol 的价，也不能清空旧账。
func TestRoleModelsSettleTheirOwnPrices(t *testing.T) {
	for _, tc := range []struct {
		scene modelScene
		model string
		want  int64
	}{
		{sceneChat, "gpt-6.1-terra", 1170}, {sceneMemberSettings, "gpt-6.1-sol", 2340},
	} {
		t.Run(tc.model, func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg := testConfig()
			cfg.editScenes([]modelScene{sceneMemberSettings}, func(s *modelSettings) { s.Model = "gpt-6.1-sol"; s.setReasoning("low") })
			day, err := s.Reserve(cfg, 100)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Settle(day, 100, 100, 7, 3); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["model"] != tc.model {
					t.Errorf("请求模型不符: %v", body["model"])
				}
				respond(w, 200, map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": "OK"}}}}, "usage": map[string]any{"input_tokens": 1000, "output_tokens": 100, "input_tokens_details": map[string]int{"cached_tokens": 400, "cache_write_tokens": 200}, "output_tokens_details": map[string]int{"reasoning_tokens": 60}}})
			}))
			defer server.Close()
			cfg.setModelEndpoint(server.URL)
			model := HTTPModel{Config: cfg, Store: s, Client: server.Client()}
			ctx := context.WithValue(context.Background(), modelSceneKey{}, tc.scene)
			ctx = context.WithValue(ctx, autonomyKey{}, "pricing-check")
			if _, err = model.Complete(ctx, []ChatMessage{{Role: "user", Content: "Reply OK"}}, nil, false); err != nil {
				t.Fatal(err)
			}
			usage, err := s.Usage()
			if err != nil {
				t.Fatal(err)
			}
			if usage.CostMicros != 100+tc.want || usage.AutonomousCostMicros != tc.want || usage.InputTokens != 1007 || usage.OutputTokens != 103 {
				t.Fatalf("总账/自主账/历史用量错误: %+v", usage)
			}
			// 响应的原生用量在逐笔明细中保留。推理是输出中的一部分，只显示一次。
			start, end := budgetWeek(time.Now())
			report, err := s.readUsageReport(ctx, start, end, time.Now())
			if err != nil || len(report.Entries) != 1 {
				t.Fatalf("请求应对应一笔明细: %+v %v", report, err)
			}
			entry := report.Entries[0]
			if entry.Model != tc.model || entry.CostMicros != tc.want || entry.CachedTokens != 400 || entry.CacheWrite != 200 ||
				entry.ReasoningTokens == nil || *entry.ReasoningTokens != 60 || !entry.Autonomous {
				t.Fatalf("模型与用量归属错误: %+v", entry)
			}
		})
	}
}

func TestFailedOrUnmeteredCallKeepsModelReservation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		badUsage bool
	}{
		{name: "missing usage", status: http.StatusOK},
		{name: "invalid cache write breakdown", status: http.StatusOK, badUsage: true},
		{name: "upstream unavailable", status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg := testConfig()
			cfg.editScenes([]modelScene{sceneMemberSettings}, func(s *modelSettings) { s.Model = "gpt-6-luna" })
			var reserved int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				usage, err := s.Usage()
				if err != nil {
					t.Error(err)
				}
				reserved = usage.CostMicros
				// HTTP 200 但没有 usage 也不能当作零成本；未知计费保留原预留。
				body := map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": "OK"}}}}}
				if tc.badUsage {
					// 用量互相矛盾时，接入包应把计价错误交给账本，已有预留保持未知。
					body["usage"] = json.RawMessage(`{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cache_write_tokens":30,"cache_write_tokens_details":{"ttl_30m_tokens":10,"ttl_12h_tokens":10}}}`)
				}
				respond(w, tc.status, body)
			}))
			defer server.Close()
			cfg.setModelEndpoint(server.URL)
			model := HTTPModel{Config: cfg, Store: s, Client: server.Client()}
			_, err := model.Complete(context.Background(), []ChatMessage{{Role: "user", Content: "Reply OK"}}, nil, false)
			if (err != nil) != (tc.status == http.StatusServiceUnavailable || tc.badUsage) {
				t.Fatalf("响应状态处理错误: %v", err)
			}
			usage, _ := s.Usage()
			if reserved <= 0 || usage.CostMicros != reserved || usage.InputTokens != 0 || usage.OutputTokens != 0 {
				t.Fatalf("未知计费被错误释放: reserved=%d usage=%+v", reserved, usage)
			}
		})
	}
}
