package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const reactionTool = "react_to_message"

// 浏览器和成员工具使用同一套表情，避免同一个字形的不同编码被计为两种回应。
var reactionPalette = []struct {
	Emoji string `json:"emoji"`
	Label string `json:"label"`
}{
	{"👍", "赞"}, {"❤️", "喜欢"}, {"😂", "大笑"}, {"🙌", "击掌"},
	{"🤔", "思考"}, {"🎉", "庆祝"}, {"😊", "微笑"}, {"👀", "关注"},
}

type MessageReaction struct {
	Emoji    string   `json:"emoji"`
	ActorIDs []string `json:"actor_ids"`
}

type reactionChange struct {
	MessageID string `json:"message_id"`
	Emoji     string `json:"emoji"`
	Present   *bool  `json:"present"`
}

func (change reactionChange) validate() error {
	if change.MessageID == "" || change.Present == nil {
		return fmt.Errorf("需要指定消息及添加或撤回")
	}
	for _, face := range reactionPalette {
		if change.Emoji == face.Emoji {
			return nil
		}
	}
	return fmt.Errorf("这个表情暂不支持")
}

// 写入的是期望状态。重复提交添加或撤回都不会翻转当前状态，也不会更改消息的序号。
// actor 由调用方的真实身份提供，目标必须是同一聊天中的普通消息。
func setMessageReaction(tx *sql.Tx, room, actor string, change reactionChange) (bool, error) {
	var found int
	if err := tx.QueryRow(`SELECT 1 FROM messages WHERE conversation_id=? AND id=? AND author_id<>'system'`, room, change.MessageID).Scan(&found); err != nil {
		return false, err
	}
	query := `INSERT INTO message_reactions(message_id,emoji,actor_id) VALUES(?,?,?) ON CONFLICT DO NOTHING`
	if !*change.Present {
		query = `DELETE FROM message_reactions WHERE message_id=? AND emoji=? AND actor_id=?`
	}
	result, err := tx.Exec(query, change.MessageID, change.Emoji, actor)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

// 一次查整页原始回应，按消息和 emoji 聚合。数量从实际回应者推导，撤回不需要维护计数器。
func (s *Store) reactionGroups(query string, args ...any) (map[string][]MessageReaction, error) {
	rows, err := s.db.Query(`SELECT r.message_id,r.emoji,r.actor_id FROM message_reactions r `+query+
		` ORDER BY r.message_id,r.emoji,r.actor_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string][]MessageReaction{}
	for rows.Next() {
		var id, emoji, actor string
		if err := rows.Scan(&id, &emoji, &actor); err != nil {
			return nil, err
		}
		reactions := groups[id]
		if len(reactions) == 0 || reactions[len(reactions)-1].Emoji != emoji {
			reactions = append(reactions, MessageReaction{Emoji: emoji})
		}
		last := &reactions[len(reactions)-1]
		last.ActorIDs = append(last.ActorIDs, actor)
		groups[id] = reactions
	}
	return groups, rows.Err()
}

func (s *Store) attachMessageReactions(messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]string, len(messages))
	for i, message := range messages {
		ids[i] = message.ID
	}
	groups, err := s.reactionGroups(`WHERE r.message_id IN (SELECT value FROM json_each(?))`, asJSON(ids))
	if err != nil {
		return err
	}
	for i := range messages {
		messages[i].Reactions = groups[messages[i].ID]
	}
	return nil
}

func (t *Toolbox) reactToMessage(ctx context.Context, c Conversation, m Member, call ToolCall, runID string) (reactionChange, error) {
	var change reactionChange
	decoder := json.NewDecoder(strings.NewReader(call.Function.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil {
		return change, fmt.Errorf("表情参数格式不正确")
	}
	if err := change.validate(); err != nil {
		return change, err
	}
	var changed bool
	err := t.Store.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		changed, err = setMessageReaction(tx, c.ID, m.ID, change)
		if err != nil {
			return err
		}
		// 回执和回应同一事务落盘。恢复时仍为 running 就可以安全重试；
		// 已完成的旧调用只返回原回执，不会重新添加后来已撤回的表情。
		_, err = tx.Exec(`UPDATE tool_runs SET result=?,status='succeeded' WHERE id=?`, asJSON(change), runID)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return change, fmt.Errorf("当前聊天没有可回应的这条消息")
	}
	if err == nil && changed {
		t.Hub.PublishReactions(c.ID)
	}
	return change, err
}

func (a *API) messageReaction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Emoji   string `json:"emoji"`
		Present *bool  `json:"present"`
	}
	if !readBody(w, r, &input) {
		return
	}
	change := reactionChange{MessageID: r.PathValue("message"), Emoji: input.Emoji, Present: input.Present}
	if err := change.validate(); err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	room := r.PathValue("id")
	var changed bool
	err := a.engine.Store.Guard("", 0, func(tx *sql.Tx) error {
		var err error
		changed, err = setMessageReaction(tx, room, "user", change)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		respond(w, 404, map[string]string{"error": "当前聊天没有可回应的这条消息"})
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if changed {
		a.engine.Hub.PublishReactions(room)
	}
	respond(w, 200, change)
}

func (a *API) conversationReactions(w http.ResponseWriter, r *http.Request) {
	from, fromErr := strconv.ParseInt(r.URL.Query().Get("from_seq"), 10, 64)
	through, throughErr := strconv.ParseInt(r.URL.Query().Get("through_seq"), 10, 64)
	if fromErr != nil || throughErr != nil || from <= 0 || through < from {
		respond(w, 400, map[string]string{"error": "消息范围无效"})
		return
	}
	groups, err := a.engine.Store.reactionGroups(`JOIN messages m ON m.id=r.message_id
	 WHERE m.conversation_id=? AND m.seq BETWEEN ? AND ?`, r.PathValue("id"), from, through)
	if err != nil {
		a.fail(w, err)
		return
	}
	respond(w, 200, groups)
}
