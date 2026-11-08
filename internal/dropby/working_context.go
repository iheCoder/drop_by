package dropby

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// 工作上下文属于一个成员在一个场景中的连续理解；断点属于某次未完行动。
// 前者在行动结束后保留，后者仍按原来的幂等执行规则删除。
type workingContext struct {
	Seen     int64
	Messages []ChatMessage
}

func (s *Store) loadWorkingContext(room, member string) (workingContext, error) {
	var result workingContext
	var raw string
	err := s.db.QueryRow(`SELECT seen,messages FROM working_contexts WHERE conversation_id=? AND member_id=?`, room, member).Scan(&result.Seen, &raw)
	if err == sql.ErrNoRows {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	err = json.Unmarshal([]byte(raw), &result.Messages)
	result.Messages = completedHistory(restoreSceneOrder(result.Messages))
	return result, err
}

// 整理内容由本人撰写，程序只校验原文归属并取回选中的完整记录。
// 不设置摘要长度、保留条数或固定栏目，不把整理升级为长期人格记忆。
type organizedContext struct {
	Note     string    `json:"note"`
	Messages []Message `json:"messages"`
	ToolRuns []ToolRun `json:"tool_runs"`
}

func (s *Store) organizeContext(c Conversation, m Member, raw string) (organizedContext, error) {
	var args struct {
		Note       string   `json:"note"`
		MessageIDs []string `json:"keep_message_ids"`
		ToolIDs    []string `json:"keep_tool_run_ids"`
	}
	result := organizedContext{}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return result, err
	}
	result.Note = args.Note
	for _, id := range args.MessageIDs {
		message := Message{ConversationID: c.ID}
		if err := s.db.QueryRow(`SELECT conversation_id,seq,id,author_id,body,reply_to,created_at FROM messages WHERE id=? AND conversation_id IN (SELECT conversation_id FROM participants WHERE member_id=?)`, id, m.ID).Scan(&message.ConversationID, &message.Seq, &message.ID, &message.AuthorID, &message.Body, &message.ReplyTo, &message.CreatedAt); err != nil {
			return result, fmt.Errorf("找不到本人参与聊天中的消息：%s", id)
		}
		result.Messages = append(result.Messages, message)
	}
	for _, id := range args.ToolIDs {
		var run ToolRun
		var arguments string
		if err := s.db.QueryRow(`SELECT id,work_id,conversation_id,member_id,name,arguments,status,result,created_at FROM tool_runs WHERE id=? AND ((conversation_id='' AND member_id=?) OR conversation_id IN (SELECT conversation_id FROM participants WHERE member_id=?))`, id, m.ID, m.ID).Scan(&run.ID, &run.WorkID, &run.ConversationID, &run.MemberID, &run.Name, &arguments, &run.Status, &run.Result, &run.CreatedAt); err != nil {
			return result, fmt.Errorf("找不到本人可读取的工具记录：%s", id)
		}
		if !toolResultVisibleTo(run, m.ID) {
			return result, fmt.Errorf("不能读取其他成员的私有记录：%s", id)
		}
		run.Arguments = json.RawMessage(arguments)
		result.ToolRuns = append(result.ToolRuns, run)
	}
	return result, nil
}

// 每次只携带一份最新现场。旧现场中的实际交付原话留下，状态、索引和
// 历史自动召回不再注入；不触碰普通聊天、模型便笺及不透明协议项目。
func refreshScene(history []ChatMessage) []ChatMessage {
	result := make([]ChatMessage, 0, len(history))
	for _, msg := range history {
		var scene map[string]json.RawMessage
		if msg.Role != "user" || len(msg.ResponseItems) > 0 || json.Unmarshal([]byte(msg.Content), &scene) != nil || scene["self"] == nil || scene["trigger"] == nil || scene["time"] == nil {
			result = append(result, msg)
			continue
		}
		var delivered []Message
		_ = json.Unmarshal(scene["latest_messages"], &delivered)
		if len(delivered) > 0 {
			result = append(result, ChatMessage{Role: "user", Content: "已交付的聊天原文：" + asJSON(delivered)})
		}
	}
	return result
}

// 按消息身份核对已交付内容，既包括 steering，也包括已确认的本人发送。
// 相似文字、不同成员的相同意见仍是不同消息，不做语义去重。
func withoutDelivered(messages []Message, history []ChatMessage) []Message {
	known := contextMessages(history)
	result := make([]Message, 0, len(messages))
	for _, m := range messages {
		if body, ok := known[m.ID]; !ok || body != m.Body {
			result = append(result, m)
		}
	}
	return result
}

// 压缩是已完成的工作；在下一次网络请求之前将成果交还执行层落盘。
type preparedContextKey struct{}
type savePreparedContext func([]ChatMessage) error

// 新现场追加在已完成的工具批次之后，原话与回应保持实际交错顺序。
// 待消费的 assistant 响应由恢复路径先处理，不能在未配对的调用中插入现场。
func withScene(history []ChatMessage, scene ChatMessage) []ChatMessage {
	return append(refreshScene(history), scene)
}

// 行动的原始来由属于一次性交付的经历，不是每次更新现场都要重申的指令。
// 已在本批新消息里的触发原话不重复附带；旧任务仍可首次交付其真实来由。
func (e *Engine) actionInput(c Conversation, w Work, delivered []Message, history []ChatMessage, seenBefore int64) (ChatMessage, error) {
	trigger := roleWork(w)
	input := map[string]any{}
	known := contextMessages(history)
	for _, message := range delivered {
		known[message.ID] = message.Body
		if trigger.Instruction == message.Body {
			trigger.Instruction = ""
		}
	}
	if w.SourceID != "" && c.ID != "" {
		var source Message
		err := e.Store.db.QueryRow(`SELECT conversation_id,seq,id,author_id,body,reply_to,created_at FROM messages WHERE id=? AND conversation_id=?`, w.SourceID, c.ID).Scan(&source.ConversationID, &source.Seq, &source.ID, &source.AuthorID, &source.Body, &source.ReplyTo, &source.CreatedAt)
		if err != nil && err != sql.ErrNoRows {
			return ChatMessage{}, err
		}
		if err == nil {
			// 跟聊沿用旧触发 ID 不代表重新收到原话。成员已整理掉的旧来由
			// 也不再强行带回；新到期的研究/预约仍可交付一次其任务来源。
			alreadyFollowed := w.Kind == "followup" && source.Seq <= seenBefore
			if body, ok := known[source.ID]; !alreadyFollowed && (!ok || body != source.Body) {
				input["trigger_message"] = source
			}
			if trigger.Instruction == source.Body {
				trigger.Instruction = ""
			}
			if _, ok := known[source.ReplyTo]; source.ReplyTo != "" && !ok && !alreadyFollowed {
				var quoted Message
				err := e.Store.db.QueryRow(`SELECT conversation_id,seq,id,author_id,body,reply_to,created_at FROM messages WHERE id=? AND conversation_id=?`, source.ReplyTo, c.ID).Scan(&quoted.ConversationID, &quoted.Seq, &quoted.ID, &quoted.AuthorID, &quoted.Body, &quoted.ReplyTo, &quoted.CreatedAt)
				if err != nil && err != sql.ErrNoRows {
					return ChatMessage{}, err
				}
				if err == nil {
					input["quoted_message"] = quoted
				}
			}
		}
	}
	input["trigger"] = trigger
	return ChatMessage{Role: "user", Content: "本次行动的来由：" + asJSON(input)}, nil
}

// 已完成的 outcome 属于运行记录；不把“本轮安静”等决定续接为下一轮状态。
// 仅用于已完成窗口，绝不用于待消费的断点。旧行动来由也不再充当新指令；
// 若它单独携带了原话，则把原话留下。真实任务记录仍可通过 read_work 回查。
func completedHistory(history []ChatMessage) []ChatMessage {
	result := make([]ChatMessage, 0, len(history))
	known := contextMessages(history)
	for _, item := range history {
		var outcome map[string]json.RawMessage
		if item.Role == "assistant" && len(item.ToolCalls) == 0 && json.Unmarshal([]byte(item.Content), &outcome) == nil && outcome["outcome"] != nil {
			continue
		}
		if item.Role == "user" && len(item.ResponseItems) == 0 && strings.HasPrefix(item.Content, "本次行动的来由：") {
			var cause struct {
				Trigger Message `json:"trigger_message"`
				Quoted  Message `json:"quoted_message"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(item.Content, "本次行动的来由：")), &cause) == nil {
				var originals []Message
				for _, source := range []Message{cause.Quoted, cause.Trigger} {
					if body, ok := known[source.ID]; source.ID != "" && (!ok || body != source.Body) {
						originals = append(originals, source)
						known[source.ID] = source.Body
					}
				}
				if len(originals) > 0 {
					result = append(result, ChatMessage{Role: "user", Content: "已交付的聊天原文：" + asJSON(originals)})
				}
				continue
			}
		}
		result = append(result, item)
	}
	return distinctMessageSources(result)
}

// 只解析程序交付的原话容器。普通文本、主动读取的工具材料和不透明压缩项目
// 不参与自动去重，避免把成员有意引用或比较的内容误当成重复通知。
func messageBlock(item ChatMessage) (string, []Message, bool) {
	if item.Role != "user" || len(item.ResponseItems) > 0 {
		return "", nil, false
	}
	for _, prefix := range []string{"已交付的聊天原文：", "接续收到的聊天原文（author_id 标明说话者）：", "真人追加的消息："} {
		if strings.HasPrefix(item.Content, prefix) {
			var messages []Message
			err := json.Unmarshal([]byte(strings.TrimPrefix(item.Content, prefix)), &messages)
			return prefix, messages, err == nil
		}
	}
	var scene map[string]json.RawMessage
	if json.Unmarshal([]byte(item.Content), &scene) == nil && scene["self"] != nil && scene["trigger"] != nil && scene["time"] != nil {
		var messages []Message
		if json.Unmarshal(scene["latest_messages"], &messages) == nil {
			return "", messages, true
		}
	}
	return "", nil, false
}

// 发送参数只有与成功回执的消息 ID 对上才算已经交付。失败、未执行的调用
// 不能吞掉真正到达的消息；不依靠文字相似或“这是自己说的”来猜测。
func sentContextMessages(history []ChatMessage) map[string]string {
	calls, sent := map[string]string{}, map[string]string{}
	for _, item := range history {
		for _, call := range item.ToolCalls {
			var args struct {
				Message string `json:"message"`
			}
			if call.Function.Name == "send_message" && json.Unmarshal([]byte(call.Function.Arguments), &args) == nil && args.Message != "" {
				calls[call.ID] = args.Message
			}
		}
		if item.Role != "tool" {
			continue
		}
		var receipt struct {
			Status string `json:"status"`
			Result struct {
				ID      string  `json:"message_id"`
				Message *string `json:"message"`
			} `json:"result"`
		}
		body, ok := calls[item.ToolCallID]
		if ok && json.Unmarshal([]byte(item.Content), &receipt) == nil && receipt.Status == "succeeded" && receipt.Result.ID != "" && (receipt.Result.Message == nil || *receipt.Result.Message == body) {
			sent[receipt.Result.ID] = body
		}
	}
	return sent
}

func contextMessages(history []ChatMessage) map[string]string {
	known := sentContextMessages(history)
	for _, item := range history {
		_, batch, ok := messageBlock(item)
		if ok {
			for _, message := range batch {
				known[message.ID] = message.Body
			}
		}
	}
	return known
}

// 只减少相同正文的载体：调用参数保留原文，成功回执保留消息 ID、目的地和
// 状态。工具原始结果仍完整存于 tool_runs；原生调用项目及其配对保持原样。
func distinctMessageSources(history []ChatMessage) []ChatMessage {
	sent := sentContextMessages(history)
	known := make(map[string]string, len(sent))
	for id, body := range sent {
		known[id] = body
	}
	result := make([]ChatMessage, 0, len(history))
	for _, item := range history {
		if item.Role == "tool" {
			var receipt, value map[string]json.RawMessage
			var id, body string
			if json.Unmarshal([]byte(item.Content), &receipt) == nil && json.Unmarshal(receipt["result"], &value) == nil {
				_ = json.Unmarshal(value["message_id"], &id)
				if expected, ok := sent[id]; json.Unmarshal(value["message"], &body) == nil && ok && expected == body {
					delete(value, "message")
					receipt["result"] = json.RawMessage(asJSON(value))
					item.Content = asJSON(receipt)
				}
			}
		}
		if prefix, batch, ok := messageBlock(item); ok {
			kept := []Message{}
			ids := []string{}
			for _, message := range batch {
				if body, exists := known[message.ID]; !exists || body != message.Body {
					kept = append(kept, message)
					ids = append(ids, message.ID)
					known[message.ID] = message.Body
				}
			}
			if prefix == "" {
				var scene map[string]json.RawMessage
				_ = json.Unmarshal([]byte(item.Content), &scene)
				scene["latest_messages"], scene["new_message_ids"] = json.RawMessage(asJSON(kept)), json.RawMessage(asJSON(ids))
				item.Content = asJSON(scene)
			} else if len(kept) > 0 {
				item.Content = prefix + asJSON(kept)
			} else {
				continue
			}
		}
		result = append(result, item)
	}
	return result
}

// 旧版把每批现场倒插到系统提示之后。完整保留“倒序原话块 + 对应行动来由”
// 时可以确定还原位置：最早一批放回最早行动之前，工具链及便笺不动。
// 已经压缩或缺少对应来由的窗口不猜测历史，也不为了升级清空成员的理解。
func restoreSceneOrder(history []ChatMessage) []ChatMessage {
	flat := refreshScene(history)
	start := 0
	for start < len(flat) && (flat[start].Role == "system" || flat[start].Role == "developer") {
		start++
	}
	end := start
	previous := int64(0)
	for end < len(flat) {
		prefix, batch, ok := messageBlock(flat[end])
		if !ok || prefix != "已交付的聊天原文：" || len(batch) == 0 {
			break
		}
		if batch[0].Seq <= 0 || (end > start && batch[len(batch)-1].Seq >= previous) {
			return history
		}
		previous = batch[0].Seq
		end++
	}
	if end-start < 2 {
		return history
	}
	causes := 0
	for _, item := range flat[end:] {
		if item.Role == "user" && len(item.ResponseItems) == 0 && strings.HasPrefix(item.Content, "本次行动的来由：") {
			causes++
		}
	}
	// 更早版本的首轮只有原话与工具链，尚未引入独立“行动来由”。这种形状
	// 多出最早的一批原话，先放回首个 assistant 之前，再配对后续行动。
	initial := 0
	if end < len(flat) && flat[end].Role == "assistant" {
		initial = 1
	}
	if causes+initial != end-start {
		return history
	}
	result := append([]ChatMessage{}, flat[:start]...)
	next := end - 1
	if initial == 1 {
		result = append(result, flat[next])
		next--
	}
	for _, item := range flat[end:] {
		if item.Role == "user" && len(item.ResponseItems) == 0 && strings.HasPrefix(item.Content, "本次行动的来由：") {
			result = append(result, flat[next])
			next--
		}
		result = append(result, item)
	}
	return result
}
