package dropby

import "encoding/json"

// 同一行动可能连续读很多网页。完整记录保留在 SQLite；较早的工具结果只回传
// 有来源的摘录。保留每个 call_id 和所有 Responses 推理项目，避免破坏工具协议。
func compactToolHistory(messages []ChatMessage) {
	remaining := 4
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "tool" {
			continue
		}
		limit := 700
		if remaining > 0 {
			limit = 6000
			remaining--
		}
		messages[i].Content = toolExcerpt(messages[i].Content, limit)
	}
}

func toolExcerpt(raw string, limit int) string {
	if len([]rune(raw)) <= limit {
		return raw
	}
	var value map[string]any
	_ = json.Unmarshal([]byte(raw), &value)
	// 多轮收缩必须幂等：对已有摘录继续截取，不能把包装 JSON 层层套进摘录。
	if excerpt, ok := value["excerpt"].(string); ok && value["truncated"] == true {
		raw = excerpt
	}
	return asJSON(map[string]any{
		"tool_run_id": value["tool_run_id"], "status": value["status"],
		"excerpt": clip(raw, limit), "truncated": true,
		"note": "完整结果已保存，需要时用 read_tool_result 按 ID 分段读取，不要重复联网搜索同一内容。",
	})
}
