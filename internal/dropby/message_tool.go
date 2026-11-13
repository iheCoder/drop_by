package dropby

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
)

// 消息身份来自本次模型调用 ID。消息已发送、工具结果尚未保存时崩溃，
// 恢复可确认原发送，不重复发一遍；新的调用仍是本人的新表达。
func (t *Toolbox) sendMessage(ctx context.Context, c Conversation, m Member, w Work, call ToolCall, target, body, reply string, evidence []string, notify bool) (any, error) {
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("消息不能为空")
	}
	if target == "" {
		target = c.ID
	}
	var destination Conversation
	var err error
	if target == "" {
		destination, err = t.Store.CreateConversation(m.Name, "direct", []string{m.ID}, t.Config.ExploreInterval)
	} else {
		destination, err = t.Store.accessibleConversation(m.ID, target)
	}
	if err != nil {
		return nil, err
	}
	if destination.DiscussionPaused {
		return nil, fmt.Errorf("目标聊天已暂停发言")
	}
	id := sentMessageID(w, m, call)
	message := Message{ID: id, ConversationID: destination.ID, AuthorID: m.ID, Body: body, ReplyTo: reply, Evidence: evidence, Notify: notify, CreatedAt: now()}
	var priorRoom, priorBody string
	err = t.Store.db.QueryRow(`SELECT conversation_id,body FROM messages WHERE id=?`, id).Scan(&priorRoom, &priorBody)
	if err == nil {
		noteSentMessage(ctx, m.ID, c.ID, priorRoom, id, true)
		return map[string]string{"message_id": id, "conversation_id": priorRoom, "message": priorBody}, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	err = t.Store.Guard(destination.ID, destination.Revision, func(tx *sql.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := publishMessage(tx, message); err != nil {
			return err
		}
		if destination.ID != c.ID || directedWork(w) {
			return wakeConversation(tx, message, w)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	noteSentMessage(ctx, m.ID, c.ID, destination.ID, id, false)

	t.Hub.Publish(destination.ID)
	return map[string]string{"message_id": id, "conversation_id": destination.ID, "message": body}, nil
}

func sentMessageID(w Work, m Member, call ToolCall) string {
	return fmt.Sprintf("msg_%x", sha256.Sum256([]byte(w.ID+"\x00"+m.ID+"\x00"+call.ID)))
}
