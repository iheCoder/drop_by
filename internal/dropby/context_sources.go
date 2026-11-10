package dropby

import (
	"encoding/json"
	"slices"
	"strings"
)

// 只有工具已经存下完整结果时才能用回读入口。状态说的是执行结果，
// delivery 说的是模型收到多少，二者不能混用，否则会把已发送当成待发送。
func toolResultReference(content, delivery string) string {
	var receipt struct {
		ID     string          `json:"tool_run_id"`
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal([]byte(content), &receipt) != nil || receipt.ID == "" || len(receipt.Result) == 0 {
		return content
	}
	if encoded := toolRunReference(receipt.ID, receipt.Status, delivery); len(encoded) < len(content) {
		return encoded
	}
	return content
}

func toolRunReference(id, status, delivery string) string {
	ref := map[string]string{"tool_run_id": id, "status": status, "delivery": delivery}
	if delivery == "deferred" {
		ref["detail"] = "本次全文超出可用窗口，尚未交付。用 read_tool_result 按此 ID 选择 query、offset 和较小的 length 分段读。"
	}
	return asJSON(ref)
}

func readOrganizedReceipts(item ChatMessage) []organizedReceipt {
	if item.Role != "user" || len(item.ResponseItems) != 0 {
		return nil
	}
	body, ok := strings.CutPrefix(item.Content, organizedReceiptsPrefix)
	if !ok {
		return nil
	}
	var receipts []organizedReceipt
	if json.Unmarshal([]byte(body), &receipts) != nil {
		return nil
	}
	return receipts
}

// 真实计数确认容量不足后，才将占用最多的可回读结果改成回执，
// 每次改完重新计数。用字节给候选排序，不用它代替容量，也不切断原文。
func deferLargestResult(window []ChatMessage) bool {
	var largest, saved int
	var replacement string
	for i, item := range window {
		if item.Role == "tool" {
			ref := toolResultReference(item.Content, "deferred")
			if gain := len(item.Content) - len(ref); gain > saved {
				largest, saved, replacement = i, gain, ref
			}
			continue
		}
		// 成员选中的工具材料也可能单份就超限。保留其选择和原始记录
		// 入口，让它用分段读取继续。便笺和所选消息原话保持完整。
		if raw, ok := strings.CutPrefix(item.Content, organizedContextPrefix); item.Role == "user" && len(item.ResponseItems) == 0 && ok {
			var organization organizedContext
			if json.Unmarshal([]byte(raw), &organization) == nil {
				var chosen int
				var ref string
				for j, run := range organization.ToolRuns {
					candidate := toolRunReference(run.ID, run.Status, "deferred")
					if gain := len(run.Result) - len(candidate); run.ID != "" && gain > saved {
						chosen, saved, ref = j, gain, candidate
					}
				}
				if ref != "" {
					organization.ToolRuns[chosen].Result = ref
					largest, replacement = i, organizedContextPrefix+asJSON(organization)
				}
			}
			continue
		}
		receipts := readOrganizedReceipts(item)
		var largestReceipt int
		var ref string
		for j, receipt := range receipts {
			candidate := toolResultReference(receipt.Result, "deferred")
			if gain := len(receipt.Result) - len(candidate); gain > saved {
				largestReceipt, saved, ref = j, gain, candidate
			}
		}
		if ref != "" {
			receipts[largestReceipt].Result = ref
			largest, replacement = i, organizedReceiptsPrefix+asJSON(receipts)
		}
	}
	if saved == 0 {
		return false
	}
	window[largest].Content = replacement
	return true
}

// 原生压缩有时仍会保留旧原话。从原始前缀创建一个较小的候选窗口，
// 只换掉应用知道如何回查的内容。函数参数和供应商原生输出保持原样。
func referenceOldContext(history []ChatMessage, canReadHistory, canReadResults bool) ([]ChatMessage, bool) {
	result := slices.Clone(history)
	changed := false
	for i, item := range result {
		content := item.Content
		if canReadHistory {
			if _, messages, ok := messageBlock(item); ok && len(messages) > 0 {
				recoverable := true
				for j := range messages {
					recoverable = recoverable && messages[j].ID != "" && messages[j].ConversationID != ""
					messages[j].Body = ""
				}
				if recoverable {
					ref := "已读聊天来源，可用 read_history 按聊天和消息 ID 回读：" + asJSON(messages)
					if len(ref) < len(content) {
						content = ref
					}
				}
			}
		}
		if canReadResults && item.Role == "tool" {
			content = toolResultReference(content, "archived")
		} else if canReadResults {
			receipts := readOrganizedReceipts(item)
			for j := range receipts {
				receipts[j].Result = toolResultReference(receipts[j].Result, "archived")
			}
			if len(receipts) > 0 {
				ref := organizedReceiptsPrefix + asJSON(receipts)
				if len(ref) < len(content) {
					content = ref
				}
			}
		}
		if content != item.Content {
			result[i].Content = content
			changed = true
		}
	}
	return result, changed
}
