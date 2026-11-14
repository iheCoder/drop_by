package openai

import (
	"encoding/json"
	"testing"
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
			price, err := PriceForModel(tc.model)
			if err != nil {
				t.Fatal(err)
			}
			var u responseUsage
			if err = json.Unmarshal([]byte(`{"input_tokens":1000,"output_tokens":100,"input_tokens_details":{"cached_tokens":400,"cache_write_tokens":200},"output_tokens_details":{"reasoning_tokens":60}}`), &u); err != nil {
				t.Fatal(err)
			}
			cost, err := price.costMicros(u, false)
			if err != nil || cost != tc.short {
				t.Fatalf("默认缓存写入重复/漏计: %d, want %d, %v", cost, tc.short, err)
			}
			_ = json.Unmarshal([]byte(`{"input_tokens":1000,"output_tokens":100,"input_tokens_details":{"cached_tokens":400,"cache_write_tokens":200,"cache_write_tokens_details":{"ttl_30m_tokens":100,"ttl_12h_tokens":100}}}`), &u)
			cost, err = price.costMicros(u, false)
			if err != nil || cost != tc.long {
				t.Fatalf("缓存保留时长未正确计价: %d, want %d, %v", cost, tc.long, err)
			}
		})
	}
	// 272K 本身不加价，超过时整个请求按长上下文价格；不是只收超出的部分。
	price, _ := PriceForModel("gpt-6.1-sol")
	for _, tc := range []struct {
		input int
		want  int64
	}{{272000, 545000}, {272001, 1089504}} {
		cost, err := price.costMicros(responseUsage{InputTokens: tc.input, OutputTokens: 100}, false)
		if err != nil || cost != tc.want {
			t.Fatalf("长输入边界错误: %d -> %d, %v", tc.input, cost, err)
		}
	}
}

func TestPricingRejectsUnknownModelsAndInvalidUsage(t *testing.T) {
	// 未配置的模型不能先收费再猜费率；非法缓存分布不能造成负费用或释放预留。
	if _, err := PriceForModel("not-configured"); err == nil {
		t.Fatal("未知模型沿用了隐式费率")
	}
	price, _ := PriceForModel("gpt-6-luna")
	var usage responseUsage
	_ = json.Unmarshal([]byte(`{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":30}}`), &usage)
	if _, err := price.costMicros(usage, false); err == nil {
		t.Fatal("缓存总数超出输入仍被计费")
	}
	_ = json.Unmarshal([]byte(`{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":30,"cache_write_tokens_details":{"ttl_30m_tokens":10,"ttl_12h_tokens":10}}}`), &usage)
	if _, err := price.costMicros(usage, false); err == nil {
		t.Fatal("缓存时长细分缺失仍被计费")
	}
}

func TestLongCacheCostWithMissingDurationBreakdown(t *testing.T) {
	price, _ := PriceForModel("gpt-6.1-sol")
	u := responseUsage{InputTokens: 2000}
	u.InputDetails.CachedTokens, u.InputDetails.CacheWriteTokens = 1000, 1000
	// 网关仅给出写入总量时，长标记可能覆盖这 1000 token。读中部分仍按读取价。
	got, err := price.costMicros(u, true)
	if err != nil || got != 5100 {
		t.Fatalf("长标记缺时长明细应保守估算 $0.0051，实际 %d 微美元 (%v)", got, err)
	}
}
