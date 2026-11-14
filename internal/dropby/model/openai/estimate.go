package openai

import (
	"drop_by/internal/dropby/model"
	"encoding/json"
	"fmt"
)

func (m *Client) Estimate(messages []model.ChatMessage, encode func([]model.ChatMessage) ([]byte, error)) (int, error) {
	// Responses 的 encrypted_content 是加密传输状态，字节大小不代表输入 token。
	// 估算时只移除这项不透明编码，并加入供应商上轮报告的 OutputTokens；真实请求
	// 仍逐字发送原 ResponseItems，不能丢 call_id 或推理项目来换取“更短”的上下文。
	visible := append([]model.ChatMessage(nil), messages...)
	reasoningAllowance := 0
	for i, message := range messages {
		reasoningAllowance += max(0, message.OutputTokens)
		if len(message.ResponseItems) == 0 {
			continue
		}
		visible[i].ResponseItems = append([]json.RawMessage(nil), message.ResponseItems...)
		for j, item := range message.ResponseItems {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(item, &fields); err != nil {
				return 0, fmt.Errorf("模型返回的推理或工具协议记录无效，已保留本次行动记录")
			}
			if string(fields["type"]) == `"reasoning"` || string(fields["type"]) == `"compaction"` {
				delete(fields, "encrypted_content")
				visible[i].ResponseItems[j], _ = json.Marshal(fields)
			}
		}
	}
	raw, err := encode(visible)
	return len(raw) + reasoningAllowance, err
}
