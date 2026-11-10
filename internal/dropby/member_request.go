package dropby

import "encoding/json"

// 成员窗口还负责断点恢复。只在发请求时分开历史与当前说明，避免调整持久化或工具执行顺序。
// 原话从第一次交付起就用收尾后沿用的格式，最新说明不再插在后续可复用的工具与聊天之间。
func memberRequestMessages(messages []ChatMessage) ([]ChatMessage, int) {
	var scene map[string]json.RawMessage
	var sceneAt int
	for i := len(messages) - 1; i >= 0; i-- {
		if scene = sceneFields(messages[i]); scene != nil {
			sceneAt = i
			break
		}
	}
	if scene == nil {
		return messages, -1
	}

	var cause map[string]json.RawMessage
	var newIDs []string
	_ = json.Unmarshal(scene["new_message_ids"], &newIDs)
	known := contextMessages(messages)
	history := make([]ChatMessage, 0, len(messages)+1)
	for i, item := range messages {
		if fields := sceneFields(item); fields != nil {
			var originals []Message
			_ = json.Unmarshal(fields["latest_messages"], &originals)
			if len(originals) > 0 {
				history = appendDeliveredMessages(history, originals)
				history[len(history)-1].PendingDelivery = item.PendingDelivery
			}
			continue
		}
		if fields, originals := splitActionCause(item, known); fields != nil {
			cause = fields
			if len(originals) > 0 {
				history = appendDeliveredMessages(history, originals)
				history[len(history)-1].PendingDelivery = item.PendingDelivery
			}
			continue
		}
		history = append(history, item)
		// 工具续接期间收到的同伴原话和真人补充也已实际交付，沿用它们的编号。
		if i > sceneAt {
			_, originals, _ := messageBlock(item)
			for _, message := range originals {
				if !contains(newIDs, message.ID) {
					newIDs = append(newIDs, message.ID)
				}
			}
		}
	}

	// 末尾只附上本次说明。群状态的最后一条正文在历史里已完整交付时才去掉副本。
	delete(scene, "latest_messages")
	if len(newIDs) > 0 {
		scene["new_message_ids"], _ = json.Marshal(newIDs)
	}
	removeRepeatedLastMessage(scene, history)
	cacheEnd := len(history) - 1
	for cacheEnd >= 0 {
		item := history[cacheEnd]
		if len(item.ResponseItems) == 0 && (item.Role == "user" || item.Role == "tool") {
			break
		}
		cacheEnd--
	}
	history = append(history, ChatMessage{Role: "user", Content: asJSON(scene)})
	if cause != nil {
		history = append(history, ChatMessage{Role: "user", Content: actionCausePrefix + asJSON(cause)})
	}
	return history, cacheEnd
}

func removeRepeatedLastMessage(scene map[string]json.RawMessage, history []ChatMessage) {
	var room map[string]json.RawMessage
	var lastSeq int64
	var lastBody string
	if json.Unmarshal(scene["conversation"], &room) != nil || json.Unmarshal(room["last_seq"], &lastSeq) != nil ||
		json.Unmarshal(room["last_message"], &lastBody) != nil || lastSeq == 0 || lastBody == "" {
		return
	}
	for i := len(history) - 1; i >= 0; i-- {
		_, originals, _ := messageBlock(history[i])
		for _, original := range originals {
			if original.Seq == lastSeq && original.Body == lastBody {
				delete(room, "last_message")
				scene["conversation"], _ = json.Marshal(room)
				return
			}
		}
	}
}
