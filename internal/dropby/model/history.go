package model

import "encoding/json"

// LegacyUserText 取出旧 Responses 压缩项附带的用户原话。切换接入点时
// 还可以交付这些原话，供应商私有的压缩状态仍保留在原记录中。
func LegacyUserText(items []json.RawMessage) []string {
	var messages []string
	for _, raw := range items {
		var item struct {
			Type, Role string
			Content    json.RawMessage
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "message" || item.Role != "user" {
			continue
		}
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
			messages = append(messages, text)
		}
	}
	return messages
}
