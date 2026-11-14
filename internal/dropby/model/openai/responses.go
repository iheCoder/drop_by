package openai

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"

	"drop_by/internal/dropby/model"
	"drop_by/internal/dropby/upstream"
)

const Protocol = "openai_responses"

// Client 绑定到场景选中的接入点。原生参数只在本包解释。
type Client struct {
	HTTP                             *http.Client
	Provider, BaseURL, APIKey, Model string
	Options                          map[string]any
	Price                            Price
	MaxOutputTokens                  int
	CacheBreakpoints                 bool
	AllowLegacyHistory               bool
	cache                            *cacheRequest
}

// 高推理工具链需携带供应商返回的推理项目，不能只把工具结果拼成聊天文本。
// store=false + 加密推理项目让服务不依赖供应商保存会话；原始推理不显示给用户。
func (m *Client) Build(messages []model.ChatMessage, cacheEnd int, defs []model.ToolDefinition, jsonOutput bool, schema *model.OutputSchema) ([]byte, error) {
	messages = m.responsesHistory(messages, &cacheEnd)
	input := []any{}
	shortEnd := -1
	for i, msg := range messages {
		if len(msg.ResponseItems) > 0 {
			for _, item := range msg.ResponseItems {
				input = append(input, item)
			}
			continue
		}
		if i == cacheEnd {
			shortEnd = len(input)
		}
		if msg.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": msg.ToolCallID, "output": msg.Content})
			continue
		}
		input = append(input, map[string]any{"role": msg.Role, "content": msg.Content})
	}
	// 复制要组合的映射，避免并发调用改动启动时加载的原生参数。
	body := map[string]any{"model": m.Model, "max_output_tokens": m.MaxOutputTokens, "store": false, "include": []string{"reasoning.encrypted_content"}}
	maps.Copy(body, m.Options)
	if include, ok := m.Options["include"].([]any); ok && !slices.Contains(include, "reasoning.encrypted_content") {
		body["include"] = append(slices.Clone(include), "reasoning.encrypted_content")
	}
	if len(defs) > 0 {
		tools := []any{}
		for _, d := range defs {
			tools = append(tools, map[string]any{"type": "function", "name": d.Function.Name, "description": d.Function.Description, "parameters": d.Function.Parameters, "strict": false})
		}
		body["tools"] = tools
	}
	if jsonOutput {
		format := map[string]any{"type": "json_object"}
		if schema != nil {
			format = map[string]any{"type": "json_schema", "name": schema.Name, "schema": schema.Schema, "strict": true}
		}
		text := map[string]any{"format": format}
		if native, ok := m.Options["text"].(map[string]any); ok {
			maps.Copy(text, native)
		}
		body["text"] = text
	}

	longEnd := -1
	if cache := m.cache; cache != nil {
		var err error
		longEnd, err = cache.longBoundary(body, input, shortEnd)
		if err != nil {
			return nil, err
		}
		if cache.next.source == m.Source() && cache.next.responseID != "" {
			options := map[string]any{"comparison_response_id": cache.next.responseID}
			if native, ok := m.Options["prompt_cache_options"].(map[string]any); ok {
				maps.Copy(options, native)
			}
			body["prompt_cache_options"] = options
		}
	}
	// 默认缓存仍可覆盖同一轮续接。长线与短线重合时，一个 12 小时标记即可。
	if m.CacheBreakpoints && shortEnd >= 0 && shortEnd != longEnd {
		markResponseCache(input[shortEnd], "")
	}
	if longEnd >= 0 {
		markResponseCache(input[longEnd], "12h")
	}
	body["input"] = input
	return json.Marshal(body)
}

func markResponseCache(raw any, ttl string) {
	item := raw.(map[string]any)
	field := "content"
	if item["type"] == "function_call_output" {
		field = "output"
	}
	marker := map[string]string{"mode": "explicit"}
	if ttl != "" {
		marker["ttl"] = ttl
	}
	item[field] = []map[string]any{{"type": "input_text", "text": item[field], "prompt_cache_breakpoint": marker}}
}
func (m *Client) Generate(ctx context.Context, payload []byte) (model.Result, error) {
	var out responseEnvelope
	var raw string
	if err := upstream.PostJSON(ctx, m.HTTP, m.BaseURL+"/responses", m.APIKey, payload, &out, &raw); err != nil {
		return model.Result{Raw: raw}, err
	}
	result, err := m.decodeResponse(out)
	result.Raw = raw
	return result, err
}

type responseEnvelope struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	Usage             responseUsage     `json:"usage"`
	CacheDiagnostic   json.RawMessage   `json:"prompt_cache_diagnostics"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

func (m *Client) decodeResponse(out responseEnvelope) (model.Result, error) {
	var err error
	result := model.Result{ID: out.ID, CacheDiagnostic: out.CacheDiagnostic}
	msg := model.ChatMessage{Role: "assistant", ResponseItems: out.Output, NativeSource: m.Source(), OutputTokens: out.Usage.OutputTokens}
	finalText, unphasedText := "", ""
	hasFinal := false
	for _, rawItem := range out.Output {
		var item struct {
			Type, Name, Arguments, Phase string
			CallID                       string `json:"call_id"`
			Content                      []struct{ Type, Text string }
		}
		if err = json.Unmarshal(rawItem, &item); err != nil {
			return result, err
		}
		switch item.Type {
		case "message":
			// 新模型可能同时返回 commentary 与 final_answer。中间说明不是
			// 业务 JSON，不能拼接成最终答案；原始项目仍完整保留用于下一轮续接。
			if item.Phase == "final_answer" {
				hasFinal = true
			}
			for _, part := range item.Content {
				if part.Type == "output_text" {
					if item.Phase == "final_answer" {
						finalText += part.Text
					} else if item.Phase == "" {
						unphasedText += part.Text
					}
				}
			}
		case "function_call":
			call := model.ToolCall{ID: item.CallID, Type: "function"}
			call.Function.Name = item.Name
			call.Function.Arguments = item.Arguments
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	msg.Content = unphasedText
	if hasFinal {
		msg.Content = finalText
	}
	result.Message = msg
	result.Incomplete = out.Status == "incomplete"
	// HTTP 和 WebSocket 都经过这里。缺少 TTL 细分时沿用本次请求的长缓存标记。
	longCacheRequested := m.cache != nil && m.cache.longWrite
	m.Price.applyUsage(&result, out.Usage, longCacheRequested)
	if m.cache != nil {
		m.cache.writeDetails = out.Usage.InputDetails.WriteDetails
	}
	return result, nil
}
