package observe

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Log 是主服务的观测写入器。它使用自己的 SQLite 文件，不打开聊天库，也不做业务初始化。
// 写入走内存队列。队列满或落盘失败时聊天继续，观测页能看到缺了记录。
type Log struct {
	db      *sql.DB
	ch      chan writeOp
	next    atomic.Int64
	dropped atomic.Int64
	failed  atomic.Bool
	stop    chan struct{}
	done    chan struct{}
}

func OpenLog(path string) (*Log, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=2000;
CREATE TABLE IF NOT EXISTS runs(
 id TEXT PRIMARY KEY, work_id TEXT NOT NULL, parent_work_id TEXT NOT NULL DEFAULT '',
 member_id TEXT NOT NULL DEFAULT '', conversation_id TEXT NOT NULL DEFAULT '',
 kind TEXT NOT NULL DEFAULT '', source_id TEXT NOT NULL DEFAULT '', root_id TEXT NOT NULL DEFAULT '',
 execution INTEGER NOT NULL DEFAULT 1, trigger TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL, note TEXT NOT NULL DEFAULT '',
 started_at TEXT NOT NULL, ended_at TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS steps(
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL, call_id TEXT NOT NULL DEFAULT '',
 parent_id TEXT NOT NULL DEFAULT '', retry_of TEXT NOT NULL DEFAULT '',
 kind TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
 attempt INTEGER NOT NULL DEFAULT 1, summary TEXT NOT NULL DEFAULT '',
 business_id TEXT NOT NULL DEFAULT '', member_id TEXT NOT NULL DEFAULT '',
 conversation_id TEXT NOT NULL DEFAULT '', target_conversation_id TEXT NOT NULL DEFAULT '',
 cost_status TEXT NOT NULL DEFAULT '', cost_micros INTEGER, reserved_micros INTEGER,
 started_at TEXT NOT NULL, ended_at TEXT NOT NULL DEFAULT '', seq INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS steps_run ON steps(run_id, seq);
CREATE INDEX IF NOT EXISTS steps_call ON steps(call_id);
CREATE TABLE IF NOT EXISTS snapshots(
 step_id TEXT PRIMARY KEY, input TEXT NOT NULL DEFAULT '', output TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS gaps(
 id INTEGER PRIMARY KEY, at TEXT NOT NULL, note TEXT NOT NULL);`); err != nil {
		db.Close()
		return nil, err
	}
	log := &Log{db: db, ch: make(chan writeOp, 256), stop: make(chan struct{}), done: make(chan struct{})}
	var maxSeq int64
	if err = db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM steps`).Scan(&maxSeq); err != nil {
		db.Close()
		return nil, err
	}
	log.next.Store(maxSeq)
	return log, nil
}

type writeOp struct {
	fn   func(*sql.Tx) error
	done chan struct{}
}

func (l *Log) Serve() {
	if l == nil {
		return
	}
	defer close(l.done)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	batch := make([]writeOp, 0, 32)
	flush := func() {
		if len(batch) == 0 && l.dropped.Load() == 0 && !l.failed.Load() {
			return
		}
		ack := func() {
			for _, op := range batch {
				if op.done != nil {
					close(op.done)
				}
			}
			batch = batch[:0]
		}
		tx, err := l.db.Begin()
		if err != nil {
			l.failed.Store(true)
			slog.Error("观测库暂时写不进去", "error", err)
			ack()
			return
		}
		for _, op := range batch {
			if err = op.fn(tx); err != nil {
				break
			}
		}
		if err == nil {
			if n := l.dropped.Swap(0); n > 0 {
				_, err = tx.Exec(`INSERT INTO gaps(at,note) VALUES(?,?)`, now(), fmt.Sprintf("有 %d 条观测因队列已满被丢弃，这段记录可能不完整", n))
			}
		}
		if err == nil && l.failed.Swap(false) {
			_, err = tx.Exec(`INSERT INTO gaps(at,note) VALUES(?,?)`, now(), "有一段观测写入失败，这段记录可能不完整")
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			tx.Rollback()
			l.failed.Store(true)
			slog.Error("观测库暂时写不进去", "error", err)
		}
		ack()
	}
	for {
		select {
		case <-l.stop:
			for {
				select {
				case op := <-l.ch:
					batch = append(batch, op)
				default:
					flush()
					return
				}
			}
		case op := <-l.ch:
			batch = append(batch, op)
			if len(batch) >= 32 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (l *Log) Close() {
	if l == nil {
		return
	}
	close(l.stop)
	<-l.done
	l.db.Close()
}

func (l *Log) emit(fn func(*sql.Tx) error) {
	l.send(writeOp{fn: fn})
}

func (l *Log) send(op writeOp) {
	if l == nil || op.fn == nil {
		if op.done != nil {
			close(op.done)
		}
		return
	}
	select {
	case l.ch <- op:
	default:
		l.dropped.Add(1)
		if op.done != nil {
			close(op.done)
		}
	}
}

// Sync 等到此前排队的写入提交。测试用来读回结果。
func (l *Log) Sync() {
	if l == nil {
		return
	}
	done := make(chan struct{})
	l.send(writeOp{fn: func(*sql.Tx) error { return nil }, done: done})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

type Run struct {
	ID, WorkID, ParentWorkID, MemberID, ConversationID string
	Kind, SourceID, RootID, Trigger, Status, Note      string
	Execution                                          int
}

func (l *Log) BeginRun(r Run) string {
	if l == nil {
		return ""
	}
	if r.ID == "" {
		r.ID = NewID("run")
	}
	if r.Status == "" {
		r.Status = "running"
	}
	if r.Execution == 0 {
		r.Execution = 1
	}
	l.emit(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO runs(id,work_id,parent_work_id,member_id,conversation_id,kind,source_id,root_id,execution,trigger,status,note,started_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET status=excluded.status, note=excluded.note`,
			r.ID, r.WorkID, r.ParentWorkID, r.MemberID, r.ConversationID, r.Kind, r.SourceID, r.RootID, r.Execution, r.Trigger, r.Status, r.Note, now())
		return err
	})
	return r.ID
}

type Step struct {
	ID, RunID, CallID, ParentID, RetryOf           string
	Kind, Name, Status, Summary                    string
	Attempt                                        int
	BusinessID, MemberID, ConversationID, TargetID string
	CostStatus                                     string
	CostMicros, ReservedMicros                     *int64
	Input, Output                                  string
}

func (l *Log) WriteRunEnd(id, status, note string) {
	if l == nil || id == "" {
		return
	}
	l.emit(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO runs(id,work_id,status,note,started_at,ended_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET status=excluded.status, note=excluded.note, ended_at=excluded.ended_at`,
			id, "", status, note, now(), now())
		return err
	})
}

func (l *Log) WriteStep(s Step) {
	if l == nil || s.ID == "" {
		return
	}
	if s.Attempt == 0 {
		s.Attempt = 1
	}
	if s.Status == "" {
		s.Status = "started"
	}
	started := now()
	seq := l.next.Add(1)
	l.emit(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO steps(id,run_id,call_id,parent_id,retry_of,kind,name,status,attempt,summary,business_id,member_id,conversation_id,target_conversation_id,cost_status,cost_micros,reserved_micros,started_at,ended_at,seq)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET
  status=excluded.status, summary=CASE WHEN excluded.summary<>'' THEN excluded.summary ELSE steps.summary END,
  business_id=CASE WHEN excluded.business_id<>'' THEN excluded.business_id ELSE steps.business_id END,
  cost_status=CASE WHEN excluded.cost_status<>'' THEN excluded.cost_status ELSE steps.cost_status END,
  cost_micros=coalesce(excluded.cost_micros, steps.cost_micros),
  reserved_micros=coalesce(excluded.reserved_micros, steps.reserved_micros),
  ended_at=CASE WHEN excluded.ended_at<>'' THEN excluded.ended_at ELSE steps.ended_at END`,
			s.ID, s.RunID, s.CallID, s.ParentID, s.RetryOf, s.Kind, s.Name, s.Status, s.Attempt, s.Summary, s.BusinessID, s.MemberID, s.ConversationID, s.TargetID, s.CostStatus, s.CostMicros, s.ReservedMicros, started, endedOf(s), seq)
		if err != nil {
			return err
		}
		if s.Input == "" && s.Output == "" {
			return nil
		}
		_, err = tx.Exec(`INSERT INTO snapshots(step_id,input,output) VALUES(?,?,?)
 ON CONFLICT(step_id) DO UPDATE SET
  input=CASE WHEN excluded.input<>'' THEN excluded.input ELSE snapshots.input END,
  output=CASE WHEN excluded.output<>'' THEN excluded.output ELSE snapshots.output END`, s.ID, s.Input, s.Output)
		return err
	})
}

func endedOf(s Step) string {
	if s.Status == "started" || s.Status == "resume" {
		return ""
	}
	return now()
}

func NewID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "_" + now()
	}
	return prefix + "_" + fmt.Sprintf("%x", b[:])
}

func now() string { return time.Now().Format(time.RFC3339Nano) }
