package dropby

import (
	"database/sql"
	"errors"
	"strings"
)

// MemberState 记录成员在这段交流里仍然在意的事情。自然语言可以表达感受、疑问
// 或目标，不另造情绪分数或性格状态机。按会话保存，避免私聊的关注泄露到别的群。
type MemberState struct {
	MemberID       string   `json:"member_id"`
	ConversationID string   `json:"conversation_id"`
	Body           string   `json:"body"`
	SourceIDs      []string `json:"source_ids"`
	UpdatedAt      string   `json:"updated_at"`
}

func (s *Store) initParticipation() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS member_observations(
	 conversation_id TEXT NOT NULL REFERENCES conversations(id), member_id TEXT NOT NULL REFERENCES members(id),
	 seen_seq INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT '', sources TEXT NOT NULL DEFAULT '[]',
	 updated_at TEXT NOT NULL DEFAULT '', PRIMARY KEY(conversation_id, member_id));`)
	if err != nil {
		return err
	}
	return s.initContextWindow()
}

func (s *Store) observation(room, member string) (int64, MemberState, error) {
	state := MemberState{MemberID: member, ConversationID: room, SourceIDs: []string{}}
	var seq int64
	var sources string
	err := s.db.QueryRow(`SELECT seen_seq,state,sources,updated_at FROM member_observations WHERE conversation_id=? AND member_id=?`, room, member).
		Scan(&seq, &state.Body, &sources, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, state, nil
	}
	if err == nil {
		decode(sources, &state.SourceIDs)
	}
	return seq, state, err
}

func (s *Store) MemberStates(room string) ([]MemberState, error) {
	rows, err := s.db.Query(`SELECT member_id,state,sources,updated_at FROM member_observations WHERE conversation_id=? AND state<>'' ORDER BY member_id`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := []MemberState{}
	for rows.Next() {
		state := MemberState{ConversationID: room}
		var sources string
		if err = rows.Scan(&state.MemberID, &state.Body, &sources, &state.UpdatedAt); err != nil {
			return nil, err
		}
		decode(sources, &state.SourceIDs)
		states = append(states, state)
	}
	return states, rows.Err()
}

// 即使最终选择安静，也记住实际读到了哪里；取消、失败、过时的判断不能推进位置。
// state 为 nil 时保留持续关注，显式空串清除已不再成立的关注。自发打算可以不带来源；
// 提供来源时核对引用范围，避免把其他场景的原文混入当前状态。
func (s *Store) recordObservation(c Conversation, member string, seen int64, state *string, sources []string) error {
	return s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO member_observations(conversation_id,member_id,seen_seq) VALUES(?,?,?)
		 ON CONFLICT(conversation_id,member_id) DO UPDATE SET seen_seq=max(seen_seq,excluded.seen_seq)`, c.ID, member, seen); err != nil {
			return err
		}
		if state == nil {
			return nil
		}
		body := strings.TrimSpace(*state)
		// 提供的引用必须真实；没有引用的个人念头也可以留下。
		for _, id := range sources {
			var exists int
			if err := tx.QueryRow(`SELECT
			 (SELECT count(*) FROM messages WHERE id=? AND conversation_id=? AND seq<=?) +
			 (SELECT count(*) FROM tool_runs WHERE id=? AND conversation_id=? AND status='succeeded')`, id, c.ID, seen, id, c.ID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return nil
			}
		}
		_, err := tx.Exec(`UPDATE member_observations SET state=?,sources=?,updated_at=? WHERE conversation_id=? AND member_id=?`, clip(body, 1600), asJSON(sources), now(), c.ID, member)
		return err
	})
}

func observedThrough(input map[string]any) int64 {
	var seq int64
	messages, _ := input["latest_messages"].([]Message)
	for _, message := range messages {
		seq = max(seq, message.Seq)
	}
	return seq
}
