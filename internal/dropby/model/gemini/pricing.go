package gemini

import (
	"fmt"
	"math"
	"time"

	"drop_by/internal/dropby/model"
)

// Google Standard 文本费率（纳美元/token），与实际接入地址无关。
// https://aistudio.google.com/docs/pricing
type Price struct{ input, cachedInput, output int64 }

// 同一次调用的预留和结算共用它开始时选定的费率。
// Google 只公布生效日期，以 UTC 日历日作为本地估算的分界。
func PriceForModel(name string, at time.Time) (Price, error) {
	switch name {
	case Hydrogen:
		return Price{1500, 75, 7500}, nil
	case Argon, Carbon:
		if at.Before(time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC)) {
			return Price{2000, 100, 10000}, nil
		}
		return Price{4000, 200, 20000}, nil
	case Flash:
		if at.Before(time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)) {
			return Price{750, 75, 3750}, nil
		}
		return Price{1500, 150, 7500}, nil
	case FlashLite:
		return Price{300, 30, 2500}, nil
	}
	return Price{}, fmt.Errorf("Gemini 模型 %q 尚未配置费率", name)
}

func (p Price) ReserveMicros(input, output int) int64 {
	return p.costMicros(int64(input), 0, int64(output))
}

func (p Price) costMicros(input, cached, output int64) int64 {
	nanos := (input-cached)*p.input + cached*p.cachedInput + output*p.output
	return (nanos + 999) / 1000
}

type usageMetadata struct {
	Prompt     *int64 `json:"promptTokenCount"`
	Cached     int64  `json:"cachedContentTokenCount"`
	Candidates int64  `json:"candidatesTokenCount"`
	ToolPrompt int64  `json:"toolUsePromptTokenCount"`
	Thoughts   *int64 `json:"thoughtsTokenCount"`
	Total      *int64 `json:"totalTokenCount"`
}

func (p Price) applyUsage(result *model.Result, u *usageMetadata) {
	if u == nil || u.Prompt == nil || (u.Total == nil && u.Thoughts == nil) {
		// Gemini 的思考量可能包含在总数中。如果两个字段都缺失，
		// 只有候选文本的数量不足以确认完整输出费用。
		return // 账本会保留预留，等待核实这笔用量。
	}
	thoughts := int64(0)
	if u.Thoughts != nil {
		thoughts = *u.Thoughts
	}
	prompt := *u.Prompt
	for _, count := range []int64{prompt, u.Cached, u.Candidates, u.ToolPrompt, thoughts} {
		if count < 0 || count > math.MaxInt32 {
			result.CostError = fmt.Errorf("Gemini 返回的 token 数无效，保留本次预算预留")
			return
		}
	}
	input := prompt + u.ToolPrompt
	output := u.Candidates + thoughts
	if u.Cached > prompt || (u.Total != nil && *u.Total != input+output) {
		result.CostError = fmt.Errorf("Gemini 返回的 token 用量不一致，保留本次预算预留")
		return
	}
	// 缓存命中是输入的子集。Gemini 将思考量与候选文本分开报告，
	// 统一账本的输出总量须包含两者，思考量也单独显示。
	result.Usage.InputTokens, result.Usage.OutputTokens = int(input), int(output)
	result.Usage.InputDetails.CachedTokens = int(u.Cached)
	if u.Thoughts != nil {
		value := int(thoughts)
		result.Usage.OutputDetails.ReasoningTokens = &value
	}
	result.CostMicros = p.costMicros(input, u.Cached, output)
}
