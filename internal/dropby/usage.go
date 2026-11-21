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

// 测光只读这一份账本。金额沿用结算时写入的微美元，不按现在的单价重算，也不改预留。
type costQuery struct {
	Period, Date, Conversation, Member, Root string
	Personal                                 bool
}

type costTotals struct {
	EstimateMicros   int64 `json:"estimate_micros"`
	EstimateCalls    int   `json:"estimate_calls"`
	RunningMicros    int64 `json:"running_micros"`
	RunningCalls     int   `json:"running_calls"`
	UnverifiedMicros int64 `json:"unverified_micros"`
	UnverifiedCalls  int   `json:"unverified_calls"`
	NotSentCalls     int   `json:"not_sent_calls"`
	ReleasedCalls    int   `json:"released_calls"`
	InputTokens      int   `json:"input_tokens"`
	CachedTokens     int   `json:"cached_tokens"`
	CacheWriteTokens int   `json:"cache_write_tokens"`
	OutputTokens     int   `json:"output_tokens"`
	ReasoningTokens  int   `json:"reasoning_tokens"`
	ReasoningKnown   int   `json:"reasoning_known"`
	ReasoningMissing int   `json:"reasoning_missing"`
}

type costPart struct {
	Purpose        string `json:"purpose"`
	Model          string `json:"model"`
	Calls          int    `json:"calls"`
	EstimateMicros int64  `json:"estimate_micros"`
}

type costCall struct {
	ID              int64  `json:"id"`
	StartedAt       string `json:"started_at"`
	Purpose         string `json:"purpose"`
	Model           string `json:"model"`
	CostMicros      int64  `json:"cost_micros"`
	InputTokens     int    `json:"input_tokens"`
	CachedTokens    int    `json:"cached_tokens"`
	CacheWrite      int    `json:"cache_write_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	ReasoningTokens *int   `json:"reasoning_tokens,omitempty"`
}

type costPlace struct {
	ConversationID string `json:"conversation_id"`
	Name           string `json:"name"`
	MemberID       string `json:"member_id"`
	MemberName     string `json:"member_name"`
	SameMember     bool   `json:"same_member"`
	costTotals
}

type costScope struct {
	Mode         string      `json:"mode"` // room、member、personal、activity、all
	Attribution  string      `json:"attribution,omitempty"`
	Period       string      `json:"period"`
	Start        string      `json:"start,omitempty"`
	End          string      `json:"end,omitempty"` // 右边界不含当天。
	Today        string      `json:"today"`
	Currency     string      `json:"currency"`
	Basis        string      `json:"basis"`
	OwnerMember  string      `json:"owner_member,omitempty"`
	OwnerRoom    string      `json:"owner_room,omitempty"`
	Direct       costTotals  `json:"direct"`
	Parts        []costPart  `json:"parts"`
	Largest      *costCall   `json:"largest,omitempty"`
	Later        []costPlace `json:"later,omitempty"`
	Groups       []costPlace `json:"groups,omitempty"`
	LegacyNote   string      `json:"legacy_note,omitempty"`
	LegacyCalls  int         `json:"legacy_calls,omitempty"`
	LegacyMicros int64       `json:"legacy_micros,omitempty"`
	LegacyHold   int64       `json:"legacy_hold_micros,omitempty"`
}

const costBasis = "用量估算是结算时按项目价格规则写入账本的金额，单位美元。账本没有保存当时的单价，这里不把现在的价格说成当时的价格，也不是供应商实际扣费。缓存输入已包含在输入里。思考用量如果回执单独给了数字，已经包含在输出里，不另加；没有单独数字时不补成零。"

type costDB interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
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

type costQueryError struct{ message string }

func (e costQueryError) Error() string { return e.message }

func (a *API) usageScope(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	report, err := a.engine.Store.readCostScope(r.Context(), costQuery{
		Period: query.Get("period"), Date: query.Get("date"), Conversation: query.Get("conversation"),
		Member: query.Get("member"), Root: query.Get("root"), Personal: query.Get("personal") == "1",
	}, time.Now())
	var invalid costQueryError
	if errors.As(err, &invalid) {
		respond(w, http.StatusBadRequest, map[string]string{"error": invalid.Error()})
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	respond(w, http.StatusOK, report)
}

func (s *Store) readCostScope(ctx context.Context, q costQuery, today time.Time) (costScope, error) {
	return queryCostScope(ctx, s.db, q, today)
}

func queryCostScope(ctx context.Context, db costDB, q costQuery, today time.Time) (costScope, error) {
	if q.Personal && (q.Conversation != "" || q.Root != "") {
		return costScope{}, costQueryError{"个人活动、会话和单场活动要分开看"}
	}
	if q.Root != "" && q.Conversation != "" {
		return costScope{}, costQueryError{"一场活动的费用不要和某个会话的直接费用混在同一次查询里"}
	}
	period := q.Period
	if period == "" {
		period = "all"
	}
	start, end := "", ""
	switch period {
	case "all":
	case "day", "week":
		date := q.Date
		if date == "" {
			date = today.Format(usageDate)
		}
		at, err := time.ParseInLocation(usageDate, date, time.Local)
		if err != nil || at.Format(usageDate) != date || date > today.Format(usageDate) {
			return costScope{}, costQueryError{"请选择有效且未超出今天的日期"}
		}
		from, to := at, at.AddDate(0, 0, 1)
		if period == "week" {
			from, to = budgetWeek(at)
		}
		start, end = from.Format(usageDate), to.Format(usageDate)
	default:
		return costScope{}, costQueryError{"查看范围应为全部、按天或按周"}
	}

	report := costScope{
		Period: period, Start: start, End: end, Today: today.Format(usageDate),
		Currency: "USD", Basis: costBasis, Parts: []costPart{}, Mode: "all",
	}
	where, args := " WHERE 1=1", []any{}
	if start != "" {
		where += " AND day>=? AND day<?"
		args = append(args, start, end)
	}
	switch {
	case q.Root != "":
		report.Mode = "activity"
		var room, member string
		err := db.QueryRowContext(ctx, `SELECT coalesce(conversation_id,''), coalesce(member_id,'') FROM work WHERE id=?`, q.Root).Scan(&room, &member)
		if errors.Is(err, sql.ErrNoRows) {
			report.Attribution = "unknown"
			report.Basis = "找不到这场活动对应的工作记录。下面只按账本里的会话分开，没有合成一个活动总额。" + costBasis
			groups, err := costGroups(ctx, db, where+" AND root_id=?", append(append([]any{}, args...), q.Root))
			if err != nil {
				return costScope{}, err
			}
			report.Groups = groups
		} else if err != nil {
			return costScope{}, err
		} else {
			report.Attribution = "work"
			report.OwnerMember, report.OwnerRoom = member, room
			ownWhere := where + " AND root_id=? AND conversation_id=?"
			ownArgs := append(append([]any{}, args...), q.Root, room)
			if report.Direct, err = costSum(ctx, db, ownWhere, ownArgs); err != nil {
				return costScope{}, err
			}
			if report.Parts, err = costParts(ctx, db, ownWhere, ownArgs); err != nil {
				return costScope{}, err
			}
			if report.Largest, err = costLargest(ctx, db, ownWhere, ownArgs); err != nil {
				return costScope{}, err
			}
			report.Later, err = costPlaces(ctx, db, where+" AND root_id=? AND conversation_id<>?", append(append([]any{}, args...), q.Root, room), member)
			if err != nil {
				return costScope{}, err
			}
		}
	default:
		if q.Conversation != "" {
			report.Mode = "room"
			where += " AND conversation_id=?"
			args = append(args, q.Conversation)
		} else if q.Personal {
			report.Mode = "personal"
			where += " AND conversation_id=''"
		}
		if q.Member != "" {
			if report.Mode == "all" {
				report.Mode = "member"
			}
			where += " AND member_id=?"
			args = append(args, q.Member)
		}
		var err error
		if report.Direct, err = costSum(ctx, db, where, args); err != nil {
			return costScope{}, err
		}
		if report.Parts, err = costParts(ctx, db, where, args); err != nil {
			return costScope{}, err
		}
		if report.Largest, err = costLargest(ctx, db, where, args); err != nil {
			return costScope{}, err
		}
	}
	note, calls, micros, hold, err := costLegacy(ctx, db, start, end)
	if err != nil {
		return costScope{}, err
	}
	report.LegacyNote, report.LegacyCalls, report.LegacyMicros, report.LegacyHold = note, calls, micros, hold
	return report, nil
}

const costEstimate = `status='settled' AND outcome NOT IN ('not_sent','released')`

func costSum(ctx context.Context, db costDB, where string, args []any) (costTotals, error) {
	var t costTotals
	err := db.QueryRowContext(ctx, `SELECT
 coalesce(sum(CASE WHEN `+costEstimate+` THEN cost_micros ELSE 0 END),0),
 coalesce(sum(CASE WHEN `+costEstimate+` THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN status='running' THEN reserved_micros ELSE 0 END),0),
 coalesce(sum(CASE WHEN status='running' THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN status='unknown' THEN reserved_micros ELSE 0 END),0),
 coalesce(sum(CASE WHEN status='unknown' THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN status='settled' AND outcome='not_sent' THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN status='settled' AND outcome='released' THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN `+costEstimate+` THEN input_tokens ELSE 0 END),0),
 coalesce(sum(CASE WHEN `+costEstimate+` THEN cached_tokens ELSE 0 END),0),
 coalesce(sum(CASE WHEN `+costEstimate+` THEN cache_write_tokens ELSE 0 END),0),
 coalesce(sum(CASE WHEN `+costEstimate+` THEN output_tokens ELSE 0 END),0),
 coalesce(sum(CASE WHEN `+costEstimate+` AND reasoning_tokens IS NOT NULL THEN reasoning_tokens ELSE 0 END),0),
 coalesce(sum(CASE WHEN `+costEstimate+` AND reasoning_tokens IS NOT NULL THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN `+costEstimate+` AND purpose NOT IN ('search','read_page') AND reasoning_tokens IS NULL THEN 1 ELSE 0 END),0)
 FROM usage_calls`+where, args...).Scan(
		&t.EstimateMicros, &t.EstimateCalls, &t.RunningMicros, &t.RunningCalls, &t.UnverifiedMicros, &t.UnverifiedCalls,
		&t.NotSentCalls, &t.ReleasedCalls, &t.InputTokens, &t.CachedTokens, &t.CacheWriteTokens, &t.OutputTokens,
		&t.ReasoningTokens, &t.ReasoningKnown, &t.ReasoningMissing)
	return t, err
}

func costParts(ctx context.Context, db costDB, where string, args []any) ([]costPart, error) {
	rows, err := db.QueryContext(ctx, `SELECT purpose, model, count(*), coalesce(sum(cost_micros),0)
 FROM usage_calls`+where+` AND `+costEstimate+` GROUP BY purpose, model ORDER BY sum(cost_micros) DESC, purpose`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parts := []costPart{}
	for rows.Next() {
		var p costPart
		if err = rows.Scan(&p.Purpose, &p.Model, &p.Calls, &p.EstimateMicros); err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

func costLargest(ctx context.Context, db costDB, where string, args []any) (*costCall, error) {
	var c costCall
	err := db.QueryRowContext(ctx, `SELECT id, started_at, purpose, model, cost_micros, input_tokens, cached_tokens, cache_write_tokens, output_tokens, reasoning_tokens
 FROM usage_calls`+where+` AND `+costEstimate+` AND cost_micros>0 ORDER BY cost_micros DESC, id DESC LIMIT 1`, args...).Scan(
		&c.ID, &c.StartedAt, &c.Purpose, &c.Model, &c.CostMicros, &c.InputTokens, &c.CachedTokens, &c.CacheWrite, &c.OutputTokens, &c.ReasoningTokens)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func costPlaces(ctx context.Context, db costDB, where string, args []any, owner string) ([]costPlace, error) {
	rows, err := db.QueryContext(ctx, `SELECT u.conversation_id, coalesce(c.name,''), u.member_id, coalesce(json_extract(m.data,'$.name'),''),
 coalesce(sum(CASE WHEN `+costEstimate+` THEN u.cost_micros ELSE 0 END),0),
 coalesce(sum(CASE WHEN `+costEstimate+` THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN u.status='running' THEN u.reserved_micros ELSE 0 END),0),
 coalesce(sum(CASE WHEN u.status='running' THEN 1 ELSE 0 END),0),
 coalesce(sum(CASE WHEN u.status='unknown' THEN u.reserved_micros ELSE 0 END),0),
 coalesce(sum(CASE WHEN u.status='unknown' THEN 1 ELSE 0 END),0)
 FROM usage_calls u LEFT JOIN conversations c ON c.id=u.conversation_id LEFT JOIN members m ON m.id=u.member_id`+where+`
 GROUP BY u.conversation_id, u.member_id ORDER BY sum(CASE WHEN `+costEstimate+` THEN u.cost_micros ELSE 0 END) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []costPlace{}
	for rows.Next() {
		var p costPlace
		if err = rows.Scan(&p.ConversationID, &p.Name, &p.MemberID, &p.MemberName, &p.EstimateMicros, &p.EstimateCalls, &p.RunningMicros, &p.RunningCalls, &p.UnverifiedMicros, &p.UnverifiedCalls); err != nil {
			return nil, err
		}
		p.SameMember = owner != "" && p.MemberID == owner
		list = append(list, p)
	}
	return list, rows.Err()
}

func costGroups(ctx context.Context, db costDB, where string, args []any) ([]costPlace, error) {
	return costPlaces(ctx, db, where, args, "")
}

func costLegacy(ctx context.Context, db costDB, start, end string) (string, int, int64, int64, error) {
	where, args := "", []any{}
	if start != "" {
		where, args = " WHERE day>=? AND day<?", []any{start, end}
	}
	rows, err := db.QueryContext(ctx, `SELECT day, calls, cost_micros-unsettled_micros, unsettled_micros FROM usage`+where, args...)
	if err != nil {
		return "", 0, 0, 0, err
	}
	defer rows.Close()
	type dayTotal struct {
		calls      int
		cost, hold int64
	}
	days := map[string]dayTotal{}
	for rows.Next() {
		var day string
		var item dayTotal
		if err = rows.Scan(&day, &item.calls, &item.cost, &item.hold); err != nil {
			return "", 0, 0, 0, err
		}
		days[day] = item
	}
	if err = rows.Err(); err != nil {
		return "", 0, 0, 0, err
	}
	rows.Close()
	detailed, err := db.QueryContext(ctx, `SELECT day, count(*),
 coalesce(sum(CASE WHEN status='settled' THEN cost_micros ELSE 0 END),0),
 coalesce(sum(CASE WHEN status='settled' THEN 0 ELSE reserved_micros END),0)
 FROM usage_calls`+where+` GROUP BY day`, args...)
	if err != nil {
		return "", 0, 0, 0, err
	}
	defer detailed.Close()
	for detailed.Next() {
		var day string
		var calls int
		var cost, hold int64
		if err = detailed.Scan(&day, &calls, &cost, &hold); err != nil {
			return "", 0, 0, 0, err
		}
		item := days[day]
		item.calls -= calls
		item.cost -= cost
		item.hold -= hold
		days[day] = item
	}
	if err = detailed.Err(); err != nil {
		return "", 0, 0, 0, err
	}
	var calls int
	var cost, hold int64
	for _, item := range days {
		if item.calls > 0 {
			calls += item.calls
		}
		if item.cost > 0 {
			cost += item.cost
		}
		if item.hold > 0 {
			hold += item.hold
		}
	}
	if calls == 0 && cost == 0 && hold == 0 {
		return "", 0, 0, 0, nil
	}
	return "同一时间里还有只记在日汇总、无法归到会话或活动的费用。它们没有算进上面的数字，也没有当成零。", calls, cost, hold, nil
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
