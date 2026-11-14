package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"drop_by/internal/dropby/model"
)

func (m *Client) Source() string {
	// 该值随原生项目持久化，用于确认能否续接。地址不写入消息，密钥不参与。
	sum := sha256.Sum256([]byte(Protocol + "\x00" + m.Provider + "\x00" + m.BaseURL + "\x00" + m.Model))
	return hex.EncodeToString(sum[:])
}

func (m *Client) responsesHistory(history []model.ChatMessage, cacheEnd *int) []model.ChatMessage {
	source := m.Source()
	var foreignCalls map[string]string
	var result []model.ChatMessage
	originalCacheEnd := *cacheEnd
	for i, msg := range history {
		if msg.Role == "assistant" {
			foreignCalls = nil
		}
		// 旧库没有来源标签，只让默认接入点沿用原有续接。新配置中的
		// 其他接入点只收到旧历史里的可见内容和工具事实。
		foreign := msg.NativeSource != source && (msg.NativeSource != "" || !m.AllowLegacyHistory)
		if foreign && len(msg.ResponseItems) > 0 {
			if result == nil {
				result = append(make([]model.ChatMessage, 0, len(history)), history[:i]...)
			}
			// 只改本次请求的表示。保留可见文本及工具事实，旧模型的加密
			// 推理和压缩状态不会进入这次请求，也不会覆盖原始记录。
			if msg.Content != "" {
				result = append(result, model.ChatMessage{Role: msg.Role, Content: msg.Content, PendingDelivery: msg.PendingDelivery})
			}
			if len(msg.ToolCalls) > 0 {
				foreignCalls = make(map[string]string, len(msg.ToolCalls))
			}
			for _, call := range msg.ToolCalls {
				foreignCalls[call.ID] = call.Function.Name
				result = append(result, model.ChatMessage{Role: "assistant", Content: "先前的工具调用：" + asJSON(call), PendingDelivery: msg.PendingDelivery})
			}
			// 压缩结果可能附有可直接复用的用户原文，逐条保留。
			if msg.Role == "user" {
				for _, raw := range msg.ResponseItems {
					var item struct {
						Type, Role string
						Content    json.RawMessage
					}
					if json.Unmarshal(raw, &item) == nil && item.Type == "message" && item.Role == "user" {
						var text string
						if json.Unmarshal(item.Content, &text) != nil {
							var parts []struct{ Type, Text string }
							if json.Unmarshal(item.Content, &parts) == nil {
								for _, part := range parts {
									if part.Type == "input_text" {
										text += part.Text
									}
								}
							}
						}
						if text != "" {
							result = append(result, model.ChatMessage{Role: "user", Content: text, PendingDelivery: msg.PendingDelivery})
						}
					}
				}
			}
			continue
		}
		if name, ok := foreignCalls[msg.ToolCallID]; msg.Role == "tool" && ok {
			msg = model.ChatMessage{Role: "user", Content: "先前的工具结果（" + name + "）：" + msg.Content, PendingDelivery: msg.PendingDelivery}
		}
		if result != nil {
			if i == originalCacheEnd {
				*cacheEnd = len(result)
			}
			result = append(result, msg)
		}
	}
	if result != nil {
		return result
	}
	return history
}

func asJSON(value any) string { raw, _ := json.Marshal(value); return string(raw) }
