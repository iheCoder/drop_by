package observe

import (
	"database/sql"
	"errors"
	"os"

	_ "modernc.org/sqlite"
)

func DBPath() string {
	if path := os.Getenv("OBSERVE_DB"); path != "" {
		return path
	}
	return "data/observe.sqlite"
}

// OpenRead 只读观测库。不要用聊天库的 OpenStore，那个入口会做业务初始化和状态调整。
func OpenRead(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err = db.Exec(`PRAGMA busy_timeout=2000; PRAGMA query_only=ON`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type RunView struct {
	ID             string `json:"id"`
	WorkID         string `json:"work_id"`
	ParentWorkID   string `json:"parent_work_id"`
	MemberID       string `json:"member_id"`
	ConversationID string `json:"conversation_id"`
	Kind           string `json:"kind"`
	SourceID       string `json:"source_id"`
	RootID         string `json:"root_id"`
	Trigger        string `json:"trigger"`
	Status         string `json:"status"`
	Note           string `json:"note"`
	Execution      int    `json:"execution"`
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at"`
}

type StepView struct {
	ID             string `json:"id"`
	RunID          string `json:"run_id"`
	CallID         string `json:"call_id"`
	ParentID       string `json:"parent_id"`
	RetryOf        string `json:"retry_of"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	Summary        string `json:"summary"`
	Attempt        int    `json:"attempt"`
	BusinessID     string `json:"business_id"`
	MemberID       string `json:"member_id"`
	ConversationID string `json:"conversation_id"`
	TargetID       string `json:"target_conversation_id"`
	CostStatus     string `json:"cost_status"`
	CostMicros     *int64 `json:"cost_micros"`
	ReservedMicros *int64 `json:"reserved_micros"`
	StartedAt      string `json:"started_at"`
	EndedAt        string `json:"ended_at"`
}

type Gap struct {
	At   string `json:"at"`
	Note string `json:"note"`
}

func ListRuns(db *sql.DB, limit int) ([]RunView, []Gap, error) {
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	rows, err := db.Query(`SELECT id,work_id,parent_work_id,member_id,conversation_id,kind,source_id,root_id,execution,trigger,status,note,started_at,ended_at
 FROM runs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	runs := []RunView{}
	for rows.Next() {
		var r RunView
		if err = rows.Scan(&r.ID, &r.WorkID, &r.ParentWorkID, &r.MemberID, &r.ConversationID, &r.Kind, &r.SourceID, &r.RootID, &r.Execution, &r.Trigger, &r.Status, &r.Note, &r.StartedAt, &r.EndedAt); err != nil {
			return nil, nil, err
		}
		runs = append(runs, r)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	gaps, err := recentGaps(db)
	return runs, gaps, err
}

func recentGaps(db *sql.DB) ([]Gap, error) {
	rows, err := db.Query(`SELECT at,note FROM gaps ORDER BY id DESC LIMIT 5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Gap{}
	for rows.Next() {
		var g Gap
		if err = rows.Scan(&g.At, &g.Note); err != nil {
			return nil, err
		}
		list = append(list, g)
	}
	return list, rows.Err()
}

func ListSteps(db *sql.DB, runID string) ([]StepView, error) {
	rows, err := db.Query(`SELECT id,run_id,call_id,parent_id,retry_of,kind,name,status,attempt,summary,business_id,member_id,conversation_id,target_conversation_id,cost_status,cost_micros,reserved_micros,started_at,ended_at
 FROM steps WHERE run_id=? ORDER BY seq, started_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []StepView{}
	for rows.Next() {
		var s StepView
		if err = rows.Scan(&s.ID, &s.RunID, &s.CallID, &s.ParentID, &s.RetryOf, &s.Kind, &s.Name, &s.Status, &s.Attempt, &s.Summary, &s.BusinessID, &s.MemberID, &s.ConversationID, &s.TargetID, &s.CostStatus, &s.CostMicros, &s.ReservedMicros, &s.StartedAt, &s.EndedAt); err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

func Snapshot(db *sql.DB, stepID string) (input, output string, err error) {
	err = db.QueryRow(`SELECT input,output FROM snapshots WHERE step_id=?`, stepID).Scan(&input, &output)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", os.ErrNotExist
	}
	return input, output, err
}
