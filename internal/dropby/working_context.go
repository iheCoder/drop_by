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
	result.Messages = completedHistory(result.Messages)
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

const workingContextPrompt = `
你可以在不同聊天中回忆、引用和分享自己知道的内容，是否分享由你结合自身特质、关系、语境和用户明确要求决定；当前场景不是遗忘其他经历或禁止分享的边界。
你在持续参与这段交流。读过新消息后，根据自己的关注决定回应什么、继续做什么，或暂时不说话。
工作上下文会延续到下次行动。话题转变、问题告一段落、资料已经读懂，或旧内容妨碍当前思考时，可以调用 organize_context 整理；无需每轮整理，也不必因为一句插话就放下原话题。可以先回应，再整理。
整理时留下接下来有帮助的便笺和必要原文引用：保持说话者、不同意见、更正和未完打算各自的含义，不把分歧合并成共识，也不把一次表达写成永久特质。原消息、工具结果和成果仍可查回。
已经读到的内容足以回应时可以先回应；后来到达的普通消息接着处理，不必推翻已经形成的表达。最新现场中的用户控制与人物资料优先于旧窗口。需要经历、资料或工作进展时，可主动使用记忆、历史、工具结果、成果和工作读取工具。
本轮发言、安静或结束只是本次行动的决定；有新输入时结合新内容自行判断。要延续的理解或打算留在工作窗口与整理便笺中，outcome 只记本次运行结果。
`

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

// steering 可能先读到排在 BOT 消息后面的真人补充。连续游标不越过 BOT，
// 但下一次普通交付不应再把已应用的同一条真人消息作为新输入。
func withoutSteered(messages []Message, history []ChatMessage) []Message {
	applied := map[string]bool{}
	for _, item := range history {
		const prefix = "真人追加的消息："
		if item.Role != "user" || !strings.HasPrefix(item.Content, prefix) {
			continue
		}
		var batch []Message
		if json.Unmarshal([]byte(strings.TrimPrefix(item.Content, prefix)), &batch) == nil {
			for _, m := range batch {
				applied[m.ID] = true
			}
		}
	}
	result := make([]Message, 0, len(messages))
	for _, m := range messages {
		if !applied[m.ID] {
			result = append(result, m)
		}
	}
	return result
}

// 压缩是已完成的工作；在下一次网络请求之前将成果交还执行层落盘。
type preparedContextKey struct{}
type savePreparedContext func([]ChatMessage) error

// 当前现场固定放在系统指令后，历史保持自身顺序；不会打断历史工具配对。
func withScene(history []ChatMessage, scene ChatMessage) []ChatMessage {
	history = refreshScene(history)
	at := 0
	for at < len(history) && (history[at].Role == "system" || history[at].Role == "developer") {
		at++
	}
	result := append([]ChatMessage{}, history[:at]...)
	result = append(result, scene)
	result = append(result, history[at:]...)
	if len(result) > 0 && result[len(result)-1].Role == "assistant" {
		result = append(result, ChatMessage{Role: "user", Content: "本次行动的最新现场已更新，请继续自行决定如何参与。"})
	}
	return result
}

// 行动的原始来由属于一次性交付的经历，不是每次更新现场都要重申的指令。
// 已在本批新消息里的触发原话不重复附带；旧任务仍可首次交付其真实来由。
func (e *Engine) actionInput(c Conversation, w Work, delivered []Message) (ChatMessage, error) {
	trigger := roleWork(w)
	input := map[string]any{}
	known := map[string]bool{}
	for _, message := range delivered {
		known[message.ID] = true
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
			if !known[source.ID] {
				input["trigger_message"] = source
			}
			if trigger.Instruction == source.Body {
				trigger.Instruction = ""
			}
			if source.ReplyTo != "" && !known[source.ReplyTo] {
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
// 仅用于已完成窗口，绝不用于待消费的断点；原话、工具链和便笺仍按原样保留。
func completedHistory(history []ChatMessage) []ChatMessage {
	result := make([]ChatMessage, 0, len(history))
	for _, item := range history {
		var outcome map[string]json.RawMessage
		if item.Role == "assistant" && len(item.ToolCalls) == 0 && json.Unmarshal([]byte(item.Content), &outcome) == nil && outcome["outcome"] != nil {
			continue
		}
		result = append(result, item)
	}
	return result
}
