package dropby

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	lock *os.File
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// 同一份聊天数据只能由一个服务进程调度。进程退出后内核会释放锁，
	// 不用猜测过期时间，也不会把另一个运行中的服务误当成崩溃恢复。
	lock, err := os.OpenFile(filepath.Join(dir, "server.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("这份数据已有服务在运行，请使用现有服务或选择另一数据目录")
	}
	opened := false
	defer func() {
		if !opened {
			lock.Close()
		}
	}()
	path := filepath.Join(dir, "dropby.sqlite")
	// 先创建为仅当前用户可读，避免聊天记录继承宽松的默认权限。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: lock}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS members(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS conversations(id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 0,
 discussion_paused INTEGER NOT NULL DEFAULT 0, research_paused INTEGER NOT NULL DEFAULT 0, guidance TEXT NOT NULL DEFAULT '', next_explore INTEGER NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS participants(conversation_id TEXT NOT NULL REFERENCES conversations(id), member_id TEXT NOT NULL REFERENCES members(id), PRIMARY KEY(conversation_id,member_id));
CREATE TABLE IF NOT EXISTS messages(seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, conversation_id TEXT NOT NULL REFERENCES conversations(id), author_id TEXT NOT NULL,
 body TEXT NOT NULL, reply_to TEXT NOT NULL DEFAULT '', mentions TEXT NOT NULL DEFAULT '[]', evidence TEXT NOT NULL DEFAULT '[]', notify INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS messages_room_seq ON messages(conversation_id,seq);
CREATE TABLE IF NOT EXISTS memories(id TEXT PRIMARY KEY, member_id TEXT NOT NULL REFERENCES members(id), conversation_id TEXT NOT NULL, body TEXT NOT NULL, source_id TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS work(id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id), member_id TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL,
 instruction TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', source_id TEXT NOT NULL, status TEXT NOT NULL, due_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, result TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS work_due ON work(status,due_at);
CREATE TABLE IF NOT EXISTS tool_runs(id TEXT PRIMARY KEY, work_id TEXT NOT NULL REFERENCES work(id), conversation_id TEXT NOT NULL, member_id TEXT NOT NULL, name TEXT NOT NULL, arguments TEXT NOT NULL,
 result TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS artifacts(id TEXT NOT NULL, version INTEGER NOT NULL, conversation_id TEXT NOT NULL REFERENCES conversations(id), member_id TEXT NOT NULL,
 title TEXT NOT NULL, content TEXT NOT NULL, source_id TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(id,version));
CREATE TABLE IF NOT EXISTS usage(day TEXT PRIMARY KEY, calls INTEGER NOT NULL DEFAULT 0, cost_micros INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0);
`)
	if err != nil {
		db.Close()
		return nil, err
	}
	// 进程退出不等于工作完成。只读网络行动可再次执行，已保存的工具记录会回到上下文。
	_, err = db.Exec(`UPDATE work SET status='pending', result=CASE WHEN result='' THEN '上次运行因服务退出中断；继续前请检查已有工具结果和最新聊天。' ELSE result END WHERE status='running'`)
	if err != nil {
		db.Close()
		return nil, err
	}
	opened = true
	return s, nil
}
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lock != nil {
		s.lock.Close()
	}
	return err
}

func (s *Store) Members() ([]Member, error) {
	rows, e := s.db.Query(`SELECT data FROM members ORDER BY rowid`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []Member{}
	for rows.Next() {
		var raw string
		var m Member
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &m); e != nil {
			return nil, e
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
func (s *Store) Member(id string) (Member, error) {
	var raw string
	var m Member
	e := s.db.QueryRow(`SELECT data FROM members WHERE id=?`, id).Scan(&raw)
	if e != nil {
		return m, e
	}
	e = json.Unmarshal([]byte(raw), &m)
	return m, e
}
func (s *Store) AddMember(m Member) error {
	_, e := s.db.Exec(`INSERT INTO members(id,data) VALUES(?,?)`, m.ID, asJSON(m))
	return e
}

func (s *Store) CreateConversation(name, kind string, ids []string, interval time.Duration) (Conversation, error) {
	c := Conversation{ID: newID("room"), Name: name, Kind: kind, MemberIDs: ids, UpdatedAt: now()}
	tx, e := s.db.Begin()
	if e != nil {
		return c, e
	}
	defer tx.Rollback()
	// 同一成员与当前真人只有一条私聊，连续经历不会被重复创建的会话割裂。
	if kind == "direct" && len(ids) == 1 {
		var existing string
		e = tx.QueryRow(`SELECT c.id FROM conversations c JOIN participants p ON p.conversation_id=c.id WHERE c.kind='direct' AND p.member_id=?`, ids[0]).Scan(&existing)
		if e == nil {
			tx.Rollback()
			return s.Conversation(existing)
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return c, e
		}
	}
	_, e = tx.Exec(`INSERT INTO conversations(id,name,kind,next_explore,updated_at) VALUES(?,?,?,?,?)`, c.ID, name, kind, time.Now().Add(interval).UnixMilli(), c.UpdatedAt)
	if e != nil {
		return c, e
	}
	for _, id := range ids {
		if _, e = tx.Exec(`INSERT INTO participants VALUES(?,?)`, c.ID, id); e != nil {
			return c, e
		}
	}
	return c, tx.Commit()
}

func (s *Store) Conversation(id string) (Conversation, error) {
	var c Conversation
	e := s.db.QueryRow(`SELECT id,name,kind,revision,discussion_paused,research_paused,guidance,updated_at FROM conversations WHERE id=?`, id).Scan(&c.ID, &c.Name, &c.Kind, &c.Revision, &c.DiscussionPaused, &c.ResearchPaused, &c.Guidance, &c.UpdatedAt)
	if e != nil {
		return c, e
	}
	rows, e := s.db.Query(`SELECT member_id FROM participants WHERE conversation_id=? ORDER BY rowid`, id)
	if e != nil {
		return c, e
	}
	defer rows.Close()
	c.MemberIDs = []string{}
	for rows.Next() {
		var v string
		if e = rows.Scan(&v); e != nil {
			return c, e
		}
		c.MemberIDs = append(c.MemberIDs, v)
	}
	return c, rows.Err()
}
func (s *Store) Conversations() ([]Conversation, error) {
	rows, e := s.db.Query(`SELECT id FROM conversations ORDER BY updated_at DESC`)
	if e != nil {
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	result := []Conversation{}
	for _, id := range ids {
		c, e := s.Conversation(id)
		if e != nil {
			return nil, e
		}
		e = s.db.QueryRow(`SELECT body,seq FROM messages WHERE conversation_id=? ORDER BY seq DESC LIMIT 1`, id).Scan(&c.LastMessage, &c.LastSeq)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		result = append(result, c)
	}
	return result, nil
}

func (s *Store) Messages(room string, before int64, limit int) ([]Message, error) {
	if before <= 0 {
		before = 1 << 62
	}
	rows, e := s.db.Query(`SELECT seq,id,conversation_id,author_id,body,reply_to,mentions,evidence,notify,created_at FROM (SELECT * FROM messages WHERE conversation_id=? AND seq<? ORDER BY seq DESC LIMIT ?) ORDER BY seq`, room, before, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []Message{}
	for rows.Next() {
		var m Message
		var mentions, evidence string
		if e = rows.Scan(&m.Seq, &m.ID, &m.ConversationID, &m.AuthorID, &m.Body, &m.ReplyTo, &mentions, &evidence, &m.Notify, &m.CreatedAt); e != nil {
			return nil, e
		}
		json.Unmarshal([]byte(mentions), &m.Mentions)
		json.Unmarshal([]byte(evidence), &m.Evidence)
		result = append(result, m)
	}
	return result, rows.Err()
}

func insertMessage(tx *sql.Tx, m Message) error {
	_, e := tx.Exec(`INSERT INTO messages(id,conversation_id,author_id,body,reply_to,mentions,evidence,notify,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, m.ID, m.ConversationID, m.AuthorID, m.Body, m.ReplyTo, asJSON(m.Mentions), asJSON(m.Evidence), m.Notify, m.CreatedAt)
	if e != nil {
		return e
	}
	_, e = tx.Exec(`UPDATE conversations SET updated_at=? WHERE id=?`, m.CreatedAt, m.ConversationID)
	return e
}

// 新用户消息、修订号和待处理事件在同一事务落盘。生成过程只能提交到它读过的修订，
// 即使 HTTP 取消来不及生效，也不会把过时回复发到群里。
func (s *Store) AddUserMessage(m Message, interval time.Duration) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if m.ReplyTo != "" {
		var exists int
		e = tx.QueryRow(`SELECT count(*) FROM messages WHERE id=? AND conversation_id=?`, m.ReplyTo, m.ConversationID).Scan(&exists)
		if e != nil {
			return e
		}
		if exists == 0 {
			return fmt.Errorf("引用消息不属于当前聊天")
		}
	}
	if e = insertMessage(tx, m); e != nil {
		return e
	}
	if _, e = tx.Exec(`UPDATE conversations SET revision=revision+1,next_explore=? WHERE id=?`, time.Now().Add(interval).UnixMilli(), m.ConversationID); e != nil {
		return e
	}
	// 极明确的停止在接收消息的事务中立即生效，不等待模型、预算或后台轮询。
	// 这也让正在执行的研究在看到新修订后无法发布旧结果。
	if discussion, research := explicitStop(m.Body); discussion || research {
		if discussion {
			if _, e = tx.Exec(`UPDATE conversations SET discussion_paused=1 WHERE id=?`, m.ConversationID); e != nil {
				return e
			}
		}
		if research {
			if _, e = tx.Exec(`UPDATE conversations SET research_paused=1 WHERE id=?`, m.ConversationID); e != nil {
				return e
			}
			if _, e = tx.Exec(`UPDATE work SET status='stopped' WHERE conversation_id=? AND kind='research' AND status IN ('pending','running','waiting_budget','paused')`, m.ConversationID); e != nil {
				return e
			}
		}
	}
	if _, e = tx.Exec(`UPDATE work SET status='superseded' WHERE conversation_id=? AND kind!='research' AND status IN ('pending','running','waiting_budget')`, m.ConversationID); e != nil {
		return e
	}
	w := Work{ID: newID("work"), ConversationID: m.ConversationID, Kind: "chat", Instruction: m.Body, SourceID: m.ID, Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
	if e = insertWork(tx, w); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) Guard(room string, revision int64, fn func(*sql.Tx) error) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var current int64
	if e = tx.QueryRow(`SELECT revision FROM conversations WHERE id=?`, room).Scan(&current); e != nil {
		return e
	}
	if current != revision {
		return ErrStale
	}
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Publish(m Message, revision int64) error {
	return s.Guard(m.ConversationID, revision, func(tx *sql.Tx) error {
		var paused bool
		if e := tx.QueryRow(`SELECT discussion_paused FROM conversations WHERE id=?`, m.ConversationID).Scan(&paused); e != nil {
			return e
		}
		if paused && m.AuthorID != "system" {
			return ErrStale
		}
		// AI 之间接话也可引用原消息，但不能把另一场景的原文带进来。
		if m.ReplyTo != "" {
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM messages WHERE id=? AND conversation_id=?`, m.ReplyTo, m.ConversationID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("引用的原消息不在当前聊天中")
			}
		}
		return insertMessage(tx, m)
	})
}
func (s *Store) SystemMessage(room, id, body string) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var n int
	if e = tx.QueryRow(`SELECT count(*) FROM messages WHERE id=?`, id).Scan(&n); e != nil {
		return e
	}
	if n > 0 {
		return nil
	}
	if e = insertMessage(tx, Message{ID: id, ConversationID: room, AuthorID: "system", Body: body, CreatedAt: now()}); e != nil {
		return e
	}
	return tx.Commit()
}

func insertWork(tx *sql.Tx, w Work) error {
	_, e := tx.Exec(`INSERT INTO work(id,conversation_id,member_id,kind,instruction,reason,source_id,status,due_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, w.ID, w.ConversationID, w.MemberID, w.Kind, w.Instruction, w.Reason, w.SourceID, w.Status, w.DueAt, w.CreatedAt)
	return e
}

const workColumns = `id,conversation_id,member_id,kind,instruction,reason,source_id,status,due_at,attempts,result,created_at`

func scanWork(rows interface{ Scan(...any) error }) (Work, error) {
	var w Work
	e := rows.Scan(&w.ID, &w.ConversationID, &w.MemberID, &w.Kind, &w.Instruction, &w.Reason, &w.SourceID, &w.Status, &w.DueAt, &w.Attempts, &w.Result, &w.CreatedAt)
	return w, e
}
func (s *Store) Works(room string) ([]Work, error) {
	rows, e := s.db.Query(`SELECT `+workColumns+` FROM work WHERE conversation_id=? ORDER BY created_at DESC LIMIT 50`, room)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	list := []Work{}
	for rows.Next() {
		w, e := scanWork(rows)
		if e != nil {
			return nil, e
		}
		list = append(list, w)
	}
	return list, rows.Err()
}
func (s *Store) DueWorks() ([]Work, error) {
	rows, e := s.db.Query(`SELECT `+workColumns+` FROM work WHERE status IN ('pending','waiting_budget') AND due_at<=? ORDER BY CASE kind WHEN 'chat' THEN 0 WHEN 'research' THEN 1 ELSE 2 END,created_at LIMIT 100`, time.Now().UnixMilli())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	list := []Work{}
	for rows.Next() {
		w, e := scanWork(rows)
		if e != nil {
			return nil, e
		}
		list = append(list, w)
	}
	return list, rows.Err()
}
func (s *Store) Claim(id string) (bool, error) {
	r, e := s.db.Exec(`UPDATE work SET status='running',attempts=attempts+1 WHERE id=? AND status IN ('pending','waiting_budget')`, id)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) Finish(w Work, status, result string, due time.Time) error {
	_, e := s.db.Exec(`UPDATE work SET status=?,result=CASE WHEN ?='' THEN result ELSE ? END,due_at=? WHERE id=? AND status='running'`, status, result, result, due.UnixMilli(), w.ID)
	return e
}

// 定时器只提供一次判断机会。没有内容可推进时，成员可安静地结束这次机会。
func (s *Store) ScheduleExploration(interval time.Duration) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.Query(`SELECT c.id FROM conversations c WHERE c.next_explore<=? AND c.research_paused=0 AND EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id) AND NOT EXISTS(SELECT 1 FROM work w WHERE w.conversation_id=c.id AND w.status IN ('pending','running','waiting_budget'))`, time.Now().UnixMilli())
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		w := Work{ID: newID("work"), ConversationID: id, Kind: "explore", Instruction: "结合自己的兴趣、最近共同经历和已有工作，看看是否有值得实际探索的方向。没有合适的事情就保持安静。不是催促回复，也不是定时问候。", Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
		if e = insertWork(tx, w); e != nil {
			return e
		}
		if _, e = tx.Exec(`UPDATE conversations SET next_explore=? WHERE id=?`, time.Now().Add(interval).UnixMilli(), id); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func (s *Store) Memories(member, room string) ([]Memory, error) {
	rows, e := s.db.Query(`SELECT id,member_id,conversation_id,body,source_id,updated_at FROM memories WHERE member_id=? AND (conversation_id=? OR conversation_id='') ORDER BY updated_at DESC LIMIT 30`, member, room)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	list := []Memory{}
	for rows.Next() {
		var m Memory
		if e = rows.Scan(&m.ID, &m.MemberID, &m.ConversationID, &m.Body, &m.SourceID, &m.UpdatedAt); e != nil {
			return nil, e
		}
		list = append(list, m)
	}
	return list, rows.Err()
}
func (s *Store) ToolRuns(room, work string) ([]ToolRun, error) {
	rows, e := s.db.Query(`SELECT id,work_id,conversation_id,member_id,name,arguments,result,status,created_at FROM tool_runs WHERE conversation_id=? AND (?='' OR work_id=?) ORDER BY created_at DESC LIMIT 30`, room, work, work)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	list := []ToolRun{}
	for rows.Next() {
		var r ToolRun
		var args string
		if e = rows.Scan(&r.ID, &r.WorkID, &r.ConversationID, &r.MemberID, &r.Name, &args, &r.Result, &r.Status, &r.CreatedAt); e != nil {
			return nil, e
		}
		r.Arguments = json.RawMessage(args)
		list = append(list, r)
	}
	return list, rows.Err()
}
func (s *Store) RecordTool(r ToolRun) error {
	_, e := s.db.Exec(`INSERT INTO tool_runs VALUES(?,?,?,?,?,?,?,?,?)`, r.ID, r.WorkID, r.ConversationID, r.MemberID, r.Name, string(r.Arguments), r.Result, r.Status, r.CreatedAt)
	return e
}
func (s *Store) Artifacts(room string) ([]Artifact, error) {
	rows, e := s.db.Query(`SELECT a.id,a.conversation_id,a.member_id,a.title,a.content,a.version,a.source_id,a.created_at FROM artifacts a WHERE conversation_id=? AND version=(SELECT max(version) FROM artifacts b WHERE b.id=a.id) ORDER BY created_at DESC`, room)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []Artifact{}
	for rows.Next() {
		var a Artifact
		if e = rows.Scan(&a.ID, &a.ConversationID, &a.MemberID, &a.Title, &a.Content, &a.Version, &a.SourceID, &a.CreatedAt); e != nil {
			return nil, e
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
func (s *Store) ArtifactVersions(id string) ([]Artifact, error) {
	rows, e := s.db.Query(`SELECT id,conversation_id,member_id,title,content,version,source_id,created_at FROM artifacts WHERE id=? ORDER BY version DESC`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	list := []Artifact{}
	for rows.Next() {
		var a Artifact
		if e = rows.Scan(&a.ID, &a.ConversationID, &a.MemberID, &a.Title, &a.Content, &a.Version, &a.SourceID, &a.CreatedAt); e != nil {
			return nil, e
		}
		list = append(list, a)
	}
	return list, rows.Err()
}
