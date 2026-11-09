package dropby

import "fmt"

// 2026-11-07 核对的官方 Standard 费率，只维护当前使用的三个模型。
// 来源：https://developers.openai.com/api/docs/models/gpt-6-luna
// https://developers.openai.com/api/docs/models/gpt-6.1-terra
// https://developers.openai.com/api/docs/models/gpt-6.1-sol
// 单位为纳美元/token；最后统一向上取整到微美元，避免浮点累计与重复取整。
type modelPrice struct{ input, cachedInput, output int64 }

func priceForModel(model string) (modelPrice, error) {
	switch model {
	case "gpt-6-luna":
		return modelPrice{100, 10, 500}, nil
	case "gpt-6.1-terra":
		return modelPrice{1000, 50, 5000}, nil
	case "gpt-6.1-sol":
		return modelPrice{2000, 100, 10000}, nil
	default:
		return modelPrice{}, fmt.Errorf("模型 %q 尚未配置费率，未发起收费调用", model)
	}
}

// 超过 272K 输入时，整次请求的输入/缓存费率翻倍，输出为 1.5 倍。
// 当前默认上下文小于这个边界，但用户已有可调上下文配置，不能漏计跨界请求。
func (p modelPrice) forInput(tokens int) modelPrice {
	if tokens > 272000 {
		p.input *= 2
		p.cachedInput *= 2
		p.output = p.output * 3 / 2
	}
	return p
}

// 预留时不知道缓存命中与写入分布，按全部输入发生最高写入费率估算。
// 生成、压缩共享这条规则；响应失败或缺少用量时保留预留，不能当成免费调用。
func (p modelPrice) reserveMicros(input, output int) int64 {
	p = p.forInput(input)
	return (int64(input)*p.input*5/2 + int64(output)*p.output + 999) / 1000
}

type responseUsage struct {
	longCacheRequested bool
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputDetails       struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
		WriteDetails     *struct {
			ThirtyMinutes int `json:"ttl_30m_tokens"`
			TwelveHours   int `json:"ttl_12h_tokens"`
		} `json:"cache_write_tokens_details"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u responseUsage) costMicros(p modelPrice) (int64, error) {
	cached, written := u.InputDetails.CachedTokens, u.InputDetails.CacheWriteTokens
	if u.InputTokens < 0 || u.OutputTokens < 0 || cached < 0 || written < 0 || cached > u.InputTokens || written > u.InputTokens-cached {
		return 0, fmt.Errorf("模型返回的 token 用量不一致，保留本次预算预留")
	}
	// 有长标记却缺少时长细分时，按可能的最高写入价格保守结算。
	short, long := written, 0
	if detail := u.InputDetails.WriteDetails; detail != nil {
		short, long = detail.ThirtyMinutes, detail.TwelveHours
		if short < 0 || long < 0 || short > written || long != written-short {
			return 0, fmt.Errorf("缓存写入用量不一致，保留本次预算预留")
		}
	} else if u.longCacheRequested {
		short, long = 0, written
	}
	p = p.forInput(u.InputTokens)
	// 缓存读取、写入和普通输入互斥。输出总量已含 reasoning tokens，不能再加一次。
	nanos := int64(u.InputTokens-cached-written)*p.input + int64(cached)*p.cachedInput + int64(short)*(p.input*5/4) + int64(long)*(p.input*5/2) + int64(u.OutputTokens)*p.output
	return (nanos + 999) / 1000, nil
}

func (m *HTTPModel) responseCost(usage responseUsage, price modelPrice) (int64, error) {
	usage.longCacheRequested = m.longCacheRequested
	return usage.costMicros(price)
}
