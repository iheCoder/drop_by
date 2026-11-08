package dropby

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// PersonalActivity 只保存本人近期在意的事情，不建立另一套人格或群任务。
// 时钟提供活动机会，不决定必须说话，也不累计发言次数。
type PersonalActivity struct {
	Body      string `json:"body"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Store) initPersonalActivity() error {
	// 探索和作品可以先属于一个人，还没有分享对象。用 NULL 表达这个归属，
	// 不创建隐藏房间或伪造用户消息。旧行、索引和外键关系完整保留。
	for _, table := range []string{"work", "artifacts"} {
		var required int
		if err := s.db.QueryRow(`SELECT "notnull" FROM pragma_table_info(?) WHERE name='conversation_id'`, table).Scan(&required); err != nil {
			return err
		}
		if required == 0 {
			continue
		}
		var definition string
		if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&definition); err != nil {
			return err
		}
		rows, err := s.db.Query(`SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name=? AND sql IS NOT NULL`, table)
		if err != nil {
			return err
		}
		var indexes []string
		for rows.Next() {
			var index string
			if err = rows.Scan(&index); err != nil {
				rows.Close()
				return err
			}
			indexes = append(indexes, index)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		definition = strings.Replace(definition, "conversation_id TEXT NOT NULL REFERENCES conversations(id)", "conversation_id TEXT REFERENCES conversations(id)", 1)
		if _, err = s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
			return err
		}
		err = func() error {
			tx, err := s.db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			temporary := table + "_personal_migration"
			if _, err = tx.Exec("CREATE TABLE " + temporary + definition[strings.Index(definition, "("):]); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO " + temporary + " SELECT * FROM " + table); err != nil {
				return err
			}
			if _, err = tx.Exec("DROP TABLE " + table); err != nil {
				return err
			}
			if _, err = tx.Exec("ALTER TABLE " + temporary + " RENAME TO " + table); err != nil {
				return err
			}
			for _, index := range indexes {
				if _, err = tx.Exec(index); err != nil {
					return err
				}
			}
			return tx.Commit()
		}()
		_, fkErr := s.db.Exec(`PRAGMA foreign_keys=ON`)
		if err != nil {
			return err
		}
		if fkErr != nil {
			return fkErr
		}
	}
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS personal_activity(
 member_id TEXT PRIMARY KEY REFERENCES members(id), body TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '', next_at INTEGER NOT NULL DEFAULT 0);
 INSERT OR IGNORE INTO personal_activity(member_id) SELECT id FROM members;
 UPDATE work SET status='superseded' WHERE kind='explore' AND member_id='' AND status IN ('pending','waiting_budget');`)
	return err
}

func (s *Store) personalActivity(member string) (PersonalActivity, error) {
	var a PersonalActivity
	err := s.db.QueryRow(`SELECT body,updated_at FROM personal_activity WHERE member_id=?`, member).Scan(&a.Body, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		err = nil
	}
	return a, err
}

// 人和房间是可以交流的对象，不默认携带所有聊天原文。成员想去哪里交流，
// 再用 read_conversation 看现场；个人方向不被某个房间的旧话题占满。
func (s *Store) memberRooms(member string) ([]Conversation, error) {
	rows, err := s.db.Query(`SELECT conversation_id FROM participants WHERE member_id=? ORDER BY rowid`, member)
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
	rooms := []Conversation{}
	for _, id := range ids {
		c, err := s.Conversation(id)
		if err != nil {
			return nil, err
		}
		c.Avatar = ""
		c.LastMessage = ""
		rooms = append(rooms, c)
	}
	return rooms, nil
}

func (s *Store) personalConversation(member string) (Conversation, error) {
	if _, err := s.Member(member); err != nil {
		return Conversation{}, err
	}
	rooms, err := s.memberRooms(member)
	if err != nil {
		return Conversation{}, err
	}
	c := Conversation{Kind: "personal", MemberIDs: []string{member}}
	for _, room := range rooms {
		for _, id := range room.MemberIDs {
			if !contains(c.MemberIDs, id) {
				c.MemberIDs = append(c.MemberIDs, id)
			}
		}
	}
	return c, nil
}

func (s *Store) updatePersonalActivity(member string, body *string) error {
	if body == nil {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO personal_activity(member_id,body,updated_at) VALUES(?,?,?)
 ON CONFLICT(member_id) DO UPDATE SET body=excluded.body,updated_at=excluded.updated_at`, member, strings.TrimSpace(*body), now())
	return err
}

// 当前关注和相关关系以有界索引进入上下文，完整经历和行动保留回读入口。
// 不把所有长期记忆塞进每一次活动。
func (e *Engine) personalContext(m Member, w Work) (map[string]any, error) {
	a, err := e.Store.personalActivity(m.ID)
	if err != nil {
		return nil, err
	}
	rooms, err := e.Store.memberRooms(m.ID)
	if err != nil {
		return nil, err
	}
	people := []map[string]string{{"id": "user", "name": "你（真人）"}}
	known := map[string]bool{m.ID: true}
	destinations := []map[string]any{}
	for _, room := range rooms {
		destinations = append(destinations, map[string]any{"id": room.ID, "name": room.Name, "kind": room.Kind, "member_ids": room.MemberIDs, "discussion_paused": room.DiscussionPaused})
		for _, id := range room.MemberIDs {
			if known[id] {
				continue
			}
			other, err := e.Store.Member(id)
			if err != nil {
				return nil, err
			}
			people = append(people, map[string]string{"id": id, "name": other.Name})
			known[id] = true
		}
	}
	// 个人活动同样不自动加载旧经历、工具索引和工作正文，按需使用已有读取工具。
	return map[string]any{"time": now(), "local_time": time.Now().Format(time.RFC3339), "self": map[string]any{"id": m.ID, "name": m.Name, "profile_revision": m.ProfileRevision, "tools": m.Tools}, "personal_focus": PersonalActivity{Body: a.Body, UpdatedAt: a.UpdatedAt}, "people": people, "conversations": destinations, "trigger": map[string]string{"id": w.ID, "kind": w.Kind}}, nil
}

func (s *Store) personalToolRuns(member string) ([]ToolRun, error) {
	rows, err := s.db.Query(`SELECT id,work_id,member_id,name,arguments,result,status,created_at FROM tool_runs WHERE conversation_id='' AND member_id=? ORDER BY rowid DESC`, member)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []ToolRun
	for rows.Next() {
		var r ToolRun
		var args string
		if err = rows.Scan(&r.ID, &r.WorkID, &r.MemberID, &r.Name, &args, &r.Result, &r.Status, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Arguments = []byte(args)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func (s *Store) relationshipMemories(member, room string, subjects []string) ([]Memory, error) {
	rows, err := s.db.Query(memorySelect+` WHERE m.member_id=?
 AND EXISTS(SELECT 1 FROM json_each(d.subject_ids) WHERE value IN (SELECT value FROM json_each(?)))
 ORDER BY m.updated_at DESC,m.rowid DESC`, member, asJSON(subjects))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}

		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *Store) accessibleConversation(member, id string) (Conversation, error) {
	c, err := s.Conversation(id)
	if err != nil {
		return c, err
	}
	if !contains(c.MemberIDs, member) {
		return c, fmt.Errorf("你不在这个聊天中")
	}
	return c, nil
}

// 用户可在朋友动态看到个人行动；它们不被伪装成当前群的委托。
func (s *Store) personalWorks(members []string) ([]Work, error) {
	rows, err := s.db.Query(`SELECT `+workColumns+` FROM work WHERE conversation_id IS NULL AND member_id IN(SELECT value FROM json_each(?)) ORDER BY created_at DESC LIMIT 50`, asJSON(members))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []Work{}
	for rows.Next() {
		job, err := scanWork(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) personalFocuses() (map[string]PersonalActivity, error) {
	rows, err := s.db.Query(`SELECT member_id,body,updated_at FROM personal_activity WHERE body<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	focus := map[string]PersonalActivity{}
	for rows.Next() {
		var id string
		var a PersonalActivity
		if err := rows.Scan(&id, &a.Body, &a.UpdatedAt); err != nil {
			return nil, err
		}
		focus[id] = a
	}
	return focus, rows.Err()
}
