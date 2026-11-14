package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"drop_by/internal/dropby/model"
	"drop_by/internal/dropby/upstream"
)

type Client struct {
	HTTP                             *http.Client
	Provider, BaseURL, APIKey, Model string
	Options                          map[string]any
	Price                            Price
	LocalTokenCounting               bool
}

const apiKeyHeader = "x-goog-api-key"

func (m *Client) endpoint(method string) string {
	return strings.TrimSuffix(m.BaseURL, "/v1beta") + "/v1beta/models/" + m.Model + ":" + method
}

func (m *Client) Build(messages []model.ChatMessage, _ int, defs []model.ToolDefinition, jsonOutput bool, schema *model.OutputSchema) ([]byte, error) {
	contents, system, err := m.history(messages)
	if err != nil {
		return nil, err
	}
	body := make(map[string]any, len(m.Options)+4)
	maps.Copy(body, m.Options)
	body["contents"] = contents
	if len(system) > 0 {
		body["systemInstruction"] = content{Parts: system}
	}

	// 复用本地工具的 JSON Schema，确保可声明的字段与执行器收到的参数一致。
	if len(defs) > 0 {
		declarations := make([]any, 0, len(defs))
		for _, def := range defs {
			declarations = append(declarations, map[string]any{
				"name": def.Function.Name, "description": def.Function.Description,
				"parametersJsonSchema": def.Function.Parameters,
			})
		}
		body["tools"] = []map[string]any{{"functionDeclarations": declarations}}
	} else {
		delete(body, "toolConfig") // 此场景内的独立 JSON 调用可能没有可用工具。
	}
	if jsonOutput {
		// 调用方的 JSON 内容由不同业务结构消费。这里只要求对象，保留
		// 各业务自己定义的字段。Gemini 原生工具可以和结构化输出同用。
		var shape any = map[string]any{"type": "object", "additionalProperties": true}
		if schema != nil {
			shape = schema.Schema
		}
		config := map[string]any{"responseFormat": map[string]any{"text": map[string]any{
			"mimeType": "APPLICATION_JSON", "schema": shape,
		}}}
		native, _ := m.Options["generationConfig"].(map[string]any)
		maps.Copy(config, native)
		body["generationConfig"] = config
	}
	return json.Marshal(body)
}

func (m *Client) Estimate(messages []model.ChatMessage, encode func([]model.ChatMessage) ([]byte, error)) (int, error) {
	// Google 将回传的 thoughtSignature 计入输入。用完整请求作保守估算，
	// 只有超出本地窗口时，才经 countTokens 核对实际容量。
	raw, err := encode(messages)
	if err == nil && m.LocalTokenCounting {
		return estimateRequestTokens(raw)
	}
	return len(raw), err
}

func (m *Client) CountBody(raw []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	// 系统提示和函数声明也占上下文；生成选项不属于这次计数的输入。
	input := map[string]any{"model": "models/" + m.Model}
	for _, name := range []string{"contents", "systemInstruction", "tools"} {
		if value, ok := request[name]; ok {
			input[name] = value
		}
	}
	return json.Marshal(map[string]any{"generateContentRequest": input})
}

func (m *Client) Count(ctx context.Context, payload []byte) (count int, raw string, err error) {
	var out struct{ TotalTokens int }
	err = upstream.PostJSONWithAPIKey(ctx, m.HTTP, m.endpoint("countTokens"), apiKeyHeader, m.APIKey, payload, &out, &raw)
	if err == nil && out.TotalTokens <= 0 {
		err = fmt.Errorf("Gemini 未返回有效的输入容量计数")
	}
	return out.TotalTokens, raw, err
}

func (m *Client) Generate(ctx context.Context, payload []byte) (result model.Result, err error) {
	var out struct {
		ResponseID string
		Candidates []struct {
			Content      json.RawMessage
			FinishReason string
		}
		UsageMetadata *usageMetadata
	}
	if err = upstream.PostJSONWithAPIKey(ctx, m.HTTP, m.endpoint("generateContent"), apiKeyHeader, m.APIKey, payload, &out, &result.Raw); err != nil {
		var failure *upstream.Failure
		if errors.As(err, &failure) && failure.Status == http.StatusBadRequest {
			detail := strings.ToLower(failure.Detail)
			if strings.Contains(detail, "the input token count exceeds the maximum number of tokens allowed") ||
				(strings.Contains(detail, "the input token count") && strings.Contains(detail, "exceeds the maximum allowed")) {
				return result, fmt.Errorf("%w：%w", model.ErrInputTooLarge, err)
			}
		}
		return result, err
	}
	result.ID = out.ResponseID
	m.Price.applyUsage(&result, out.UsageMetadata)
	// 截断、过滤或不完整的调用可能已经产生用量，先让账本结算，
	// 再由业务拒绝执行不完整的工具或发布被截断的聊天。
	if len(out.Candidates) != 1 || out.Candidates[0].FinishReason != "STOP" {
		result.Incomplete = true
		return result, nil
	}
	var decodeErr error
	result.Message, decodeErr = m.decodeContent(out.Candidates[0].Content, result.Usage.OutputTokens)
	result.Incomplete = decodeErr != nil
	return result, nil
}

func (m *Client) decodeContent(raw json.RawMessage, outputTokens int) (model.ChatMessage, error) {
	var turn content
	if err := json.Unmarshal(raw, &turn); err != nil || turn.Role != "model" || len(turn.Parts) == 0 {
		return model.ChatMessage{}, fmt.Errorf("Gemini 未返回有效的模型消息")
	}
	msg := model.ChatMessage{Role: "assistant", NativeSource: m.source(), ResponseItems: []json.RawMessage{raw}, OutputTokens: outputTokens}
	var text strings.Builder
	seen := make(map[string]bool)
	for _, raw := range turn.Parts {
		var part struct {
			Text         string
			Thought      bool
			FunctionCall *struct {
				ID, Name string
				Args     map[string]json.RawMessage
			}
		}
		if err := json.Unmarshal(raw, &part); err != nil {
			return model.ChatMessage{}, err
		}
		if part.Thought {
			continue // 思考摘要只留在完整原生消息中，不能混进业务 JSON。
		}
		text.WriteString(part.Text)
		if f := part.FunctionCall; f != nil {
			if f.ID == "" || f.Name == "" || seen[f.ID] {
				return model.ChatMessage{}, fmt.Errorf("Gemini 工具调用缺少唯一 ID 或函数名")
			}
			seen[f.ID] = true
			args := []byte("{}")
			if f.Args != nil {
				args, _ = json.Marshal(f.Args)
			}
			call := model.ToolCall{ID: f.ID, Type: "function"}
			call.Function.Name, call.Function.Arguments = f.Name, string(args)
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	msg.Content = text.String()
	return msg, nil
}
