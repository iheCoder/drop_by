package dropby

import (
	"database/sql"
	"encoding/json"
	"fmt"
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
已经读到的内容足以回应时可以先回应；后来到达的普通消息接着处理，不必推翻已经形成的表达。最新现场中的用户控制、人物资料和实际工作状态优先于旧窗口。
`

// 私有笔记和旧的写入参数不重新注入默认索引，避免已整理的内容绕路回来。
func sourceArguments(run ToolRun) string {
	if run.Name == "search" || run.Name == "read_page" {
		return string(run.Arguments)
	}
	return ""
}
