package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 同一份用量包含普通输入、缓存读取、缓存写入与输出；人工算出的美元金额
// 是预期依据，不能仅检查“费用不同”而漏掉重复收取缓存或推理 tokens。
func TestSelectedModelUsagePrices(t *testing.T) {
	for _, tc := range []struct {
		model       string
		short, long int64
	}{
		{"gpt-6-luna", 119, 132}, {"gpt-6.1-terra", 1170, 1295}, {"gpt-6.1-sol", 2340, 2590},
	} {
		t.Run(tc.model, func(t *testing.T) {
			price, err := priceForModel(tc.model)
			if err != nil {
				t.Fatal(err)
			}
			var u responseUsage
			if err = json.Unmarshal([]byte(`{"input_tokens":1000,"output_tokens":100,"input_tokens_details":{"cached_tokens":400,"cache_write_tokens":200},"output_tokens_details":{"reasoning_tokens":60}}`), &u); err != nil {
				t.Fatal(err)
			}
			cost, err := u.costMicros(price)
			if err != nil || cost != tc.short {
				t.Fatalf("默认缓存写入重复/漏计: %d, want %d, %v", cost, tc.short, err)
			}
			_ = json.Unmarshal([]byte(`{"input_tokens":1000,"output_tokens":100,"input_tokens_details":{"cached_tokens":400,"cache_write_tokens":200,"cache_write_tokens_details":{"ttl_30m_tokens":100,"ttl_12h_tokens":100}}}`), &u)
			cost, err = u.costMicros(price)
			if err != nil || cost != tc.long {
				t.Fatalf("缓存保留时长未正确计价: %d, want %d, %v", cost, tc.long, err)
			}
		})
	}
	// 272K 本身不加价，超过时整个请求按长上下文价格；不是只收超出的部分。
	price, _ := priceForModel("gpt-6.1-sol")
	for _, tc := range []struct {
		input int
		want  int64
	}{{272000, 545000}, {272001, 1089504}} {
		cost, err := (responseUsage{InputTokens: tc.input, OutputTokens: 100}).costMicros(price)
		if err != nil || cost != tc.want {
			t.Fatalf("长输入边界错误: %d -> %d, %v", tc.input, cost, err)
		}
	}
}

// 真正经过角色选择、HTTP 响应解析、预留与 SQLite 结算。相同输入输出
// 应依照实际发出的模型计价，不能一直使用主模型 Sol 的价，也不能清空旧账。
func TestRoleModelsSettleTheirOwnPrices(t *testing.T) {
	for _, tc := range []struct {
		role, model string
		want        int64
	}{
		{"chat", "gpt-6.1-terra", 1170}, {"", "gpt-6.1-sol", 2340},
	} {
		t.Run(tc.model, func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg := testConfig()
			cfg.Model = "gpt-6.1-sol"
			cfg.ChatModel = "gpt-6.1-terra"
			cfg.Reasoning = "low"
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
			cfg.BaseURL = server.URL
			model := HTTPModel{Config: cfg, Store: s, Client: server.Client()}
			ctx := context.WithValue(context.Background(), modelRoleKey{}, tc.role)
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

func TestPricingRejectsUnknownModelsAndInvalidUsage(t *testing.T) {
	// 未配置的模型不能先收费再猜费率；非法缓存分布不能造成负费用或释放预留。
	if _, err := priceForModel("not-configured"); err == nil {
		t.Fatal("未知模型沿用了隐式费率")
	}
	price, _ := priceForModel("gpt-6-luna")
	var usage responseUsage
	_ = json.Unmarshal([]byte(`{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":30}}`), &usage)
	if _, err := usage.costMicros(price); err == nil {
		t.Fatal("缓存总数超出输入仍被计费")
	}
	_ = json.Unmarshal([]byte(`{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":30,"cache_write_tokens_details":{"ttl_30m_tokens":10,"ttl_12h_tokens":10}}}`), &usage)
	if _, err := usage.costMicros(price); err == nil {
		t.Fatal("缓存时长细分缺失仍被计费")
	}
}

func TestFailedOrUnmeteredCallKeepsModelReservation(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg := testConfig()
			cfg.Model = "gpt-6-luna"
			var reserved int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				usage, err := s.Usage()
				if err != nil {
					t.Error(err)
				}
				reserved = usage.CostMicros
				// HTTP 200 但没有 usage 也不能当作零成本；未知计费保留原预留。
				respond(w, status, map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": "OK"}}}}})
			}))
			defer server.Close()
			cfg.BaseURL = server.URL
			model := HTTPModel{Config: cfg, Store: s, Client: server.Client()}
			_, err := model.Complete(context.Background(), []ChatMessage{{Role: "user", Content: "Reply OK"}}, nil, false)
			if (err != nil) != (status == 503) {
				t.Fatalf("响应状态处理错误: %v", err)
			}
			usage, _ := s.Usage()
			if reserved <= 0 || usage.CostMicros != reserved || usage.InputTokens != 0 || usage.OutputTokens != 0 {
				t.Fatalf("未知计费被错误释放: reserved=%d usage=%+v", reserved, usage)
			}
		})
	}
}
