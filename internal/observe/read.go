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
	HasSnapshot    bool   `json:"has_snapshot"`
}

type Gap struct {
	At   string `json:"at"`
	Note string `json:"note"`
}

// RunFilter 只描述要看的范围。费用合计不从这一页的运行里加出来。
type RunFilter struct {
	Limit                int
	BeforeAt, BeforeID   string
	Conversation, Member string
	Work, Root           string
	From, To             string // 日期，右边界不含 To。
	Personal             bool
}

type RunOutput struct {
	ConversationID string `json:"conversation_id"`
	Count          int    `json:"count"`
}

type RunPage struct {
	Runs          []RunView
	Gaps          []Gap
	HasEarlier    bool
	BudgetBlocked int
	Outputs       []RunOutput
}

func ListRuns(db *sql.DB, limit int) ([]RunView, []Gap, error) {
	page, err := ListRunPage(db, RunFilter{Limit: limit})
	if err != nil {
		return nil, nil, err
	}
	return page.Runs, page.Gaps, nil
}

func ListRunPage(db *sql.DB, f RunFilter) (RunPage, error) {
	limit := f.Limit
	if limit <= 0 || limit > 50 {
		limit = 40
	}
	where, args := runWhere(f, true)
	rows, err := db.Query(`SELECT id,work_id,parent_work_id,member_id,conversation_id,kind,source_id,root_id,execution,trigger,status,note,started_at,ended_at
 FROM runs`+where+` ORDER BY started_at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return RunPage{}, err
	}
	defer rows.Close()
	page := RunPage{Runs: []RunView{}, Outputs: []RunOutput{}}
	for rows.Next() {
		var r RunView
		if err = rows.Scan(&r.ID, &r.WorkID, &r.ParentWorkID, &r.MemberID, &r.ConversationID, &r.Kind, &r.SourceID, &r.RootID, &r.Execution, &r.Trigger, &r.Status, &r.Note, &r.StartedAt, &r.EndedAt); err != nil {
			return RunPage{}, err
		}
		page.Runs = append(page.Runs, r)
	}
	if err = rows.Err(); err != nil {
		return RunPage{}, err
	}
	if len(page.Runs) > limit {
		page.HasEarlier = true
		page.Runs = page.Runs[:limit]
	}
	scope, scopeArgs := runWhere(f, false)
	if err = db.QueryRow(`SELECT count(*) FROM runs`+scope+` AND status='waiting_budget'`, scopeArgs...).Scan(&page.BudgetBlocked); err != nil {
		return RunPage{}, err
	}
	if f.Work != "" {
		out, err := db.Query(`SELECT target_conversation_id, count(*) FROM steps s JOIN runs r ON r.id=s.run_id
 WHERE r.work_id=? AND s.kind='message' AND s.name='send_message' AND s.status IN ('sent','reused')
 GROUP BY target_conversation_id ORDER BY count(*) DESC`, f.Work)
		if err != nil {
			return RunPage{}, err
		}
		defer out.Close()
		for out.Next() {
			var item RunOutput
			if err = out.Scan(&item.ConversationID, &item.Count); err != nil {
				return RunPage{}, err
			}
			page.Outputs = append(page.Outputs, item)
		}
		if err = out.Err(); err != nil {
			return RunPage{}, err
		}
	}
	page.Gaps, err = recentGaps(db)
	return page, err
}

func runWhere(f RunFilter, paged bool) (string, []any) {
	clause, args := " WHERE 1=1", []any{}
	if f.Personal {
		clause += " AND conversation_id=''"
	} else if f.Conversation != "" {
		clause += " AND conversation_id=?"
		args = append(args, f.Conversation)
	}
	if f.Member != "" {
		// Group runs leave the run's member empty and record who acted on each step.
		clause += " AND (member_id=? OR id IN (SELECT run_id FROM steps WHERE member_id=?))"
		args = append(args, f.Member, f.Member)
	}
	if f.Work != "" {
		clause += " AND work_id=?"
		args = append(args, f.Work)
	}
	if f.Root != "" {
		clause += " AND root_id=?"
		args = append(args, f.Root)
	}
	if f.From != "" {
		clause += " AND substr(started_at,1,10)>=?"
		args = append(args, f.From)
	}
	if f.To != "" {
		clause += " AND substr(started_at,1,10)<?"
		args = append(args, f.To)
	}
	if paged && f.BeforeAt != "" {
		clause += " AND (started_at<? OR (started_at=? AND id<?))"
		args = append(args, f.BeforeAt, f.BeforeAt, f.BeforeID)
	}
	return clause, args
}

// Delivery 把群里的一条消息指回来源执行。来源会话不同时，消息本身不是那场活动的费用。
type Delivery struct {
	MessageID      string `json:"message_id"`
	StepID         string `json:"step_id"`
	RunID          string `json:"run_id"`
	WorkID         string `json:"work_id"`
	RootID         string `json:"root_id"`
	MemberID       string `json:"member_id"`
	Kind           string `json:"kind"`
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Note           string `json:"note"`
	Execution      int    `json:"execution"`
}

func Deliveries(db *sql.DB, room string) ([]Delivery, error) {
	rows, err := db.Query(`SELECT s.business_id, s.id, r.id, r.work_id, r.root_id, coalesce(nullif(s.member_id,''), r.member_id), r.kind, r.conversation_id, r.status, r.note, r.execution
 FROM steps s JOIN runs r ON r.id=s.run_id
 WHERE s.kind='message' AND s.name='send_message' AND s.status IN ('sent','reused') AND s.target_conversation_id=? AND s.business_id<>''
 ORDER BY s.seq`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Delivery{}
	for rows.Next() {
		var d Delivery
		if err = rows.Scan(&d.MessageID, &d.StepID, &d.RunID, &d.WorkID, &d.RootID, &d.MemberID, &d.Kind, &d.ConversationID, &d.Status, &d.Note, &d.Execution); err != nil {
			return nil, err
		}
		list = append(list, d)
	}
	return list, rows.Err()
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
	rows, err := db.Query(`SELECT id,run_id,call_id,parent_id,retry_of,kind,name,status,attempt,summary,business_id,member_id,conversation_id,target_conversation_id,cost_status,cost_micros,reserved_micros,started_at,ended_at,
 EXISTS(SELECT 1 FROM snapshots WHERE step_id=steps.id)
 FROM steps WHERE run_id=? ORDER BY seq, started_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []StepView{}
	for rows.Next() {
		var s StepView
		if err = rows.Scan(&s.ID, &s.RunID, &s.CallID, &s.ParentID, &s.RetryOf, &s.Kind, &s.Name, &s.Status, &s.Attempt, &s.Summary, &s.BusinessID, &s.MemberID, &s.ConversationID, &s.TargetID, &s.CostStatus, &s.CostMicros, &s.ReservedMicros, &s.StartedAt, &s.EndedAt, &s.HasSnapshot); err != nil {
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
