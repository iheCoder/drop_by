package dropby

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var errActionStopped = fmt.Errorf("%w：该行动已停止", ErrStale)

// 不新增恢复计数或网络状态机。可恢复的网络错误回到原工作，沿用个人时钟间隔；
// 用户停止和永久参数错误不会因这个分支复活，历史 failed 也不会批量重放。
func recoverableNetwork(err error) bool {
	var failure *requestFailure
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &failure) && failure.Temporary)
}

// 同批工具可能只完成了一部分。已经落盘的结果必须交付；未执行的旧调用
// 以真实状态配对，模型看到新条件后可另作选择，而不是先执行过时的发送。
func (e *Engine) resumeReceipts(messages []ChatMessage, w Work, m Member) ([]ChatMessage, error) {
	if len(messages) == 0 || messages[len(messages)-1].Role != "assistant" {
		return messages, nil
	}
	last := messages[len(messages)-1]
	if len(last.ToolCalls) == 0 {
		return append(messages[:len(messages)-1], ChatMessage{Role: "user", Content: "上次尚未完成发布的内容：" + last.Content, PendingDelivery: true}), nil
	}
	for _, call := range last.ToolCalls {
		var status, result string
		// 工具和消息使用相同的调用散列，仅前缀不同。
		id := "tool_" + sentMessageID(w, m, call)[4:]
		err := e.Store.db.QueryRow(`SELECT status,result FROM tool_runs WHERE id=?`, id).Scan(&status, &result)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if err == sql.ErrNoRows {
			result = asJSON(map[string]string{"status": "not_executed", "reason": "恢复时先结合最新情况重新决定"})
		} else if status == "running" {
			var body string
			messageErr := e.Store.db.QueryRow(`SELECT body FROM messages WHERE id=?`, sentMessageID(w, m, call)).Scan(&body)
			if messageErr == nil {
				result = asJSON(map[string]string{"status": "succeeded", "message": body})
			} else {
				result = asJSON(map[string]string{"status": "unconfirmed", "tool_run_id": id})
			}
		} else {
			result = fmt.Sprintf(`{"tool_run_id":%q,"status":%q,"result":%s}`, id, status, result)
		}
		messages = append(messages, ChatMessage{Role: "tool", ToolCallID: call.ID, Content: result, PendingDelivery: true})
	}
	return messages, nil
}

// 个人行动没有默认房间，但仍能收到自己参与聊天的新话；只取本次行动开始后
// 或已经实际读到的位置之后的消息，旧历史继续按需检索。
func (s *Store) personalMessages(member string, after int64, since string) ([]Message, error) {
	rows, err := s.db.Query(`SELECT id FROM messages WHERE seq>? AND (?<>0 OR created_at>=?) AND conversation_id IN (SELECT conversation_id FROM participants WHERE member_id=?) ORDER BY seq`, after, after, since, member)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := []Message{}
	for _, id := range ids {
		var m Message
		err = s.db.QueryRow(`SELECT seq,id,conversation_id,author_id,body,created_at FROM messages WHERE id=?`, id).Scan(&m.Seq, &m.ID, &m.ConversationID, &m.AuthorID, &m.Body, &m.CreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, nil
}
