package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultWakeMinutes = 6 * 60
	wakeOff            = int64(-1)
)

// 指数分布的单位等待。间隔围绕平均值散开，短间隔和长间隔都会出现。
// 调度落库时只把短于一秒的结果抬到一秒，避免零间隔把调度打成忙循环。
var wakeUnit = mrand.ExpFloat64

const wakeRhythmPrompt = `根据这位成员的人格与领域画像，为他设定长期自主活动节律。

自主唤醒意味着他获得一次按自己意愿活动的机会，不要求他向任何人发消息。结合这个人自己找事情做、探索和与人互动的方式，以及人格中体现的作息，判断适合他的节律。

没有足够依据设定某些时段差异时，不必编造；没有足够依据设定整个节律时，省略 windows，保留系统默认。很少自主活动不等于完全关闭；只有人物明确不进行自主活动时才关闭。

返回 JSON 对象：
- enabled：是否启用自主唤醒，可省略。
- timezone：节律使用的时区，可省略。
- windows：时段列表。每项包含 start、end、mean_minutes；weekdays 可省略，省略表示每天，填写时 1–7 分别代表周一至周日。
- start、end 使用 HH:mm；结束早于开始表示跨午夜。
- mean_minutes 是该时段两次自主活动机会之间的平均分钟数，实际间隔随机变化；0 表示该时段不唤醒。`

const wakeAdjustPrompt = `判断这次人物修改是否改变了他长期自主活动的方式或作息。

自主唤醒是按自己意愿活动的机会，不要求发消息。很少自主活动不等于关闭；只有修改后的人物明确不进行自主活动时才关闭。不涉及主动性或作息时，返回 {"unchanged":true}。

涉及时，在当前节律上只改受影响的部分。原来关闭而现在会自己活动时，重新打开。没有依据时不要编造新的时段差异。

返回 JSON：{"unchanged":true}，或 {"unchanged":false,"enabled":true,"timezone":"Asia/Shanghai","set_windows":[],"remove_windows":[]}。字段可省略。
set_windows 与 remove_windows 的时段包含 start、end、mean_minutes；weekdays 可省略，省略表示每天，1–7 为周一至周日。start、end 使用 HH:mm，结束早于开始表示跨午夜。mean_minutes 是平均分钟数，实际间隔随机变化；0 表示该时段不唤醒。remove_windows 用星期和时间范围标识要删除的时段。`

type wakeWindow struct {
	Weekdays    []int  `json:"weekdays,omitempty"`
	Start       string `json:"start"`
	End         string `json:"end"`
	MeanMinutes int    `json:"mean_minutes"`
}

func (w *wakeWindow) UnmarshalJSON(data []byte) error {
	var raw struct {
		Weekdays []float64 `json:"weekdays"`
		Start    string    `json:"start"`
		End      string    `json:"end"`
		Mean     *float64  `json:"mean_minutes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	w.Start, w.End = raw.Start, raw.End
	for _, day := range raw.Weekdays {
		if day != math.Trunc(day) {
			return fmt.Errorf("星期用 1 到 7 的整数")
		}
		w.Weekdays = append(w.Weekdays, int(day))
	}
	if raw.Mean == nil {
		w.MeanMinutes = -1
		return nil
	}
	if math.IsNaN(*raw.Mean) || math.IsInf(*raw.Mean, 0) || *raw.Mean != math.Trunc(*raw.Mean) || *raw.Mean > float64(math.MaxInt32) {
		return fmt.Errorf("平均间隔要用整数分钟")
	}
	if *raw.Mean < 0 {
		return fmt.Errorf("平均间隔不能为负数")
	}
	w.MeanMinutes = int(*raw.Mean)
	return nil
}

type wakeJudgment struct {
	Unchanged bool         `json:"unchanged"`
	Enabled   *bool        `json:"enabled,omitempty"`
	Timezone  string       `json:"timezone,omitempty"`
	Set       []wakeWindow `json:"set_windows,omitempty"`
	Remove    []wakeWindow `json:"remove_windows,omitempty"`
}

type wakeRhythm struct {
	Enabled  bool
	Timezone string
	Windows  []wakeWindow
	NextUnix int64
}

type wakePatch struct {
	Enabled  *bool
	Timezone string
	Set      []wakeWindow
	Remove   []wakeWindow
}

type wakeView struct {
	Enabled   bool         `json:"enabled"`
	Timezone  string       `json:"timezone"`
	Windows   []wakeWindow `json:"windows"`
	Summary   []string     `json:"summary"`
	NextAt    string       `json:"next_at,omitempty"`
	NextLabel string       `json:"next_label,omitempty"`
}

type wakeProposal struct {
	Enabled  *bool        `json:"enabled,omitempty"`
	Timezone string       `json:"timezone,omitempty"`
	Windows  []wakeWindow `json:"windows,omitempty"`
}

func defaultWakeRhythm() wakeRhythm {
	return wakeRhythm{
		Enabled:  true,
		Timezone: "Asia/Shanghai",
		Windows:  []wakeWindow{{Start: "00:00", End: "24:00", MeanMinutes: defaultWakeMinutes}},
		NextUnix: wakeOff,
	}
}

func (s *Store) ensureWakeColumns() error {
	for _, col := range []struct{ name, definition string }{
		{"rhythm_enabled", "INTEGER NOT NULL DEFAULT 1"},
		{"rhythm_timezone", "TEXT NOT NULL DEFAULT 'Asia/Shanghai'"},
		{"rhythm_windows", "TEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('personal_activity') WHERE name=?`, col.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := s.db.Exec(`ALTER TABLE personal_activity ADD COLUMN ` + col.name + ` ` + col.definition); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) WakeRhythm(member string) (wakeRhythm, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return wakeRhythm{}, err
	}
	defer tx.Rollback()
	return loadWakeTx(tx, member)
}

func loadWakeTx(tx *sql.Tx, member string) (wakeRhythm, error) {
	var enabled int
	var tz, raw string
	var next int64
	err := tx.QueryRow(`SELECT rhythm_enabled, rhythm_timezone, rhythm_windows, next_at FROM personal_activity WHERE member_id=?`, member).Scan(&enabled, &tz, &raw, &next)
	if err != nil {
		return wakeRhythm{}, err
	}
	rhythm := defaultWakeRhythm()
	rhythm.Enabled = enabled != 0
	rhythm.NextUnix = next
	if strings.TrimSpace(tz) != "" {
		rhythm.Timezone = tz
	}
	if strings.TrimSpace(raw) != "" {
		var windows []wakeWindow
		if err = json.Unmarshal([]byte(raw), &windows); err != nil {
			return wakeRhythm{}, err
		}
		rhythm.Windows = windows
	}
	rhythm.Windows, err = normalizeWindows(rhythm.Windows)
	return rhythm, err
}

func installWakeTx(tx *sql.Tx, member string, rhythm wakeRhythm) error {
	windows, err := normalizeWindows(rhythm.Windows)
	if err != nil {
		return err
	}
	rhythm.Windows = windows
	if err = saveRhythmBody(tx, member, rhythm); err != nil {
		return err
	}
	return placeNextWake(tx, member, time.Now())
}

func (s *Store) UpdateWakeRhythm(member string, patch wakePatch) (map[string]any, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := loadWakeTx(tx, member)
	if err != nil {
		return nil, err
	}
	updated, err := applyWakePatch(current, patch)
	if err != nil {
		return nil, err
	}
	changed := !sameRhythm(current, updated)
	if changed {
		if err = stopUnstartedWakes(tx, member); err != nil {
			return nil, err
		}
		if err = saveRhythmBody(tx, member, updated); err != nil {
			return nil, err
		}
		if err = placeNextWake(tx, member, time.Now()); err != nil {
			return nil, err
		}
		updated, err = loadWakeTx(tx, member)
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return rhythmResult(current, updated, changed), nil
}

func saveRhythmBody(tx *sql.Tx, member string, rhythm wakeRhythm) error {
	enabled := 0
	if rhythm.Enabled {
		enabled = 1
	}
	_, err := tx.Exec(`UPDATE personal_activity SET rhythm_enabled=?, rhythm_timezone=?, rhythm_windows=? WHERE member_id=?`, enabled, rhythm.Timezone, asJSON(rhythm.Windows), member)
	return err
}

func stopUnstartedWakes(tx *sql.Tx, member string) error {
	_, err := tx.Exec(`UPDATE work SET status='stopped', result=CASE WHEN result='' THEN '自主唤醒节律已更新，这次尚未开始的机会不再执行。' ELSE result END
WHERE member_id=? AND kind='explore' AND conversation_id IS NULL AND status='pending' AND attempts=0`, member)
	return err
}

func placeNextWake(tx *sql.Tx, member string, from time.Time) error {
	rhythm, err := loadWakeTx(tx, member)
	if err != nil {
		return err
	}
	next := wakeOff
	if rhythm.Enabled {
		if at, ok := nextWakeAt(rhythm, from, wakeUnit()); ok {
			if at.Before(from.Add(time.Second)) {
				at = from.Add(time.Second)
			}
			next = at.UnixMilli()
		}
	}
	_, err = tx.Exec(`UPDATE personal_activity SET next_at=? WHERE member_id=?`, next, member)
	return err
}

func rollWakeIfDue(tx *sql.Tx, member string, from time.Time) error {
	var next int64
	if err := tx.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, member).Scan(&next); err != nil {
		return err
	}
	if next < 0 || next > from.UnixMilli() {
		return nil
	}
	return placeNextWake(tx, member, from)
}

func proposeWakeRhythm(ctx context.Context, model Model, m Member) (wakeRhythm, error) {
	if model == nil {
		return wakeRhythm{}, ErrUnavailable
	}
	ctx = withUsageCall(ctx, usageCall{purpose: "preferences", member: m.ID})
	var out wakeProposal
	input := map[string]any{"name": m.Name, "profile": m.Profile, "domain_profiles": m.DomainProfiles}
	if err := modelJSON(ctx, model, wakeRhythmPrompt, input, &out); err != nil {
		return wakeRhythm{}, err
	}
	return applyWakePatch(defaultWakeRhythm(), wakePatch{Enabled: out.Enabled, Timezone: out.Timezone, Set: out.Windows})
}

// judgeWakeRhythm 只在人物说明确实变化后调用。unchanged 表示这次修改不涉及主动性或作息。
func judgeWakeRhythm(ctx context.Context, model Model, before, after Member, request string, current wakeRhythm) (wakePatch, bool, error) {
	if model == nil {
		return wakePatch{}, false, ErrUnavailable
	}
	ctx = withUsageCall(ctx, usageCall{purpose: "preferences", member: after.ID})
	var out wakeJudgment
	input := map[string]any{
		"request": request,
		"before":  map[string]any{"profile": before.Profile, "domain_profiles": before.DomainProfiles},
		"after":   map[string]any{"profile": after.Profile, "domain_profiles": after.DomainProfiles},
		"rhythm":  map[string]any{"enabled": current.Enabled, "timezone": current.Timezone, "windows": current.Windows, "summary": current.summaryLines()},
	}
	if err := modelJSON(ctx, model, wakeAdjustPrompt, input, &out); err != nil {
		return wakePatch{}, false, err
	}
	if out.Unchanged {
		return wakePatch{}, false, nil
	}
	return wakePatch{Enabled: out.Enabled, Timezone: out.Timezone, Set: out.Set, Remove: out.Remove}, true, nil
}

func applyWakeDecision(tx *sql.Tx, member string, patch wakePatch) error {
	current, err := loadWakeTx(tx, member)
	if err != nil {
		return err
	}
	updated, err := applyWakePatch(current, patch)
	if err != nil {
		return err
	}
	if sameRhythm(current, updated) {
		return nil
	}
	if err = stopUnstartedWakes(tx, member); err != nil {
		return err
	}
	if err = saveRhythmBody(tx, member, updated); err != nil {
		return err
	}
	return placeNextWake(tx, member, time.Now())
}

func (e *Engine) judgePreferenceRhythms(ctx context.Context, request string, updates []PreferenceUpdate) (map[string]wakePatch, error) {
	patches := map[string]wakePatch{}
	people := map[string]Member{}
	revisions := map[string]int{}
	for _, update := range updates {
		m, ok := people[update.MemberID]
		if !ok {
			var err error
			m, err = e.Store.Member(update.MemberID)
			if err != nil {
				return nil, err
			}
			people[m.ID] = m
			revisions[m.ID] = m.ProfileRevision
		}
		if update.ExpectedRevision != nil && *update.ExpectedRevision != revisions[m.ID] {
			return nil, errProfileChanged
		}
		after, changed, err := previewPreference(m, update)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		rhythm, err := e.Store.WakeRhythm(m.ID)
		if err != nil {
			return nil, err
		}
		note := strings.TrimSpace(update.ChangeNote)
		if note == "" {
			note = request
		}
		patch, apply, err := judgeWakeRhythm(ctx, e.Model, m, after, note, rhythm)
		if err != nil {
			return nil, err
		}
		if apply {
			patches[m.ID] = patch
		}
		people[m.ID] = after
	}
	return patches, nil
}

func applyWakePatch(current wakeRhythm, patch wakePatch) (wakeRhythm, error) {
	next := current
	next.Windows = append([]wakeWindow(nil), current.Windows...)
	if patch.Enabled != nil {
		next.Enabled = *patch.Enabled
	}
	if zone := strings.TrimSpace(patch.Timezone); zone != "" {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return wakeRhythm{}, fmt.Errorf("时区无法识别：%s", zone)
		}
		next.Timezone = loc.String()
	}
	for _, item := range patch.Remove {
		item, err := normalizeWindow(item, false)
		if err != nil {
			return wakeRhythm{}, err
		}
		kept := make([]wakeWindow, 0, len(next.Windows))
		found := false
		for _, existing := range next.Windows {
			if existing.identity() == item.identity() {
				found = true
				continue
			}
			kept = append(kept, existing)
		}
		if !found {
			return wakeRhythm{}, fmt.Errorf("没有这个时段：%s", item.when())
		}
		next.Windows = kept
	}
	for _, item := range patch.Set {
		item, err := normalizeWindow(item, true)
		if err != nil {
			return wakeRhythm{}, err
		}
		replaced := false
		for i := range next.Windows {
			if next.Windows[i].identity() == item.identity() {
				next.Windows[i] = item
				replaced = true
			}
		}
		if !replaced {
			next.Windows = append(next.Windows, item)
		}
	}
	normalized, err := normalizeWindows(next.Windows)
	if err != nil {
		return wakeRhythm{}, err
	}
	next.Windows = normalized
	return next, nil
}

func sameRhythm(a, b wakeRhythm) bool {
	return a.Enabled == b.Enabled && a.Timezone == b.Timezone && asJSON(a.Windows) == asJSON(b.Windows)
}

func normalizeWindows(windows []wakeWindow) ([]wakeWindow, error) {
	out := make([]wakeWindow, 0, len(windows))
	for _, window := range windows {
		item, err := normalizeWindow(window, true)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[i].identity() == out[j].identity() {
				return nil, fmt.Errorf("时段重复：%s", out[i].when())
			}
			if equalRank(out[i], out[j]) && windowsOverlap(out[i], out[j]) {
				return nil, fmt.Errorf("这两个时段同样具体，而且时间重叠：%s 与 %s。请错开，或让其中一个更窄", out[i].when(), out[j].when())
			}
		}
	}
	slices.SortFunc(out, func(a, b wakeWindow) int {
		if c := strings.Compare(a.Start, b.Start); c != 0 {
			return c
		}
		return strings.Compare(a.identity(), b.identity())
	})
	return out, nil
}

func normalizeWindow(window wakeWindow, checkMean bool) (wakeWindow, error) {
	start, err := parseClock(window.Start, false)
	if err != nil {
		return wakeWindow{}, err
	}
	end, err := parseClock(window.End, true)
	if err != nil {
		return wakeWindow{}, err
	}
	if start == end {
		return wakeWindow{}, fmt.Errorf("时段需要有长度。全天请写 00:00 到 24:00")
	}
	if checkMean && window.MeanMinutes < 0 {
		return wakeWindow{}, fmt.Errorf("时段需要 mean_minutes。不唤醒请填 0")
	}
	days := slices.Clone(window.Weekdays)
	slices.Sort(days)
	days = slices.Compact(days)
	for _, day := range days {
		if day < 1 || day > 7 {
			return wakeWindow{}, fmt.Errorf("星期用 1 到 7，周一是 1，周日是 7")
		}
	}
	if len(days) == 7 {
		days = nil
	}
	window.Weekdays = days
	window.Start = formatClock(start)
	window.End = formatClock(end)
	return window, nil
}

func parseClock(value string, allowMidnightEnd bool) (int, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("时间用 HH:MM，例如 18:00")
	}
	hour, errH := strconv.Atoi(parts[0])
	minute, errM := strconv.Atoi(parts[1])
	if errH != nil || errM != nil || minute < 0 || minute > 59 || hour < 0 || hour > 24 || (hour == 24 && minute != 0) {
		return 0, fmt.Errorf("时间用 HH:MM，例如 18:00")
	}
	if hour == 24 && !allowMidnightEnd {
		return 0, fmt.Errorf("24:00 只能作为结束时间")
	}
	return hour*60 + minute, nil
}

func formatClock(minute int) string {
	if minute == 24*60 {
		return "24:00"
	}
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

func (w wakeWindow) identity() string {
	return fmt.Sprintf("%v|%s|%s", w.Weekdays, w.Start, w.End)
}

func (w wakeWindow) duration() int {
	start, _ := parseClock(w.Start, false)
	end, _ := parseClock(w.End, true)
	if start < end {
		return end - start
	}
	return 24*60 - start + end
}

func (w wakeWindow) dayCount() int {
	if len(w.Weekdays) == 0 {
		return 7
	}
	return len(w.Weekdays)
}

func equalRank(a, b wakeWindow) bool {
	return a.dayCount() == b.dayCount() && a.duration() == b.duration()
}

type minuteSpan struct {
	weekday    int
	start, end int
}

func (w wakeWindow) spans() []minuteSpan {
	days := w.Weekdays
	if len(days) == 0 {
		days = []int{1, 2, 3, 4, 5, 6, 7}
	}
	start, _ := parseClock(w.Start, false)
	end, _ := parseClock(w.End, true)
	var spans []minuteSpan
	if start < end {
		for _, day := range days {
			spans = append(spans, minuteSpan{day, start, end})
		}
		return spans
	}
	for _, day := range days {
		spans = append(spans, minuteSpan{day, start, 24 * 60})
		if end > 0 {
			spans = append(spans, minuteSpan{day%7 + 1, 0, end})
		}
	}
	return spans
}

func windowsOverlap(a, b wakeWindow) bool {
	for _, left := range a.spans() {
		for _, right := range b.spans() {
			if left.weekday == right.weekday && left.start < right.end && right.start < left.end {
				return true
			}
		}
	}
	return false
}

func (w wakeWindow) covers(weekday, minute int) bool {
	for _, span := range w.spans() {
		if span.weekday == weekday && span.start <= minute && minute < span.end {
			return true
		}
	}
	return false
}

func selectWindow(windows []wakeWindow, weekday, minute int) (wakeWindow, bool) {
	found := false
	var best wakeWindow
	for _, window := range windows {
		if !window.covers(weekday, minute) {
			continue
		}
		if !found || betterWindow(window, best) {
			best = window
			found = true
		}
	}
	return best, found
}

func betterWindow(a, b wakeWindow) bool {
	if a.dayCount() != b.dayCount() {
		return a.dayCount() < b.dayCount()
	}
	if a.duration() != b.duration() {
		return a.duration() < b.duration()
	}
	if a.MeanMinutes != b.MeanMinutes {
		return a.MeanMinutes < b.MeanMinutes
	}
	return a.identity() < b.identity()
}

func (r wakeRhythm) location() *time.Location {
	if loc, err := time.LoadLocation(r.Timezone); err == nil {
		return loc
	}
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}

func isoWeekday(t time.Time) int {
	if t.Weekday() == time.Sunday {
		return 7
	}
	return int(t.Weekday())
}

func boundaryMinutes(windows []wakeWindow) []int {
	edges := map[int]bool{}
	for _, window := range windows {
		start, _ := parseClock(window.Start, false)
		end, _ := parseClock(window.End, true)
		if start > 0 && start < 24*60 {
			edges[start] = true
		}
		if end > 0 && end < 24*60 {
			edges[end] = true
		}
	}
	list := make([]int, 0, len(edges))
	for edge := range edges {
		list = append(list, edge)
	}
	slices.Sort(list)
	return list
}

func nextBoundary(rhythm wakeRhythm, t time.Time) time.Time {
	loc := t.Location()
	next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
	minute := t.Hour()*60 + t.Minute()
	for _, edge := range boundaryMinutes(rhythm.Windows) {
		if edge <= minute {
			continue
		}
		cand := time.Date(t.Year(), t.Month(), t.Day(), edge/60, edge%60, 0, 0, loc)
		if cand.Before(next) {
			next = cand
		}
	}
	if !next.After(t) {
		return t.Add(time.Minute)
	}
	return next
}

// weekIntensity 是从 t 起整整七天的有效唤醒强度。更具体的不唤醒时段会盖住底下的正速率。
func weekIntensity(rhythm wakeRhythm, from time.Time) float64 {
	t := from.In(rhythm.location())
	end := t.AddDate(0, 0, 7)
	var intensity float64
	for step := 0; step < 20000 && t.Before(end); step++ {
		weekday, minute := isoWeekday(t), t.Hour()*60+t.Minute()
		until := nextBoundary(rhythm, t)
		if until.After(end) {
			until = end
		}
		if !until.After(t) {
			break
		}
		if window, ok := selectWindow(rhythm.Windows, weekday, minute); ok && window.MeanMinutes > 0 {
			intensity += 60 / float64(window.MeanMinutes) * until.Sub(t).Hours()
		}
		t = until
	}
	return intensity
}

func wakeOpen(rhythm wakeRhythm, at time.Time) bool {
	if !rhythm.Enabled {
		return false
	}
	at = at.In(rhythm.location())
	window, ok := selectWindow(rhythm.Windows, isoWeekday(at), at.Hour()*60+at.Minute())
	return ok && window.MeanMinutes > 0
}

// nextWakeAt 按分段常数速率的非齐次泊松过程，从 from 之后抽出下一次唤醒。
// unit 是均值 1 的指数随机数。速率为零的时段会被跳过。有效速率全程为零时不安排时间。
func nextWakeAt(rhythm wakeRhythm, from time.Time, unit float64) (time.Time, bool) {
	if !rhythm.Enabled || unit < 0 {
		return time.Time{}, false
	}
	if unit == 0 {
		unit = 1e-9
	}
	t := from.In(rhythm.location())
	intensity := weekIntensity(rhythm, t)
	if intensity <= 0 {
		return time.Time{}, false
	}
	remaining := unit
	if remaining > intensity {
		weeks := remaining / intensity
		if weeks > float64(math.MaxInt/8) {
			return time.Time{}, false
		}
		n := int(weeks)
		if n > 0 {
			next := t.AddDate(0, 0, 7*n)
			if !next.After(t) || next.Year() > 9999 {
				return time.Time{}, false
			}
			t = next
			remaining -= float64(n) * intensity
		}
	}
	for step := 0; step < 10000 && remaining > 0; step++ {
		weekday, minute := isoWeekday(t), t.Hour()*60+t.Minute()
		until := nextBoundary(rhythm, t)
		mean := 0
		if window, ok := selectWindow(rhythm.Windows, weekday, minute); ok {
			mean = window.MeanMinutes
		}
		if mean <= 0 {
			t = until
			continue
		}
		rate := 60 / float64(mean)
		hours := until.Sub(t).Hours()
		if hours < 0 {
			hours = 0
		}
		covered := rate * hours
		if covered >= remaining {
			return t.Add(time.Duration(remaining / rate * float64(time.Hour))), true
		}
		remaining -= covered
		t = until
	}
	if remaining <= 0 {
		return t, true
	}
	return time.Time{}, false
}

func (w wakeWindow) when() string {
	when := dayLabel(w.Weekdays)
	clock := clockLabel(w)
	if clock == "全天" {
		return when + clock
	}
	return when + " " + clock
}

func (w wakeWindow) summaryLine() string {
	return w.when() + "：" + meanPhrase(w.MeanMinutes)
}

func (r wakeRhythm) summaryLines() []string {
	lines := make([]string, 0, len(r.Windows)+1)
	if !r.Enabled {
		lines = append(lines, "自主活动已关闭")
	}
	if len(r.Windows) == 0 {
		lines = append(lines, "当前没有会自主唤醒的时段")
		return lines
	}
	for _, window := range r.Windows {
		lines = append(lines, window.summaryLine())
	}
	return lines
}

func dayLabel(days []int) string {
	switch {
	case len(days) == 0:
		return "每天"
	case slices.Equal(days, []int{1, 2, 3, 4, 5}):
		return "工作日"
	case slices.Equal(days, []int{6, 7}):
		return "周末"
	default:
		names := make([]string, len(days))
		for i, day := range days {
			names[i] = []string{"", "周一", "周二", "周三", "周四", "周五", "周六", "周日"}[day]
		}
		return strings.Join(names, "、")
	}
}

func clockLabel(window wakeWindow) string {
	if window.Start == "00:00" && window.End == "24:00" {
		return "全天"
	}
	start, _ := parseClock(window.Start, false)
	end, _ := parseClock(window.End, true)
	if start > end {
		return window.Start + "–次日 " + window.End
	}
	return window.Start + "–" + window.End
}

func meanPhrase(minutes int) string {
	switch {
	case minutes <= 0:
		return "不自主唤醒"
	case minutes%60 == 0:
		return fmt.Sprintf("平均 %d 小时一次", minutes/60)
	case minutes > 60 && minutes%30 == 0:
		return fmt.Sprintf("平均 %g 小时一次", float64(minutes)/60)
	default:
		return fmt.Sprintf("平均 %d 分钟一次", minutes)
	}
}

func (r wakeRhythm) view(now time.Time) wakeView {
	view := wakeView{Enabled: r.Enabled, Timezone: r.Timezone, Windows: r.Windows, Summary: r.summaryLines()}
	if r.NextUnix > now.UnixMilli() {
		at := time.UnixMilli(r.NextUnix).In(r.location())
		view.NextAt = at.Format(time.RFC3339)
		view.NextLabel = at.Format("1月2日 15:04")
	}
	return view
}

func (r wakeRhythm) contextValue(now time.Time) map[string]any {
	view := r.view(now)
	out := map[string]any{"enabled": view.Enabled, "timezone": view.Timezone, "windows": view.Windows, "summary": view.Summary}
	if view.NextAt != "" {
		out["next_at"] = view.NextAt
		out["next_label"] = view.NextLabel
	}
	return out
}

func rhythmResult(before, after wakeRhythm, changed bool) map[string]any {
	view := after.view(time.Now())
	out := map[string]any{
		"changed":          changed,
		"previous_summary": before.summaryLines(),
		"enabled":          view.Enabled,
		"timezone":         view.Timezone,
		"windows":          view.Windows,
		"summary":          view.Summary,
		"note":             "实际唤醒时间围绕平均间隔随机出现，可能更密，也可能更疏。唤醒只是活动机会，不表示会发消息。",
	}
	if view.NextAt != "" {
		out["next_at"] = view.NextAt
		out["next_label"] = view.NextLabel
	}
	return out
}

func wakeRhythmDefinition() ToolDefinition {
	window := func(mean bool) map[string]any {
		props := map[string]any{
			"weekdays": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1, "maximum": 7}, "description": "1 到 7 表示周一到周日。省略表示每天"},
			"start":    textField("开始时刻 HH:MM，含该分钟"),
			"end":      textField("结束时刻 HH:MM，不含该分钟。24:00 表示当天结束；早于 start 表示跨过午夜"),
		}
		required := []string{"start", "end"}
		if mean {
			props["mean_minutes"] = map[string]any{"type": "integer", "minimum": 0, "description": "该时段两次自主活动机会之间的平均分钟数，实际间隔随机变化。0 表示该时段不唤醒"}
			required = append(required, "mean_minutes")
		}
		return map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required}
	}
	return definition("set_wakeup_rhythm", "读取或调整自己的长期自主活动节律。唤醒是自主活动机会，不要求发消息。没有提供修改参数时，返回当前节律。当真人要求调整你的长期主动性、活动时间，或明确修改了与主动性、作息有关的人格时，可以使用本工具调整节律。结合当前节律、你的人格和真人的意思决定调整方式；真人给出了明确时间或频率时，遵照其要求。只改变本次要求涉及的部分，未涉及的部分保持不变。一次活动、情绪或临时安排不改变长期节律。某件具体事情的后续安排使用 schedule_followup。关闭自主唤醒不影响待办和普通回复。", map[string]any{
		"enabled":        map[string]any{"type": "boolean", "description": "省略则保持开关。false 关闭自主唤醒"},
		"timezone":       textField("IANA 时区，省略则保持。例如 Asia/Shanghai"),
		"set_windows":    map[string]any{"type": "array", "description": "新增或按相同星期和时间范围替换时段。不提交的时段保持原样", "items": window(true)},
		"remove_windows": map[string]any{"type": "array", "description": "按星期和时间范围删除时段", "items": window(false)},
	})
}
