package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 保留原 memories 表及已有数据。附表只补足笔记的出处和修订关系，旧笔记
// 视为第 0 版，首次修改时才存入版本档案；不会凭空给旧数据制造依据。
func (s *Store) initMemories() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS memory_details(
 memory_id TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL, subject_ids TEXT NOT NULL, source_ids TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS memory_versions(
 memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL, snapshot TEXT NOT NULL, PRIMARY KEY(memory_id,revision));
CREATE INDEX IF NOT EXISTS memories_owner_scope ON memories(member_id,conversation_id);`)
	return err
}

const memorySelect = `SELECT m.id,m.member_id,m.conversation_id,m.body,m.source_id,m.updated_at,
 coalesce(d.revision,0),coalesce(d.subject_ids,'[]'),coalesce(d.source_ids,'[]')
 FROM memories m LEFT JOIN memory_details d ON d.memory_id=m.id`

func scanMemory(row interface{ Scan(...any) error }, prefix ...any) (Memory, error) {
	var m Memory
	var subjects, sources string
	fields := append(prefix, &m.ID, &m.MemberID, &m.ConversationID, &m.Body, &m.SourceID, &m.UpdatedAt,
		&m.Revision, &subjects, &sources)
	err := row.Scan(fields...)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal([]byte(subjects), &m.SubjectIDs); err != nil {
		return m, err
	}
	if err := json.Unmarshal([]byte(sources), &m.SourceIDs); err != nil {
		return m, err
	}
	if len(m.SourceIDs) == 0 && m.SourceID != "" {
		m.SourceIDs = []string{m.SourceID}
	}
	return m, nil
}

func (s *Store) memory(member, room, id string) (Memory, error) {
	m, err := scanMemory(s.db.QueryRow(memorySelect+` WHERE m.id=? AND m.member_id=? AND (m.conversation_id=? OR m.conversation_id='')`, id, member, room))
	if errors.Is(err, sql.ErrNoRows) {
		return m, fmt.Errorf("当前没有权限读取这条经历，或这条经历已被删除")
	}
	return m, err
}

// MemoryEvidence 保留作者、来源会话与实际原文，避免将他人的转述改写为自己
// 的亲历。其他会话只返回来源标识；携带个人理解不等于获得跨会话原文权限。
type MemoryEvidence struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	AuthorID       string `json:"author_id"`
	Kind           string `json:"kind"`
	Body           string `json:"body,omitempty"`
}

func (s *Store) memoryEvidence(member, room string, ids, inherited []string) ([]MemoryEvidence, error) {
	result := []MemoryEvidence{}
	for _, id := range ids {
		e := MemoryEvidence{ID: id}
		err := s.db.QueryRow(`SELECT conversation_id,author_id,body FROM messages WHERE id=?`, id).
			Scan(&e.ConversationID, &e.AuthorID, &e.Body)
		e.Kind = "message"
		if errors.Is(err, sql.ErrNoRows) {
			var name, status string
			err = s.db.QueryRow(`SELECT conversation_id,member_id,name,result,status FROM tool_runs WHERE id=?`, id).
				Scan(&e.ConversationID, &e.AuthorID, &name, &e.Body, &status)
			e.Kind = "tool:" + name
			if err == nil && (status != "succeeded" || name == "remember" || name == "read_memory" || name == "search_memory" || name == "read_tool_result") {
				return nil, fmt.Errorf("经历依据需要原始消息或成功行动；失败、笔记及其复制不能作为新的事实依据：%s", id)
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			// 无真人触发的自主工作也有真实来由，但工作计划不能证明已经完成。
			err = s.db.QueryRow(`SELECT conversation_id,member_id,instruction || char(10) || '实际状态：' || status || char(10) || result FROM work WHERE id=? AND member_id=?`, id, member).
				Scan(&e.ConversationID, &e.AuthorID, &e.Body)
			e.Kind = "work"
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("找不到经历的真实来源：%s", id)
		}
		if err != nil {
			return nil, err
		}
		if e.ConversationID != room {
			if !contains(inherited, id) {
				return nil, fmt.Errorf("不能用未携带的其他聊天记录形成经历：%s", id)
			}
			e.Body = "" // 仍可解释出处，但不借个人笔记读取别的群或私聊原文。
		} else {
			e.Body = contextExcerpt(e.Body, 1800)
		}
		result = append(result, e)
	}
	return result, nil
}

func (s *Store) readMemory(member, room, id string, revision *int) (any, error) {
	m, err := s.memory(member, room, id)
	if err != nil {
		return nil, err
	}
	current := m.Revision
	if revision != nil && *revision != current {
		var raw string
		if err := s.db.QueryRow(`SELECT snapshot FROM memory_versions WHERE memory_id=? AND revision=?`, id, *revision).Scan(&raw); err != nil {
			return nil, fmt.Errorf("没有这版经历记录")
		}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		// 缩小可见范围之后，旧版本不能绕过新的会话边界。
		if m.ConversationID != "" && m.ConversationID != room {
			return nil, fmt.Errorf("旧版本属于其他聊天")
		}
	}
	evidence, err := s.memoryEvidence(member, room, m.SourceIDs, m.SourceIDs)
	if err != nil && m.Revision != 0 {
		return nil, err
	}
	return map[string]any{"memory": m, "evidence": evidence, "current_revision": current,
		"historical": current != m.Revision, "note": "旧版本仅用于核对变化，不能当作当前结论；来源相同的转述不是多份独立证据。"}, nil
}

type memoryWrite struct {
	ID         string    `json:"id"`
	Revision   *int      `json:"revision"`
	Scope      string    `json:"scope"`
	Body       string    `json:"body"`
	SubjectIDs *[]string `json:"subject_ids"`
	SourceIDs  []string  `json:"source_ids"`
}

type memoryReviewKey struct{}

func (t *Toolbox) remember(ctx context.Context, c Conversation, m Member, w Work, args memoryWrite) (any, error) {
	var previous Memory
	var err error
	if args.ID != "" {
		previous, err = t.Store.memory(m.ID, c.ID, args.ID)
		if err != nil {
			return nil, err
		}
		// 更新者必须确认自己读到的版本；另一个会话刚保存的纠正不能被老草稿覆盖。
		if args.Revision == nil || *args.Revision != previous.Revision {
			return nil, fmt.Errorf("经历已有版本 %d；请 read_memory 后带 revision 更新原 ID", previous.Revision)
		}
	}
	next := previous
	if next.ID == "" {
		next = Memory{ID: newID("memory"), MemberID: m.ID, ConversationID: c.ID}
	}
	switch args.Scope {
	case "": // 更新时沿用原范围，不因省略 scope 把个人经历悄悄搬回当前房间。
	case "member":
		next.ConversationID = ""
	case "conversation":
		next.ConversationID = c.ID
	default:
		return nil, fmt.Errorf("记忆范围无效")
	}
	next.Body = strings.TrimSpace(args.Body)
	if next.Body == "" && previous.ID == "" {
		return nil, fmt.Errorf("经历需要非空正文；删除已有笔记需提供 ID 和 revision")
	}
	if args.SubjectIDs != nil {
		next.SubjectIDs = uniqueMemoryIDs(*args.SubjectIDs)
	}
	for _, id := range next.SubjectIDs {
		if id != "user" && id != m.ID && !contains(c.MemberIDs, id) && !contains(previous.SubjectIDs, id) {
			return nil, fmt.Errorf("经历涉及的人必须来自当前聊天或原有经历：%s", id)
		}
	}
	if next.Body != "" {
		// 没有显式列出来源时仍绑定本次真实触发，兼容原有工具调用。原来的
		// 依据留在版本档案；本版来源去重，绝不按转述次数增加“信任分”。
		next.SourceIDs = uniqueMemoryIDs(args.SourceIDs)
		if len(next.SourceIDs) == 0 {
			source := w.SourceID
			if source == "" {
				source = w.ID
			}
			next.SourceIDs = uniqueMemoryIDs([]string{source})
		}
		if len(next.SourceIDs) == 0 {
			return nil, fmt.Errorf("经历需引用真实消息或行动来源")
		}
		evidence, err := t.Store.memoryEvidence(m.ID, c.ID, next.SourceIDs, previous.SourceIDs)
		if err != nil {
			return nil, err
		}
		next.SourceID = next.SourceIDs[0]
		if t.Review != nil {
			// 核对器直接看到引用原文，不能只根据最近聊天猜测旧经历是否真实。
			review := map[string]any{"previous": previous, "sources": evidence, "portable": next.ConversationID == ""}
			ctx = context.WithValue(ctx, memoryReviewKey{}, review)
			next.Body, err = t.Review(ctx, c, m, w, "经历笔记", next.Body)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(next.Body) == "" {
				return nil, fmt.Errorf("核对后的经历为空或过长，本次未保存")
			}
		}
	}
	next.Revision, next.UpdatedAt = previous.Revision+1, now()
	err = t.Store.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if previous.ID != "" {
			var revision int
			if err := tx.QueryRow(`SELECT coalesce(d.revision,0) FROM memories m LEFT JOIN memory_details d ON d.memory_id=m.id WHERE m.id=? AND m.member_id=?`, previous.ID, m.ID).Scan(&revision); err != nil {
				return err
			}
			if revision != previous.Revision {
				return fmt.Errorf("经历在核对期间有了新修正，请重新读取后再更新")
			}
			if next.Body == "" {
				_, err := tx.Exec(`DELETE FROM memories WHERE id=?`, next.ID)
				return err // 附表级联删除；聊天原文不因删除笔记而消失。
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO memory_versions VALUES(?,?,?)`, previous.ID, previous.Revision, asJSON(previous)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET conversation_id=excluded.conversation_id,body=excluded.body,source_id=excluded.source_id,updated_at=excluded.updated_at`,
			next.ID, m.ID, next.ConversationID, next.Body, next.SourceID, next.UpdatedAt)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO memory_details VALUES(?,?,?,?) ON CONFLICT(memory_id) DO UPDATE SET revision=excluded.revision,subject_ids=excluded.subject_ids,source_ids=excluded.source_ids`,
			next.ID, next.Revision, asJSON(next.SubjectIDs), asJSON(next.SourceIDs)); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO memory_versions VALUES(?,?,?)`, next.ID, next.Revision, asJSON(next))
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"memory_id": next.ID, "revision": next.Revision, "memory": next, "deleted": next.Body == ""}, nil
}

func uniqueMemoryIDs(ids []string) []string {
	result := []string{}
	for _, id := range ids {
		if id != "" && !contains(result, id) {
			result = append(result, id)
		}
	}
	return result
}
