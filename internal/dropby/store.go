package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
CREATE TABLE IF NOT EXISTS message_reactions(message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 emoji TEXT NOT NULL, actor_id TEXT NOT NULL, PRIMARY KEY(message_id,emoji,actor_id));
CREATE TABLE IF NOT EXISTS message_questions(message_id TEXT PRIMARY KEY REFERENCES messages(id), question TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'open', response_id TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS memories(id TEXT PRIMARY KEY, member_id TEXT NOT NULL REFERENCES members(id), conversation_id TEXT NOT NULL, body TEXT NOT NULL, source_id TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS work(id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id), member_id TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL,
 instruction TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', source_id TEXT NOT NULL, status TEXT NOT NULL, due_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, result TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS work_due ON work(status,due_at);
CREATE TABLE IF NOT EXISTS participation(work_id TEXT NOT NULL REFERENCES work(id), member_id TEXT NOT NULL, action TEXT NOT NULL,
 reason TEXT NOT NULL, selected INTEGER NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS working_contexts(conversation_id TEXT NOT NULL, member_id TEXT NOT NULL REFERENCES members(id), seen INTEGER NOT NULL, messages TEXT NOT NULL, PRIMARY KEY(conversation_id,member_id));
CREATE TABLE IF NOT EXISTS action_checkpoints(id TEXT PRIMARY KEY, work_id TEXT NOT NULL REFERENCES work(id), member_id TEXT NOT NULL, revision INTEGER NOT NULL, seen INTEGER NOT NULL, messages TEXT NOT NULL, UNIQUE(work_id,member_id));
CREATE INDEX IF NOT EXISTS participation_work ON participation(work_id);
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
	if err = s.initArtifacts(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initProactivity(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initUsageCalls(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initCoordination(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initResponsibilities(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initIM(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initParticipation(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initMemories(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initPersonalActivity(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.initPersonality(); err != nil {
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
	return s.insertMember(m, nil)
}

// AddMemberAndRhythm 把人物和初始节律放进同一次提交。节律无效时成员也不会留下。
func (s *Store) AddMemberAndRhythm(m Member, rhythm wakeRhythm) error {
	return s.insertMember(m, &rhythm)
}

func (s *Store) insertMember(m Member, rhythm *wakeRhythm) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`INSERT INTO members(id,data) VALUES(?,?)`, m.ID, asJSON(m)); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT INTO personal_activity(member_id) VALUES(?)`, m.ID); e != nil {
		return e
	}
	if e = recordProfileVersion(tx, m, "created", "创建时的人物说明", nil); e != nil {
		return e
	}
	if rhythm != nil {
		if e = installWakeTx(tx, m.ID, *rhythm); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func (s *Store) CreateConversation(name, kind string, ids []string, interval time.Duration, portrait ...string) (Conversation, error) {
	c := Conversation{ID: newID("room"), Name: name, Kind: kind, MemberIDs: ids, UpdatedAt: now()}
	if kind == "group" && len(portrait) > 0 {
		c.Avatar = portrait[0]
	}
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
	// 新相识不必等真人先开口；一分钟后给成员第一次自主判断机会。
	_, e = tx.Exec(`INSERT INTO conversations(id,name,kind,next_explore,updated_at,avatar) VALUES(?,?,?,?,?,?)`, c.ID, name, kind, time.Now().Add(min(interval, time.Minute)).UnixMilli(), c.UpdatedAt, c.Avatar)
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
	e := s.db.QueryRow(`SELECT id,name,kind,revision,discussion_paused,research_paused,guidance,updated_at,coordinator_id,avatar FROM conversations WHERE id=?`, id).Scan(&c.ID, &c.Name, &c.Kind, &c.Revision, &c.DiscussionPaused, &c.ResearchPaused, &c.Guidance, &c.UpdatedAt, &c.CoordinatorID, &c.Avatar)
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
	e = rows.Err()
	rows.Close()
	if e != nil {
		return c, e
	}
	c.Responsibilities, e = s.Responsibilities(c.ID)
	if c.CoordinatorID != "" {
		c.Responsibilities = append(c.Responsibilities, Responsibility{ID: "legacy_" + c.ID, MemberID: c.CoordinatorID, Body: "用户此前在群设置中约定：需要一起做事时帮助分工与跟进。"})
	}
	return c, e
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
		e = s.db.QueryRow(`SELECT body,seq FROM messages WHERE conversation_id=? AND NOT (author_id='system' AND substr(id,1,7)='budget_') ORDER BY seq DESC LIMIT 1`, id).Scan(&c.LastMessage, &c.LastSeq)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		result = append(result, c)
	}
	return result, nil
}

func (s *Store) latestMessageSeq(ctx context.Context, room string) (int64, error) {
	var seq int64
	// 与会话详情保持相同的可见范围。空会话返回 0，已删除的会话则返回 ErrNoRows。
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((
		SELECT seq FROM messages WHERE conversation_id=c.id
		AND NOT (author_id='system' AND substr(id,1,7)='budget_')
		ORDER BY seq DESC LIMIT 1
	), 0) FROM conversations c WHERE c.id=?`, room).Scan(&seq)
	return seq, err
}

func (s *Store) Messages(room string, before int64, limit int) ([]Message, error) {
	return s.messagesAfter(room, 0, before, limit)
}

// 行动读取按已交付位置续接；limit=-1 使用 SQLite 的无条数上限查询。
func (s *Store) messagesAfter(room string, after, before int64, limit int) ([]Message, error) {
	if before <= 0 {
		before = 1 << 62
	}
	rows, e := s.db.Query(`SELECT m.seq,m.id,m.conversation_id,m.author_id,m.body,m.reply_to,m.mentions,m.evidence,m.notify,m.created_at,COALESCE(q.question,''),COALESCE(q.status,'') FROM (SELECT * FROM messages WHERE conversation_id=? AND seq>? AND seq<? AND NOT (author_id='system' AND substr(id,1,7)='budget_') ORDER BY seq DESC LIMIT ?) m LEFT JOIN message_questions q ON q.message_id=m.id ORDER BY m.seq`, room, after, before, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []Message{}
	for rows.Next() {
		var m Message
		var mentions, evidence, question string
		if e = rows.Scan(&m.Seq, &m.ID, &m.ConversationID, &m.AuthorID, &m.Body, &m.ReplyTo, &mentions, &evidence, &m.Notify, &m.CreatedAt, &question, &m.QuestionStatus); e != nil {
			return nil, e
		}
		json.Unmarshal([]byte(mentions), &m.Mentions)
		json.Unmarshal([]byte(evidence), &m.Evidence)
		if question != "" {
			if e = json.Unmarshal([]byte(question), &m.Question); e != nil {
				return nil, e
			}
		}
		result = append(result, m)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	// 当前连接池只开一条连接，先结束消息读取再解析成果引用，避免嵌套查询等待自己。
	rows.Close()
	if e = s.attachMessageArtifacts(result); e != nil {
		return nil, e
	}
	return result, s.attachMessageReactions(result)
}

func insertMessage(tx *sql.Tx, m Message) error {
	_, e := tx.Exec(`INSERT INTO messages(id,conversation_id,author_id,body,reply_to,mentions,evidence,notify,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, m.ID, m.ConversationID, m.AuthorID, m.Body, m.ReplyTo, asJSON(m.Mentions), asJSON(m.Evidence), m.Notify, m.CreatedAt)
	if e != nil {
		return e
	}
	if m.Question != nil {
		if _, e = tx.Exec(`INSERT INTO message_questions(message_id,question,status) VALUES(?,?,'open')`, m.ID, asJSON(m.Question)); e != nil {
			return e
		}
	}
	_, e = tx.Exec(`UPDATE conversations SET updated_at=? WHERE id=?`, m.CreatedAt, m.ConversationID)
	return e
}

// 普通新消息只追加待读内容与持久唤醒，不撤销正在形成的回复。
// 明确停止仍在事务内提升控制修订，使旧执行无法越过用户控制。
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
	// 发自己的话就继续聊天。立即收起选项，避免模型请求尚未完成时问题仍占住界面；
	// 自由回答是否属于这个问题，稍后由语义判断确认，绝不要求额外点“跳过”。
	if _, e = tx.Exec(`UPDATE message_questions SET status=CASE WHEN message_id=? THEN 'answered' ELSE 'dismissed' END,response_id=? WHERE status='open' AND message_id IN (SELECT id FROM messages WHERE conversation_id=?)`, m.ReplyTo, m.ID, m.ConversationID); e != nil {
		return e
	}
	// 极明确的停止在接收消息的事务中立即生效，不等待模型、预算或后台轮询。
	// 这也让正在执行的研究在看到新修订后无法发布旧结果。
	discussion, research := explicitStop(m.Body)
	// 真人重新接话恢复讨论，研究暂停保持原样；无需为这一既有行为调用模型。
	if !discussion {
		if _, e = tx.Exec(`UPDATE conversations SET discussion_paused=0 WHERE id=?`, m.ConversationID); e != nil {
			return e
		}
	}
	if discussion || research {
		if _, e = tx.Exec(`UPDATE conversations SET revision=revision+1 WHERE id=?`, m.ConversationID); e != nil {
			return e
		}
		if discussion {
			if _, e = tx.Exec(`UPDATE conversations SET discussion_paused=1 WHERE id=?`, m.ConversationID); e != nil {
				return e
			}
		}
		if research {
			if _, e = tx.Exec(`UPDATE conversations SET research_paused=1 WHERE id=?`, m.ConversationID); e != nil {
				return e
			}
			if _, e = tx.Exec(`UPDATE work SET status='stopped' WHERE conversation_id=? AND kind IN ('research','revisit') AND status IN ('pending','running','waiting_budget','paused')`, m.ConversationID); e != nil {
				return e
			}
		}
	}
	if _, e = tx.Exec(`UPDATE work SET status='superseded' WHERE conversation_id=? AND kind NOT IN ('research','revisit') AND (status IN ('pending','waiting_budget') OR (? AND status='running'))`, m.ConversationID, discussion || research); e != nil {
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
	if room == "" {
		if e = fn(tx); e != nil {
			return e
		}
		return tx.Commit()
	}
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
func (s *Store) Publish(m Message, revision int64, actionID ...string) error {
	return s.Guard(m.ConversationID, revision, func(tx *sql.Tx) error {
		if err := publishMessage(tx, m); err != nil {
			return err
		}
		if len(actionID) > 0 {
			_, err := tx.Exec("DELETE FROM action_checkpoints WHERE id=?", actionID[0])
			return err
		}
		return nil
	})
}

func publishMessage(tx *sql.Tx, m Message) error {
	var paused bool
	if e := tx.QueryRow(`SELECT discussion_paused FROM conversations WHERE id=?`, m.ConversationID).Scan(&paused); e != nil {
		return e
	}
	if paused && m.AuthorID != "system" {
		return ErrStale
	}
	// 成员离群后，旧任务即便读到了新的修订号也不能继续发言。
	if m.AuthorID != "user" && m.AuthorID != "system" {
		var present int
		if err := tx.QueryRow(`SELECT count(*) FROM participants WHERE conversation_id=? AND member_id=?`, m.ConversationID, m.AuthorID).Scan(&present); err != nil {
			return err
		}
		if present != 1 {
			return ErrStale
		}
	}
	// AI 之间接话也可引用原消息，但不能把另一场景的原文带进来。
	if m.ReplyTo != "" {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM messages WHERE id=? AND conversation_id=?`, m.ReplyTo, m.ConversationID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			// 引用只是定位辅助。成员输错 ID 时保留正文，不把一次可选引用变成发言失败。
			if m.AuthorID == "user" {
				return fmt.Errorf("引用的原消息不在当前聊天中")
			}
			m.ReplyTo = ""
		}
	}
	if err := insertMessage(tx, m); err != nil {
		return err
	}
	return nil
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
	if w.RootID == "" {
		w.RootID = w.ID
	}
	_, e := tx.Exec(`INSERT INTO work(id,conversation_id,member_id,kind,instruction,reason,source_id,status,due_at,created_at,root_id,autonomous,assigned_by,parent_id,first_speakers) VALUES(?,nullif(?,''),?,?,?,?,?,?,?,?,?,?,?,?,?)`, w.ID, w.ConversationID, w.MemberID, w.Kind, w.Instruction, w.Reason, w.SourceID, w.Status, w.DueAt, w.CreatedAt, w.RootID, w.Autonomous, w.AssignedBy, w.ParentID, asJSON(w.FirstSpeakers))
	return e
}

const workColumns = `id,coalesce(conversation_id,''),member_id,kind,instruction,reason,source_id,status,due_at,attempts,result,created_at,root_id,autonomous,assigned_by,parent_id,first_speakers`

func scanWork(rows interface{ Scan(...any) error }) (Work, error) {
	var w Work
	var audience string
	e := rows.Scan(&w.ID, &w.ConversationID, &w.MemberID, &w.Kind, &w.Instruction, &w.Reason, &w.SourceID, &w.Status, &w.DueAt, &w.Attempts, &w.Result, &w.CreatedAt, &w.RootID, &w.Autonomous, &w.AssignedBy, &w.ParentID, &audience)
	decode(audience, &w.FirstSpeakers)
	return w, e
}
func (s *Store) Works(room string) ([]Work, error) {
	rows, e := s.db.Query(`SELECT `+workColumns+` FROM work WHERE coalesce(conversation_id,'')=? ORDER BY created_at DESC LIMIT 50`, room)
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

func (s *Store) Participations(room string) ([]Participation, error) {
	rows, err := s.db.Query(`SELECT p.work_id,p.member_id,p.action,p.reason,p.selected,p.created_at FROM participation p JOIN work w ON w.id=p.work_id WHERE w.conversation_id=? ORDER BY p.rowid DESC LIMIT 40`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Participation{}
	for rows.Next() {
		var p Participation
		if err := rows.Scan(&p.WorkID, &p.MemberID, &p.Action, &p.Reason, &p.Selected, &p.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}
func (s *Store) DueWorks() ([]Work, error) {
	return s.dueWorks(true, true, true)
}

// 在原有批次上限之前过滤。缺少某个供应商凭据的排队工作不占用其他场景的名额。
func (s *Store) dueWorks(chat, research, exploration bool) ([]Work, error) {
	rows, e := s.db.Query(`SELECT `+workColumns+` FROM work
WHERE status IN ('pending','waiting_budget') AND due_at<=?
  AND CASE WHEN kind='research' THEN ? WHEN coalesce(conversation_id,'')='' THEN ? ELSE ? END
ORDER BY CASE kind WHEN 'chat' THEN 0 WHEN 'share' THEN 1 WHEN 'revisit' THEN 2 WHEN 'research' THEN 2 ELSE 3 END,created_at LIMIT 100`,
		time.Now().UnixMilli(), research, exploration, chat)
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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if result != "" && !strings.HasPrefix(result, identityRevisionRetry) {
		// 人物编辑可能晚于本轮工作开始，甚至发生在网络请求返回错误的同时。
		// 保存错误不能冲掉更新事务写下的先后关系，否则预算恢复会重放旧偏好。
		var previous string
		err := tx.QueryRow(`SELECT result FROM work WHERE id=?`, w.ID).Scan(&previous)
		if err != nil {
			return err
		}
		if strings.HasPrefix(previous, identityRevisionRetry) {
			result = identityRevisionRetry + "\n" + result
		}
	}
	changed, err := tx.Exec(`UPDATE work SET status=?,result=CASE WHEN ?='' AND ?<>'done' THEN result ELSE ? END,due_at=?,budget_required=? WHERE id=? AND status='running'`, status, result, status, result, due.UnixMilli(), w.BudgetRequired, w.ID)
	if err != nil {
		return err
	}
	count, err := changed.RowsAffected()
	if err != nil {
		return err
	}
	// 正常研究在保存结果的事务里回报；运行异常也应让协调者知道实际阻碍。
	// 只在本次确实结束工作时通知，被真人停止或已保存的任务不能被旧执行复活。
	if count > 0 && (status == "failed" || status == "paused") {
		if err := queueCoordinationReview(tx, w, status, result); err != nil {
			return err
		}
	}
	// 一次自主活动结束时，如果下一次已经落在过去，就从结束时刻重抽。
	// 不把运行期间错过的那次立刻再开一遍。恢复中的同一次活动保持原时间。
	if count > 0 && w.Kind == "explore" && w.ConversationID == "" && status != "pending" && status != "waiting_budget" && status != "paused" && status != "running" {
		if err := rollWakeIfDue(tx, w.MemberID, time.Now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Memories(member, room string) ([]Memory, error) {
	rows, e := s.db.Query(memorySelect+` WHERE m.member_id=? AND (m.conversation_id=? OR m.conversation_id='') ORDER BY m.updated_at DESC,m.rowid DESC LIMIT 30`, member, room)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	list := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
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
	rows, e := s.db.Query(`WITH visible AS (SELECT * FROM artifacts a WHERE `+visibleArtifactSQL+`)
 SELECT a.id,coalesce(a.conversation_id,''),a.member_id,a.title,a.content,a.version,a.source_id,a.created_at,a.format FROM visible a
 WHERE version=(SELECT max(version) FROM visible b WHERE b.id=a.id) ORDER BY created_at DESC`, room, "", room)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []Artifact{}
	for rows.Next() {
		var a Artifact
		if e = rows.Scan(&a.ID, &a.ConversationID, &a.MemberID, &a.Title, &a.Content, &a.Version, &a.SourceID, &a.CreatedAt, &a.Format); e != nil {
			return nil, e
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
func (s *Store) ArtifactVersions(id string) ([]Artifact, error) {
	rows, e := s.db.Query(`SELECT id,coalesce(conversation_id,''),member_id,title,content,version,source_id,created_at,format FROM artifacts WHERE id=? ORDER BY version DESC`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	list := []Artifact{}
	for rows.Next() {
		var a Artifact
		if e = rows.Scan(&a.ID, &a.ConversationID, &a.MemberID, &a.Title, &a.Content, &a.Version, &a.SourceID, &a.CreatedAt, &a.Format); e != nil {
			return nil, e
		}
		list = append(list, a)
	}
	return list, rows.Err()
}
