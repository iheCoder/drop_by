package openai

import (
	"context"
	"encoding/json"
	"fmt"

	"drop_by/internal/dropby/model"
	"drop_by/internal/dropby/upstream"
)

func (m *Client) CountBody(raw []byte) ([]byte, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	// 只发送计数端点的参数。场景可以配置生成专用扩展，不能把它们
	// 原样送到另一个端点，导致正常的容量检查在长输入时失败。
	countBody := make(map[string]json.RawMessage)
	for _, key := range []string{"model", "input", "instructions", "reasoning", "text", "tool_choice", "tools", "parallel_tool_calls", "truncation", "personality"} {
		if value, ok := body[key]; ok {
			countBody[key] = value
		}
	}
	return json.Marshal(countBody)
}

func (m *Client) Count(ctx context.Context, payload []byte) (count int, raw string, err error) {
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	err = upstream.PostJSON(ctx, m.HTTP, m.BaseURL+"/responses/input_tokens", m.APIKey, payload, &out, &raw)
	if err == nil && out.InputTokens <= 0 {
		err = fmt.Errorf("输入容量计数未返回有效结果")
	}
	return out.InputTokens, raw, err
}

func (m *Client) CompactBody(raw []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"model": m.Model, "input": request["input"]})
}

// 保留完整原生压缩项目。没有可续接的项目时仍返回用量供账本结算。
func (m *Client) Compact(ctx context.Context, payload []byte) (result model.Result, err error) {
	var out responseEnvelope
	if err = upstream.PostJSON(ctx, m.HTTP, m.BaseURL+"/responses/compact", m.APIKey, payload, &out, &result.Raw); err != nil {
		return result, err
	}
	m.Price.applyUsage(&result, out.Usage, false)
	for _, item := range out.Output {
		var value struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &value) == nil && value.Type == "compaction" {
			result.Message = model.ChatMessage{Role: "user", ResponseItems: out.Output, OutputTokens: out.Usage.OutputTokens, NativeSource: m.Source()}
			break
		}
	}
	return result, nil
}
