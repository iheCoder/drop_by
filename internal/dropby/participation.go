package dropby

import (
	"database/sql"
	"errors"
	"fmt"
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
	return err
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
// state 为 nil 时保留持续关注，显式空串清除已不再成立的关注。新内容必须引用本群
// 真实存在的材料；这里核对引用范围，具体内容与材料是否相符仍依赖模型判断。
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
		// 缺失或虚构的引用不能成为持久自述；不为这项可选更新挡住有效的聊天回复。
		if body != "" && len(sources) == 0 {
			return nil
		}
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
		_, err := tx.Exec(`UPDATE member_observations SET state=?,sources=?,updated_at=? WHERE conversation_id=? AND member_id=?`, clip(body, 600), asJSON(sources), now(), c.ID, member)
		return err
	})
}

// 关注点可以比近期窗口更早，也可以串起几个人的不同发言。保留具体引用，执行时
// 重新读原话，避免仅凭先前的一句意图摘要继续发挥。
func (s *Store) selectedMessages(room string, ids []string) ([]Message, error) {
	messages := []Message{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var m Message
		var question string
		m.ConversationID = room
		err := s.db.QueryRow(`SELECT m.seq,m.id,m.author_id,m.body,m.reply_to,m.created_at,coalesce(q.question,''),coalesce(q.status,'')
		 FROM messages m LEFT JOIN message_questions q ON q.message_id=m.id WHERE m.conversation_id=? AND m.id=?`, room, id).
			Scan(&m.Seq, &m.ID, &m.AuthorID, &m.Body, &m.ReplyTo, &m.CreatedAt, &question, &m.QuestionStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("成员选择的消息不在当前聊天中")
		}
		if err != nil {
			return nil, err
		}
		m.Body = clip(m.Body, 1600)
		if question != "" {
			var q Question
			decode(question, &q)
			m.Question = &q
		}
		messages = append(messages, m)
	}
	return messages, nil
}

func observedThrough(input map[string]any) int64 {
	var seq int64
	for _, message := range input["latest_messages"].([]Message) {
		seq = max(seq, message.Seq)
	}
	return seq
}

const statePrompt = `state 是你此刻仍在意的具体事情、感受或想继续弄清的目标，用一两句自然语言写给用户看，可体现性格；不必凑齐状态/目标字段，不是内心推理日志。
只有新交流或真实行动改变了持续关注时才返回 state，并在 state_source_ids 引用本会话已有的消息或成功工具记录 ID；没有变化就省略这两个字段，已有关注不再有效可显式 state="" 清除。
可以因他人说的话产生好奇、犹疑、欣赏或改变想法，但不能捏造身体感受、离线生活、已完成行动，也不能把一次临时表达固化为永久性格。`
