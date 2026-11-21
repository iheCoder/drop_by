package dropby

import (
	"encoding/json"
	"sort"
	"unicode/utf8"
)

// 这是一次普通搜索愿意主动展示的原文量（字符），独立于模型的硬窗口。
// full、指定正文区间或主动展开记忆来源，都表示成员已经选择要读的材料。
const recallPreviewChars = 3200

// 执行与恢复都走这一步。tool_runs 保存完整快照，只在产生下一次模型输入时做预览。
// 同批先交付短结果，余下原文平分可用篇幅；明确索取的内容不参与默认分配。
func presentRecallBatch(calls []ToolCall, messages []ChatMessage) {
	type pending struct {
		index int
		id    string
		raw   json.RawMessage
		size  int
	}
	var items []pending
	for i, call := range calls {
		if call.Function.Name != "recall" {
			continue
		}
		var q recallRequest
		if json.Unmarshal([]byte(call.Function.Arguments), &q) != nil || q.Full || (q.ID != "" && (q.Length > 0 || q.SourceBodies)) {
			continue
		}
		var receipt struct {
			ID     string `json:"tool_run_id"`
			Status string
			Result json.RawMessage
		}
		if json.Unmarshal([]byte(messages[i].Content), &receipt) != nil || receipt.Status != "succeeded" || receipt.ID == "" || len(receipt.Result) == 0 {
			continue
		}
		items = append(items, pending{i, receipt.ID, receipt.Result, utf8.RuneCount(receipt.Result)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].size < items[j].size })
	left := recallPreviewChars
	for i, item := range items {
		share := left / (len(items) - i)
		if item.size <= share {
			left -= item.size
			continue
		}
		left -= share
		page := recallPreview(item.raw, share)
		page["preview"] = true
		page["result_chars"] = item.size
		page["note"] = "仅展示部分原文。可按 tool_run_id 用 read_tool_result 定位或分段读取此次完整结果。"
		messages[item.index].Content = asJSON(map[string]any{"tool_run_id": item.id, "status": "succeeded", "result": page})
	}
}

// SQL 和搜索候选都沿用原顺序。能完整展示的行原样返回；若第一行自己就太长，
// 至少给出其开头的原文。完整快照的行号和字符范围始终可以另外读取。
func recallPreview(raw json.RawMessage, budget int) map[string]any {
	var result map[string]json.RawMessage
	_ = json.Unmarshal(raw, &result)
	page := map[string]any{}
	for _, key := range []string{"rows", "records"} {
		var rows []json.RawMessage
		if json.Unmarshal(result[key], &rows) != nil {
			continue
		}
		if columns := result["columns"]; len(columns) > 0 && utf8.RuneCount(columns) <= budget/2 {
			page["columns"] = columns
			budget -= utf8.RuneCount(columns)
		}
		shown := []json.RawMessage{}
		for _, row := range rows {
			size := utf8.RuneCount(row) + 1
			if size > budget {
				if len(shown) == 0 {
					var cells []json.RawMessage
					if key == "rows" && json.Unmarshal(row, &cells) == nil && len(cells) == 1 {
						// 常见的 SELECT body：展示字段文字，保留正常的一行一列结构。
						shown = append(shown, json.RawMessage(asJSON([]string{recallTextExcerpt(cells[0], budget)})))
					} else {
						page["row_excerpt"] = recallTextExcerpt(row, budget)
					}
				}
				break
			}
			shown = append(shown, row)
			budget -= size
		}
		page[key] = shown
		page["total_rows"] = len(rows)
		// 原 next_offset 指向原始整页的末尾，预览只交付了其中几条。
		if key == "records" && len(rows) > 0 {
			var next int
			if json.Unmarshal(result["next_offset"], &next) == nil {
				page["next_offset"] = next - len(rows) + len(shown)
			}
			page["has_more"] = len(shown) < len(rows) || string(result["has_more"]) == "true"
		}
		return page
	}
	// 对单篇作品、消息或记忆优先展示正文。其余元信息、记忆来源保留在原快照。
	for _, key := range []string{"artifact", "message", "memory"} {
		var record map[string]json.RawMessage
		if json.Unmarshal(result[key], &record) != nil {
			continue
		}
		for _, field := range []string{"id", "version", "revision", "title"} {
			if v := record[field]; v != nil && utf8.RuneCount(v) <= budget/2 {
				page[field] = v
				budget -= utf8.RuneCount(v)
			}
		}
		for _, field := range []string{"content", "body"} {
			if v := record[field]; v != nil {
				page["excerpt"] = recallTextExcerpt(v, budget)
				return page
			}
		}
	}
	page["excerpt"] = recallTextExcerpt(raw, budget)
	return page
}

func recallTextExcerpt(value json.RawMessage, limit int) string {
	var text string
	if json.Unmarshal(value, &text) != nil {
		text = string(value)
	}
	chars := []rune(text)
	return string(chars[:min(len(chars), max(0, limit))])
}
