package dropby

import (
	"database/sql"
	"fmt"
)

// context_start_seq 只决定这位成员本次自动带入的原话从哪里开始，不删除历史，
// 也不决定性格和话题分类。切题是否成立由成员结合语境判断；身份、持续关注、
// 用户更正和未完成工作不受这个窗口影响。
func (s *Store) initContextWindow() error {
	var exists int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('member_observations') WHERE name='context_start_seq'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		_, err := s.db.Exec(`ALTER TABLE member_observations ADD COLUMN context_start_seq INTEGER NOT NULL DEFAULT 0`)
		return err
	}
	return nil
}

func (s *Store) contextStart(room, member string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(`SELECT context_start_seq FROM member_observations WHERE conversation_id=? AND member_id=?`, room, member).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return seq, err
}

func (s *Store) rememberContextStart(c Conversation, member string, seen int64, id *string) error {
	if id == nil {
		return nil
	}
	return s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		var seq int64
		if *id != "" {
			// 只接受实际读到的本会话原话。跨私聊引用、未来消息不能改变记忆窗口。
			if err := tx.QueryRow(`SELECT seq FROM messages WHERE conversation_id=? AND id=? AND seq<=?`, c.ID, *id, seen).Scan(&seq); err != nil {
				return fmt.Errorf("上下文起点必须是已读到的当前聊天消息")
			}
		}
		_, err := tx.Exec(`INSERT INTO member_observations(conversation_id,member_id,context_start_seq) VALUES(?,?,?)
		 ON CONFLICT(conversation_id,member_id) DO UPDATE SET context_start_seq=excluded.context_start_seq`, c.ID, member, seq)
		return err
	})
}

// 已按参与关系定位原消息，再读取指定字符区间和实时表情。
func (s *Store) readMessage(room, id string, offset, length int) (any, error) {
	m, err := scanChatMessage(s.db.QueryRow(`SELECT `+chatMessageColumns+` FROM messages m LEFT JOIN message_questions q ON q.message_id=m.id WHERE m.conversation_id=? AND m.id=?`, room, id))
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("当前聊天没有这条消息")
	}
	if err != nil {
		return nil, err
	}
	chars := []rune(m.Body)
	if offset < 0 || offset > len(chars) {
		return nil, fmt.Errorf("读取位置无效")
	}
	end := offset + min(readDefault([]int{length}, 0, 5000), len(chars)-offset)
	m.Body = string(chars[offset:end])
	messages := []Message{m}
	if err := s.attachMessageArtifacts(messages); err != nil {
		return nil, err
	}
	if err := s.attachMessageReactions(messages); err != nil {
		return nil, err
	}
	return map[string]any{"message": messages[0], "next_offset": end, "has_more": end < len(chars)}, nil
}

func observationLabel(c Conversation, w Work) string {
	switch w.Kind {
	case "explore":
		return "正在回顾自己的关注"
	case "revisit":
		return "正在重新考虑先前的打算"
	}
	if c.Kind == "direct" {
		return "正在读你们的新消息"
	}
	return "正在读群里的新消息"
}
