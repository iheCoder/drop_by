package dropby

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

const usageDate = "2006-01-02"
const settleDailyUsage = `UPDATE usage SET cost_micros=max(0,cost_micros-?+?),
 autonomous_cost_micros=max(0,autonomous_cost_micros+?),input_tokens=input_tokens+?,output_tokens=output_tokens+?,
 unsettled_micros=max(0,unsettled_micros-?) WHERE day=?`

// 用途来自已有的调用节点。成员、会话与事项是独立维度，独立的系统判断可以没有成员。
type usageCall struct {
	purpose, provider, model string
	member, room, root       string
}

type usageCallKey struct{}

func withUsageCall(ctx context.Context, call usageCall) context.Context {
	return context.WithValue(ctx, usageCallKey{}, call)
}

func workUsage(purpose, member string, c Conversation, w Work) usageCall {
	return usageCall{purpose: purpose, member: member, room: c.ID, root: rootOf(w)}
}

type usageReservation struct {
	id  int64
	day string
}

func (s *Store) initUsageCalls() error {
	// 旧账只有日汇总，保持原始粒度。上次进程退出前仍在途的请求现在无法核实，
	// 只调整展示状态，保留它占用的额度与请求发生日期。
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS usage_calls(
 id INTEGER PRIMARY KEY, day TEXT NOT NULL REFERENCES usage(day), started_at TEXT NOT NULL,
 purpose TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL,
 member_id TEXT NOT NULL, conversation_id TEXT NOT NULL, root_id TEXT NOT NULL, autonomous INTEGER NOT NULL,
 reserved_micros INTEGER NOT NULL, cost_micros INTEGER NOT NULL DEFAULT 0,
 status TEXT NOT NULL DEFAULT 'running', outcome TEXT NOT NULL DEFAULT '',
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 cached_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER
);
CREATE INDEX IF NOT EXISTS usage_calls_day ON usage_calls(day,id);
UPDATE usage_calls SET status='unknown' WHERE status='running';`)
	return err
}

func insertUsageCall(tx *sql.Tx, call usageCall, day string, at time.Time, reserved int64, autonomous bool) (int64, error) {
	result, err := tx.Exec(`INSERT INTO usage_calls(day,started_at,purpose,provider,model,member_id,conversation_id,root_id,autonomous,reserved_micros)
 VALUES(?,?,?,?,?,?,?,?,?,?)`, day, at.Format(time.RFC3339Nano), call.purpose, call.provider, call.model, call.member, call.room, call.root, autonomous, reserved)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) reserveCall(ctx context.Context, c Config, cost int64, provider, model, purpose string) (usageReservation, error) {
	call, _ := ctx.Value(usageCallKey{}).(usageCall)
	call.provider, call.model = provider, model
	if purpose != "" {
		call.purpose = purpose
	}
	if call.purpose == "" {
		call.purpose = "model"
	}
	return s.reserveBudget(ctx, c, cost, time.Now(), &call)
}

// 请求已离开本地调用边界而缺少可结算的用量。标记结果待核实，不释放预留。
func (s *Store) finishUsageCall(call usageReservation) {
	if call.id == 0 {
		return
	}
	if _, err := s.db.Exec(`UPDATE usage_calls SET status='unknown' WHERE id=? AND status='running'`, call.id); err != nil {
		slog.Error("无法更新额度明细状态", "error", err)
	}
}

func (s *Store) settleCall(call usageReservation, actual int64, usage responseUsage, outcome string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var day string
	var reserved int64
	var autonomous bool
	// 按单笔凭据结算。重复回执既不增加 token，也不释放其他并发请求的预留。
	err = tx.QueryRow(`SELECT day,reserved_micros,autonomous FROM usage_calls WHERE id=? AND status='running'`, call.id).
		Scan(&day, &reserved, &autonomous)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var autoDelta int64
	if autonomous {
		autoDelta = actual - reserved
	}
	if _, err = tx.Exec(settleDailyUsage, reserved, actual, autoDelta, usage.InputTokens, usage.OutputTokens, reserved, day); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE usage_calls SET status='settled',outcome=?,cost_micros=?,input_tokens=?,output_tokens=?,
 cached_tokens=?,cache_write_tokens=?,reasoning_tokens=? WHERE id=?`, outcome, actual, usage.InputTokens, usage.OutputTokens,
		usage.InputDetails.CachedTokens, usage.InputDetails.CacheWriteTokens, usage.OutputDetails.ReasoningTokens, call.id); err != nil {
		return err
	}
	return tx.Commit()
}

type usageEntry struct {
	ID              int64  `json:"id"`
	Day             string `json:"day"`
	StartedAt       string `json:"started_at"`
	Purpose         string `json:"purpose"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	MemberID        string `json:"member_id"`
	MemberName      string `json:"member_name"`
	ConversationID  string `json:"conversation_id"`
	Conversation    string `json:"conversation"`
	RootID          string `json:"root_id"`
	RootKind        string `json:"root_kind"`
	Matter          string `json:"matter"`
	Status          string `json:"status"`
	Outcome         string `json:"outcome"`
	Autonomous      bool   `json:"autonomous"`
	CostMicros      int64  `json:"cost_micros"`
	ReservedMicros  int64  `json:"reserved_micros"`
	InputTokens     int    `json:"input_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	CachedTokens    int    `json:"cached_tokens"`
	CacheWrite      int    `json:"cache_write_tokens"`
	ReasoningTokens *int   `json:"reasoning_tokens,omitempty"`
}

type dailyUsage struct {
	Day            string `json:"day"`
	Calls          int    `json:"calls"`
	CostMicros     int64  `json:"cost_micros"`
	ReservedMicros int64  `json:"reserved_micros"`
}

type usageReport struct {
	Start    string       `json:"start"`
	End      string       `json:"end"` // 右边界不含当天，与预算周的计算一致。
	Today    string       `json:"today"`
	FirstDay string       `json:"first_day"`
	Trend    []dailyUsage `json:"trend"` // 一柱对应一个自然日或一个预算周，包含旧账。
	Legacy   []dailyUsage `json:"legacy"`
	Entries  []usageEntry `json:"entries"`
}

func (a *API) usageDetails(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	today := time.Now()
	date := query.Get("date")
	if date == "" {
		date = today.Format(usageDate)
	}
	at, err := time.ParseInLocation(usageDate, date, time.Local)
	if err != nil || at.Format(usageDate) != date || date > today.Format(usageDate) {
		respond(w, http.StatusBadRequest, map[string]string{"error": "请选择有效且未超出今天的日期。"})
		return
	}
	start, end := at, at.AddDate(0, 0, 1)
	switch query.Get("period") {
	case "day":
	case "", "week":
		start, end = budgetWeek(at)
	default:
		respond(w, http.StatusBadRequest, map[string]string{"error": "查看范围应为按天或按周。"})
		return
	}
	report, err := a.engine.Store.readUsageReport(r.Context(), start, end, today)
	if err != nil {
		a.fail(w, err)
		return
	}
	respond(w, http.StatusOK, report)
}

func (s *Store) readUsageReport(ctx context.Context, start, end, today time.Time) (usageReport, error) {
	report := usageReport{Start: start.Format(usageDate), End: end.Format(usageDate), Today: today.Format(usageDate), Legacy: []dailyUsage{}, Entries: []usageEntry{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(min(day),?) FROM usage WHERE calls>0`, report.Today).Scan(&report.FirstDay); err != nil {
		return report, err
	}
	if report.Trend, err = usageTrend(ctx, tx, start, end, today); err != nil {
		return report, err
	}

	// 仅使用用户可见的会话名称、成员名称和本人发送的事项来源。账单不复制
	// 提示词、搜索参数、工具正文、个人探索的私有指令或模型输出。
	rows, err := tx.QueryContext(ctx, `SELECT u.id,u.day,u.started_at,u.purpose,u.provider,u.model,u.member_id,
 coalesce(json_extract(m.data,'$.name'),''),u.conversation_id,coalesce(c.name,''),u.root_id,coalesce(root.kind,''),
 coalesce(substr(msg.body,1,72),''),u.status,u.outcome,u.autonomous,u.cost_micros,
 CASE WHEN u.status='settled' THEN 0 ELSE u.reserved_micros END,
 u.input_tokens,u.output_tokens,u.cached_tokens,u.cache_write_tokens,u.reasoning_tokens
 FROM usage_calls u LEFT JOIN members m ON m.id=u.member_id
 LEFT JOIN conversations c ON c.id=u.conversation_id
 LEFT JOIN work root ON root.id=u.root_id AND root.conversation_id=u.conversation_id
 LEFT JOIN messages msg ON msg.id=root.source_id AND msg.conversation_id=u.conversation_id AND msg.author_id='user'
 WHERE u.day>=? AND u.day<? ORDER BY u.day DESC,u.id DESC`, report.Start, report.End)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	detailed := map[string]dailyUsage{}
	for rows.Next() {
		var u usageEntry
		if err = rows.Scan(&u.ID, &u.Day, &u.StartedAt, &u.Purpose, &u.Provider, &u.Model, &u.MemberID, &u.MemberName,
			&u.ConversationID, &u.Conversation, &u.RootID, &u.RootKind, &u.Matter, &u.Status, &u.Outcome, &u.Autonomous,
			&u.CostMicros, &u.ReservedMicros, &u.InputTokens, &u.OutputTokens, &u.CachedTokens, &u.CacheWrite, &u.ReasoningTokens); err != nil {
			return report, err
		}
		report.Entries = append(report.Entries, u)
		d := detailed[u.Day]
		d.Calls++
		d.CostMicros += u.CostMicros
		d.ReservedMicros += u.ReservedMicros
		detailed[u.Day] = d
	}
	if err = rows.Err(); err != nil {
		return report, err
	}
	rows.Close()

	// 在同一个读快照中用日总账减掉已知逐笔金额，只把剩余部分作为历史汇总。
	rows, err = tx.QueryContext(ctx, `SELECT day,calls,cost_micros-unsettled_micros,unsettled_micros FROM usage WHERE day>=? AND day<? ORDER BY day DESC`, report.Start, report.End)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var old dailyUsage
		if err = rows.Scan(&old.Day, &old.Calls, &old.CostMicros, &old.ReservedMicros); err != nil {
			return report, err
		}
		d := detailed[old.Day]
		old.Calls -= d.Calls
		old.CostMicros -= d.CostMicros
		old.ReservedMicros -= d.ReservedMicros
		if old.Calls != 0 || old.CostMicros != 0 || old.ReservedMicros != 0 {
			report.Legacy = append(report.Legacy, old)
		}
	}
	return report, rows.Err()
}

func usageTrend(ctx context.Context, tx *sql.Tx, selected, end, today time.Time) ([]dailyUsage, error) {
	const size = 7
	step := 1
	latest := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	if !selected.AddDate(0, 0, 1).Equal(end) {
		step = 7
		latest, _ = budgetWeek(today)
	}
	// 同一组七个周期内切换时保持柱子位置。按日历日期分页，避开夏令时的 23/25 小时天。
	civilDay := func(t time.Time) int64 {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
	}
	pageSize := size * step
	pagesBack := int(civilDay(latest)-civilDay(selected)) / pageSize
	first := latest.AddDate(0, 0, -pagesBack*pageSize-(size-1)*step)
	last := first.AddDate(0, 0, pageSize)
	buckets := make([]dailyUsage, size)
	for i := range buckets {
		buckets[i].Day = first.AddDate(0, 0, i*step).Format(usageDate)
	}
	rows, err := tx.QueryContext(ctx, `SELECT day,calls,cost_micros-unsettled_micros,unsettled_micros
 FROM usage WHERE day>=? AND day<? ORDER BY day`, buckets[0].Day, last.Format(usageDate))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var day dailyUsage
		if err = rows.Scan(&day.Day, &day.Calls, &day.CostMicros, &day.ReservedMicros); err != nil {
			return nil, err
		}
		for index+1 < len(buckets) && day.Day >= buckets[index+1].Day {
			index++
		}
		buckets[index].Calls += day.Calls
		buckets[index].CostMicros += day.CostMicros
		buckets[index].ReservedMicros += day.ReservedMicros
	}
	return buckets, rows.Err()
}

// 可证明没有发出的请求结算为零；超时/断线但发送结果不明仍保留 unknown。
func (m *HTTPModel) releaseUnsent(call usageReservation, err error) error {
	var failure *requestFailure
	if errors.As(err, &failure) && failure.NotSent {
		if e := m.Store.settleCall(call, 0, responseUsage{}, "not_sent"); e != nil {
			return &modelAccountingError{e}
		}
	}
	return nil
}
