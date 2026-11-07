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
			remaining--
			continue
		}
		messages[i].Content = toolExcerpt(messages[i].Content, limit)
	}
}

func toolExcerpt(raw string, limit int) string {
	if overview, ok := searchOverview(raw); ok {
		return overview
	}
	if len([]rune(raw)) <= limit {
		return raw
	}
	var value map[string]any
	_ = json.Unmarshal([]byte(raw), &value)
	// 多轮收缩必须幂等：对已有摘录继续截取，不能把包装 JSON 层层套进摘录。
	if excerpt, ok := value["excerpt"].(string); ok && value["truncated"] == true {
		raw = excerpt
	}
	excerpt := map[string]any{
		"tool_run_id": value["tool_run_id"], "status": value["status"],
		"excerpt": clip(raw, limit), "truncated": true,
		"note": "完整结果已保存，需要时用 read_tool_result 按 ID 分段读取。",
	}
	// 正文可能排在 JSON 时间字段之前；折叠时把网页来源时间单独留下。
	provenance := value
	if result, ok := value["result"].(map[string]any); ok {
		provenance = result
	}
	for _, key := range []string{"retrieved_at", "reused"} {
		if v, ok := provenance[key]; ok {
			excerpt[key] = v
		}
	}
	return asJSON(excerpt)
}

// 搜索概览保留所有原始摘要和来源元信息，不能截取整段 JSON 的开头，
// 让第一篇长文章遮住后面的结果。完整原文仍由 tool_run_id 分页读取。
type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Summary string `json:"summary"`
	Snippet string `json:"snippet"`
	Content string `json:"content"`
}

func searchOverview(raw string) (string, bool) {
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &value) != nil {
		return "", false
	}
	payload := value
	if nested, ok := value["result"]; ok {
		payload = nil
		if json.Unmarshal(nested, &payload) != nil {
			return "", false
		}
	}
	results, ok := payload["results"]
	if !ok {
		return "", false
	}
	var entries []map[string]any
	if json.Unmarshal(results, &entries) != nil {
		return "", false
	}
	for i := range entries {
		delete(entries[i], "content")
	}
	payload["results"] = json.RawMessage(asJSON(entries))
	payload["excerpt_note"] = json.RawMessage(asJSON("正文已省略；完整结果可按 tool_run_id 读取。"))
	if _, ok := value["result"]; ok {
		value["result"] = json.RawMessage(asJSON(payload))
	} else {
		value = payload
	}
	return asJSON(value), true
}

func sharedToolResult(raw string) string {
	if overview, ok := searchOverview(raw); ok {
		return overview
	}
	// 网页正文缩短时保留取得时间及缓存来源，避免把旧快照当成本次联网结果。
	var page map[string]any
	if json.Unmarshal([]byte(raw), &page) == nil && page["retrieved_at"] != nil {
		if content, ok := page["content"].(string); ok {
			page["content"] = clip(content, 650)
		}
		return asJSON(page)
	}
	return clip(raw, 650)
}
