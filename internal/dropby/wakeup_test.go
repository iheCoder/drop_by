package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWakeSamplingFollowsSegmentsAndStaysIrregular(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	at := func(day, hour, minute int) time.Time {
		return time.Date(2026, 11, day, hour, minute, 0, 0, loc)
	}
	if isoWeekday(at(16, 10, 0)) != 1 {
		t.Fatal("2026-11-16 应该是周一")
	}
	near := func(got, want time.Time) {
		t.Helper()
		if d := got.Sub(want); d < -time.Second || d > time.Second {
			t.Fatalf("got %s want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
	allDay := func(mean int) wakeWindow {
		return wakeWindow{Start: "00:00", End: "24:00", MeanMinutes: mean}
	}

	t.Run("平均间隔", func(t *testing.T) {
		rhythm := wakeRhythm{Enabled: true, Timezone: "Asia/Shanghai", Windows: []wakeWindow{allDay(60)}}
		got, ok := nextWakeAt(rhythm, at(16, 10, 0), 1)
		if !ok {
			t.Fatal("没有下一次")
		}
		near(got, at(16, 11, 0))
	})

	t.Run("安静时段会被跳过", func(t *testing.T) {
		rhythm := wakeRhythm{Enabled: true, Timezone: "Asia/Shanghai", Windows: []wakeWindow{
			allDay(60),
			{Start: "00:00", End: "08:00", MeanMinutes: 0},
		}}
		var err error
		rhythm.Windows, err = normalizeWindows(rhythm.Windows)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := nextWakeAt(rhythm, at(16, 7, 0), 0.5)
		if !ok {
			t.Fatal("没有下一次")
		}
		near(got, at(16, 8, 30))
		rng := mrand.New(mrand.NewPCG(20261113, 3))
		cursor := at(16, 1, 0)
		late := false
		for range 80 {
			cursor, ok = nextWakeAt(rhythm, cursor, rng.ExpFloat64())
			if !ok {
				t.Fatal("链断了")
			}
			local := cursor.In(loc)
			if local.Hour() < 8 {
				t.Fatalf("安静时段被唤醒: %s", local.Format(time.RFC3339))
			}
			if local.Hour() >= 18 {
				late = true
			}
		}
		if !late {
			t.Fatal("唤醒都挤在安静时段刚结束的地方")
		}
	})

	t.Run("工作日和周末", func(t *testing.T) {
		rhythm := wakeRhythm{Enabled: true, Timezone: "Asia/Shanghai", Windows: []wakeWindow{
			{Weekdays: []int{1, 2, 3, 4, 5}, Start: "00:00", End: "24:00", MeanMinutes: 360},
			{Weekdays: []int{6, 7}, Start: "00:00", End: "24:00", MeanMinutes: 30},
		}}
		var err error
		rhythm.Windows, err = normalizeWindows(rhythm.Windows)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := nextWakeAt(rhythm, at(16, 10, 0), 1)
		if !ok {
			t.Fatal("工作日没有下一次")
		}
		near(got, at(16, 16, 0))
		got, ok = nextWakeAt(rhythm, at(21, 10, 0), 1)
		if !ok || isoWeekday(at(21, 10, 0)) != 6 {
			t.Fatal("周六样本无效", got, ok)
		}
		near(got, at(21, 10, 30))
		rng := mrand.New(mrand.NewPCG(20261113, 5))
		cursor := at(16, 0, 0)
		weekend, weekday := 0, 0
		for range 240 {
			cursor, ok = nextWakeAt(rhythm, cursor, rng.ExpFloat64())
			if !ok {
				t.Fatal("链断了")
			}
			if isoWeekday(cursor.In(loc)) >= 6 {
				weekend++
			} else {
				weekday++
			}
		}
		if weekend < weekday*2 {
			t.Fatalf("周末更高的频率没有体现在分布里: 周末 %d 工作日 %d", weekend, weekday)
		}
	})

	t.Run("跨午夜", func(t *testing.T) {
		rhythm := wakeRhythm{Enabled: true, Timezone: "Asia/Shanghai", Windows: []wakeWindow{
			allDay(0),
			{Start: "22:00", End: "02:00", MeanMinutes: 60},
		}}
		var err error
		rhythm.Windows, err = normalizeWindows(rhythm.Windows)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := nextWakeAt(rhythm, at(16, 3, 0), 0.5)
		if !ok {
			t.Fatal("没有跨到夜里")
		}
		near(got, at(16, 22, 30))
		got, ok = nextWakeAt(rhythm, at(16, 23, 0), 2)
		if !ok {
			t.Fatal("夜里没有延续")
		}
		near(got, at(17, 1, 0))
	})

	t.Run("不是固定节拍", func(t *testing.T) {
		rhythm := wakeRhythm{Enabled: true, Timezone: "Asia/Shanghai", Windows: []wakeWindow{allDay(60)}}
		rng := mrand.New(mrand.NewPCG(20261113, 7))
		from := at(16, 10, 0)
		const n = 400
		gaps := make([]float64, 0, n)
		seen := map[int]bool{}
		outside := 0
		for range n {
			next, ok := nextWakeAt(rhythm, from, rng.ExpFloat64())
			if !ok {
				t.Fatal("没有下一次")
			}
			gap := next.Sub(from).Minutes()
			gaps = append(gaps, gap)
			seen[int(gap+0.5)] = true
			if gap < 30 || gap > 90 {
				outside++
			}
		}
		mean, std := meanStd(gaps)
		if mean < 45 || mean > 80 || std/mean < 0.6 || gaps[minIndex(gaps)] > 15 || gaps[maxIndex(gaps)] < 180 || outside < n/5 || len(seen) < 40 {
			t.Fatalf("分布太像固定间隔: mean %.1f cv %.2f min %.1f max %.1f outside %d unique %d", mean, std/mean, gaps[minIndex(gaps)], gaps[maxIndex(gaps)], outside, len(seen))
		}
	})
}

func TestWakeScheduleDoesNotBurstOrReplay(t *testing.T) {
	s, _, m := fixture(t)
	previous := wakeUnit
	wakeUnit = func() float64 { return 1 }
	t.Cleanup(func() { wakeUnit = previous })

	if err := s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	var next int64
	if err := s.db.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	var again int64
	var pending int
	s.db.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&again)
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=? AND status='pending'`, m.ID).Scan(&pending)
	if again != next || pending != 1 {
		t.Fatalf("同一次唤醒被重复安排: next %d/%d pending %d", next, again, pending)
	}

	var id string
	if err := s.db.QueryRow(`SELECT id FROM work WHERE kind='explore' AND member_id=? AND status='pending'`, m.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Claim(id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := s.db.Exec(`UPDATE personal_activity SET next_at=? WHERE member_id=?`, time.Now().Add(-time.Hour).UnixMilli(), m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=? AND status IN ('pending','running')`, m.ID).Scan(&pending)
	if pending != 1 {
		t.Fatalf("运行中的活动又被排了一次: %d", pending)
	}
	job, err := scanWork(s.db.QueryRow(`SELECT `+workColumns+` FROM work WHERE id=?`, id))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(job, "done", "安静地结束", time.Now()); err != nil {
		t.Fatal(err)
	}
	var after int64
	s.db.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&after)
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=? AND status='pending'`, m.ID).Scan(&pending)
	if after <= time.Now().UnixMilli() || pending != 0 {
		t.Fatalf("结束后没有从结束时刻重抽: next %d pending %d", after, pending)
	}

	if _, err = s.db.Exec(`UPDATE personal_activity SET next_at=? WHERE member_id=?`, time.Now().Add(-8*time.Hour).UnixMilli(), m.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ScheduleExploration(); err != nil || s.ScheduleExploration() != nil {
		t.Fatal(err)
	}
	var created int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=? AND status='pending'`, m.ID).Scan(&created)
	if created != 1 {
		t.Fatalf("停机错过的唤醒被集中补跑: %d", created)
	}

	if _, err = s.db.Exec(`UPDATE work SET status='done' WHERE kind='explore' AND member_id=?`, m.ID); err != nil {
		t.Fatal(err)
	}
	var kept int64
	if err = s.db.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if err = s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&again)
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=? AND status='pending'`, m.ID).Scan(&pending)
	if again != kept || pending != 0 {
		t.Fatalf("重启重复执行了尚未到期的唤醒: next %d/%d pending %d", kept, again, pending)
	}

	on := true
	if _, err = s.UpdateWakeRhythm(m.ID, wakePatch{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE personal_activity SET next_at=0 WHERE member_id=?`, m.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err = s.UpdateWakeRhythm(m.ID, wakePatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	var enabled int
	s.db.QueryRow(`SELECT rhythm_enabled, next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&enabled, &after)
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=? AND status='pending'`, m.ID).Scan(&pending)
	if enabled != 0 || after != wakeOff || pending != 0 {
		t.Fatalf("关闭后仍会醒来: enabled %d next %d pending %d", enabled, after, pending)
	}
	if err = s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=? AND status='pending'`, m.ID).Scan(&pending)
	if pending != 0 {
		t.Fatal("关闭后仍安排了活动", pending)
	}
}

func TestWakeRhythmEditsKeepUnmentionedWindows(t *testing.T) {
	s, c, m := fixture(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	domains := []DomainProfile{{Domain: "写作", Profile: "要有具体的人和物件。"}}
	if _, err = setMemberProfile(tx, &m, m.Profile, domains, "edit", "补上写作画像", nil); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Member(m.ID)
	profile, revision := m.Profile, m.ProfileRevision
	if _, err = s.UpdateWakeRhythm(m.ID, wakePatch{Set: []wakeWindow{
		{Weekdays: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "18:00", MeanMinutes: 360},
		{Weekdays: []int{1, 2, 3, 4, 5}, Start: "18:00", End: "23:00", MeanMinutes: 120},
		{Weekdays: []int{6, 7}, Start: "00:00", End: "24:00", MeanMinutes: 90},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateWakeRhythm(m.ID, wakePatch{Set: []wakeWindow{
		{Weekdays: []int{5, 1, 1, 2, 3, 4}, Start: "9:00", End: "13:00", MeanMinutes: 60},
		{Weekdays: []int{1, 2, 3, 4, 5}, Start: "11:00", End: "15:00", MeanMinutes: 60},
	}}); err == nil || !strings.Contains(err.Error(), "重叠") {
		t.Fatal("同样具体的重叠时段被接受", err)
	}
	var missing []wakeWindow
	if err = json.Unmarshal([]byte(`[{"start":"00:00","end":"24:00"}]`), &missing); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateWakeRhythm(m.ID, wakePatch{Set: missing}); err == nil || !strings.Contains(err.Error(), "mean_minutes") {
		t.Fatal("缺少平均间隔被当成不唤醒", err)
	}
	if _, err = s.UpdateWakeRhythm(m.ID, wakePatch{Set: []wakeWindow{{Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "24:00", MeanMinutes: 60}}}); err == nil || !strings.Contains(err.Error(), "星期") {
		t.Fatal("非法星期被当成每天", err)
	}
	if _, err = s.UpdateWakeRhythm(m.ID, wakePatch{Set: []wakeWindow{{Start: "00:00", End: "24:00", MeanMinutes: 10}}}); err != nil {
		t.Fatal("明确写出的短间隔被拒绝", err)
	}
	if _, err = s.UpdateWakeRhythm(m.ID, wakePatch{Set: []wakeWindow{{Start: "00:00", End: "24:00", MeanMinutes: 360}}}); err != nil {
		t.Fatal(err)
	}

	msg := userMessage(c, "你以后晚上活跃一点，白天照旧。")
	if err = s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, defs []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			if !strings.Contains(ms[0].Content, "不必为了这次唤醒") || !strings.Contains(asJSON(ms), "平均 2 小时一次") {
				t.Error("行动没有拿到唤醒的含义或当前晚上的间隔")
			}
			names := map[string]bool{}
			for _, def := range defs {
				names[def.Function.Name] = true
			}
			if !names["set_wakeup_rhythm"] || !names["schedule_followup"] {
				t.Error("节律工具或待办预约缺失", names)
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("set_wakeup_rhythm", map[string]any{
				"set_windows": []wakeWindow{{Weekdays: []int{1, 2, 3, 4, 5}, Start: "18:00", End: "23:00", MeanMinutes: 60}},
			})}}, nil
		}
		return jsonReply(Outcome{Outcome: "晚上的间隔改短了。"}), nil
	}), testConfig())
	if _, err = e.act(context.Background(), c, m, w, "继续"); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Member(m.ID)
	if saved.Profile != profile || saved.ProfileRevision != revision || len(saved.DomainProfiles) != 1 || saved.DomainProfiles[0].Profile != domains[0].Profile {
		t.Fatal("改节律时改写了人格或领域画像", saved)
	}
	rhythm, err := s.WakeRhythm(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	means := map[string]int{}
	for _, window := range rhythm.Windows {
		means[window.identity()] = window.MeanMinutes
	}
	if means["[1 2 3 4 5]|09:00|18:00"] != 360 || means["[1 2 3 4 5]|18:00|23:00"] != 60 || means["[6 7]|00:00|24:00"] != 90 || means["[]|00:00|24:00"] != 360 {
		t.Fatalf("未提到的时段被改掉: %+v", rhythm.Windows)
	}
	var status, raw string
	if err = s.db.QueryRow(`SELECT status, result FROM tool_runs WHERE member_id=? AND name='set_wakeup_rhythm'`, m.ID).Scan(&status, &raw); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || !strings.Contains(raw, "平均 2 小时一次") || !strings.Contains(raw, "平均 1 小时一次") {
		t.Fatal("工具回读没有保留修改前后的节律", status, raw)
	}
}

func TestPersonalityChangeCanUpdateRhythmSeparately(t *testing.T) {
	s, c, m := fixture(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	domains := []DomainProfile{{Domain: "写作", Profile: "要有具体的人和物件。"}}
	if _, err = setMemberProfile(tx, &m, m.Profile, domains, "edit", "补上写作画像", nil); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	m, _ = s.Member(m.ID)
	source := userMessage(c, "你本身以后更主动，会自己找事情做。写作的口味不用改。")
	if err = s.AddUserMessage(source, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	updated := m.Profile + " 我会自己找事情做。"
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if reply, ok := replyIfWakeAdjust(ms); ok {
			return reply, nil
		}
		calls++
		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
				toolCall("apply_instruction", map[string]any{"source_id": source.ID, "preference_updates": []PreferenceUpdate{{
					MemberID: m.ID, Profile: updated, SourceID: source.ID, ExpectedRevision: &m.ProfileRevision,
				}}}),
				toolCall("set_wakeup_rhythm", map[string]any{"set_windows": []wakeWindow{{Start: "00:00", End: "24:00", MeanMinutes: 30}}}),
			}}, nil
		}
		return jsonReply(Outcome{Outcome: "性格和活动节律都按这句话改了。"}), nil
	}), testConfig())
	if _, err = e.act(context.Background(), c, m, w, "继续"); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Member(m.ID)
	if saved.Profile != updated || len(saved.DomainProfiles) != 1 || saved.DomainProfiles[0] != domains[0] {
		t.Fatal("人格修改带丢了领域画像", saved.Profile, saved.DomainProfiles)
	}
	rhythm, err := s.WakeRhythm(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rhythm.Windows) != 1 || rhythm.Windows[0].MeanMinutes != 30 || rhythm.Windows[0].Start != "00:00" {
		t.Fatal("主动性没有写进节律", rhythm.Windows)
	}
	if strings.Contains(saved.Profile, "mean_minutes") || strings.Contains(asJSON(saved.DomainProfiles), "18:00") {
		t.Fatal("节律写进了人格或领域画像")
	}
}

func TestNewMemberRhythmFollowsProfile(t *testing.T) {
	s, _, _ := fixture(t)
	var before int
	s.db.QueryRow(`SELECT count(*) FROM members`).Scan(&before)
	h := NewHandler(NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.Contains(messages[0].Content, "自主唤醒") {
			if !strings.Contains(messages[0].Content, "省略 windows") || !strings.Contains(messages[0].Content, "只有人物明确不进行自主活动时才关闭") {
				t.Error("创建节律提示没有说明默认值和关闭条件")
			}
			var in struct {
				Profile string `json:"profile"`
			}
			if err := json.Unmarshal([]byte(messages[len(messages)-1].Content), &in); err != nil {
				t.Fatal(err)
			}
			switch {
			case strings.Contains(in.Profile, "几乎不自己行动"):
				return jsonReply(map[string]any{"enabled": false}), nil
			case strings.Contains(in.Profile, "白天把世界关在外面"):
				return jsonReply(map[string]any{"windows": []wakeWindow{
					{Weekdays: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "18:00", MeanMinutes: 0},
					{Weekdays: []int{1, 2, 3, 4, 5}, Start: "18:00", End: "23:00", MeanMinutes: 30},
				}}), nil
			case strings.Contains(in.Profile, "停不下来"):
				return jsonReply(map[string]any{"windows": []wakeWindow{{Start: "00:00", End: "24:00", MeanMinutes: 30}}}), nil
			default:
				return jsonReply(map[string]any{"windows": []any{}}), nil
			}
		}
		var in struct {
			Direction string `json:"direction"`
		}
		json.Unmarshal([]byte(messages[len(messages)-1].Content), &in)
		profile := "我按自己的节奏慢慢来。"
		switch {
		case strings.Contains(in.Direction, "几乎不自己行动"):
			profile = "我几乎不自己行动，除非有人来找我。"
		case strings.Contains(in.Direction, "白天安静"):
			profile = "我白天把世界关在外面，晚上才愿意自己找事情做。"
		case strings.Contains(in.Direction, "停不下来"):
			profile = "我停不下来，总想自己找下一件事。"
		}
		return jsonReply(map[string]any{"profile": profile}), nil
	}), testConfig()))
	create := func(name, direction string) Member {
		t.Helper()
		response := preferenceRequest(h, "POST", "/api/members", map[string]string{"name": name, "direction": direction})
		var m Member
		if response.Code != 201 || json.Unmarshal(response.Body.Bytes(), &m) != nil {
			t.Fatal(response.Code, response.Body.String())
		}
		return m
	}
	plain := create("慢慢", "")
	quiet := create("夜猫", "白天安静")
	eager := create("忙忙", "停不下来")
	absent := create("隐隐", "几乎不自己行动")
	check := func(m Member, profile string, want func(wakeRhythm)) {
		t.Helper()
		saved, err := s.Member(m.ID)
		if err != nil || saved.Profile != profile || len(saved.DomainProfiles) != 0 {
			t.Fatal("人格或领域画像没有按原文保存", saved, err)
		}
		rhythm, err := s.WakeRhythm(m.ID)
		if err != nil {
			t.Fatal(err)
		}
		want(rhythm)
	}
	check(plain, "我按自己的节奏慢慢来。", func(rhythm wakeRhythm) {
		if !rhythm.Enabled || len(rhythm.Windows) != 1 || rhythm.Windows[0].MeanMinutes != 360 {
			t.Fatal("没有明确主动性时没有落到 6 小时", rhythm)
		}
	})
	check(quiet, "我白天把世界关在外面，晚上才愿意自己找事情做。", func(rhythm wakeRhythm) {
		text := strings.Join(rhythm.summaryLines(), "\n")
		if !strings.Contains(text, "工作日 09:00–18:00：不自主唤醒") || !strings.Contains(text, "工作日 18:00–23:00：平均 30 分钟一次") || !strings.Contains(text, "每天全天：平均 6 小时一次") {
			t.Fatal("白天和晚上没有分开", text)
		}
	})
	check(eager, "我停不下来，总想自己找下一件事。", func(rhythm wakeRhythm) {
		if len(rhythm.Windows) != 1 || rhythm.Windows[0].MeanMinutes != 30 {
			t.Fatal("积极的人没有用上 30 分钟", rhythm.Windows)
		}
	})
	check(absent, "我几乎不自己行动，除非有人来找我。", func(rhythm wakeRhythm) {
		if rhythm.Enabled || rhythm.NextUnix != wakeOff {
			t.Fatal("明确不活动的人仍会醒来", rhythm.Enabled, rhythm.NextUnix)
		}
	})
	if _, err := s.CreateConversation(absent.Name, "direct", []string{absent.ID}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=?`, absent.ID).Scan(&n)
	if n != 0 {
		t.Fatal("关闭的节律仍产生了活动", n)
	}

	h = NewHandler(NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.Contains(messages[0].Content, "自主唤醒") {
			return jsonReply(map[string]any{"windows": []map[string]string{{"start": "00:00", "end": "24:00"}}}), nil
		}
		return jsonReply(map[string]any{"profile": "我想每分钟都动一下。"}), nil
	}), testConfig()))
	rejected := preferenceRequest(h, "POST", "/api/members", map[string]string{"name": "过密", "direction": "非常积极"})
	var after int
	s.db.QueryRow(`SELECT count(*) FROM members`).Scan(&after)
	if rejected.Code == 201 || after != before+4 || !strings.Contains(rejected.Body.String(), "mean_minutes") {
		t.Fatalf("缺少平均间隔仍创建了成员: %d %s count %d", rejected.Code, rejected.Body.String(), after)
	}
}

func TestQuietRecoverySkipsOrdinaryWakeAndKeepsFollowup(t *testing.T) {
	s, c, m := fixture(t)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().In(loc)
	end := "24:00"
	if at.Hour() < 23 {
		end = fmt.Sprintf("%02d:00", at.Hour()+1)
	}
	if _, err = s.UpdateWakeRhythm(m.ID, wakePatch{Set: []wakeWindow{{Start: fmt.Sprintf("%02d:00", at.Hour()), End: end, MeanMinutes: 0}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE personal_activity SET next_at=? WHERE member_id=?`, at.Add(-time.Hour).UnixMilli(), m.ID); err != nil {
		t.Fatal(err)
	}
	source := addWork(t, s, c, m, "chat")
	if _, err = s.ScheduleFollowup(c, m, source, "看看那件具体的事", "他约过", at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	var explores, revisits int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore' AND member_id=? AND status='pending'`, m.ID).Scan(&explores)
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='revisit' AND member_id=? AND status='pending'`, m.ID).Scan(&revisits)
	var next int64
	s.db.QueryRow(`SELECT next_at FROM personal_activity WHERE member_id=?`, m.ID).Scan(&next)
	if explores != 0 || revisits != 1 || next <= at.UnixMilli() {
		t.Fatalf("安静时段仍补跑了过期唤醒，或跟进被取消: explore %d revisit %d next %d", explores, revisits, next)
	}
}

func TestCoveredSilenceHasNoFarWake(t *testing.T) {
	rhythm := defaultWakeRhythm()
	rhythm.Windows = append(rhythm.Windows,
		wakeWindow{Weekdays: []int{1, 2, 3, 4, 5}, Start: "00:00", End: "24:00", MeanMinutes: 0},
		wakeWindow{Weekdays: []int{6, 7}, Start: "00:00", End: "24:00", MeanMinutes: 0},
	)
	if _, ok := nextWakeAt(rhythm, time.Now(), 8); ok {
		t.Fatal("有效时段都不唤醒时仍算出了下一次")
	}
	s, _, m := fixture(t)
	if _, err := s.UpdateWakeRhythm(m.ID, wakePatch{Set: rhythm.Windows[1:]}); err != nil {
		t.Fatal(err)
	}
	saved, err := s.WakeRhythm(m.ID)
	if err != nil || saved.NextUnix != wakeOff {
		t.Fatal("全覆盖的安静节律留下了下一次时间", saved.NextUnix, err)
	}
}

func TestMemberAndRhythmCommitTogether(t *testing.T) {
	s, _, _ := fixture(t)
	var before int
	s.db.QueryRow(`SELECT count(*) FROM members`).Scan(&before)
	err := s.AddMemberAndRhythm(Member{ID: "rollback_person", Name: "回滚", Profile: "我。", CreatedAt: now()}, wakeRhythm{
		Enabled: true, Timezone: "Asia/Shanghai",
		Windows: []wakeWindow{{Weekdays: []int{0}, Start: "00:00", End: "24:00", MeanMinutes: 15}},
	})
	var after, activity int
	s.db.QueryRow(`SELECT count(*) FROM members`).Scan(&after)
	s.db.QueryRow(`SELECT count(*) FROM personal_activity WHERE member_id='rollback_person'`).Scan(&activity)
	if err == nil || after != before || activity != 0 {
		t.Fatal("无效节律留下了成员", err, after, before, activity)
	}
}

func TestProfileEditsUpdateRhythm(t *testing.T) {
	s, c, m := fixture(t)
	original := m.Profile
	engine := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if !strings.Contains(ms[0].Content, "判断这次人物修改") {
			t.Fatal("人物修改没有判断节律", ms[0].Content)
		}
		var in struct {
			After struct {
				Profile string `json:"profile"`
			} `json:"after"`
		}
		if err := json.Unmarshal([]byte(ms[len(ms)-1].Content), &in); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.Contains(in.After.Profile, "星期写错"):
			return jsonReply(map[string]any{"unchanged": false, "set_windows": []map[string]any{{"weekdays": []int{0}, "start": "00:00", "end": "24:00", "mean_minutes": 10}}}), nil
		case strings.Contains(in.After.Profile, "自己找事情做"):
			return jsonReply(map[string]any{"unchanged": false, "enabled": true, "set_windows": []wakeWindow{{Start: "00:00", End: "24:00", MeanMinutes: 45}}}), nil
		default:
			return jsonReply(map[string]any{"unchanged": true}), nil
		}
	}), testConfig())
	h := NewHandler(engine)
	review := func() {
		engine.dispatchProfileWakeReviews(t.Context())
		engine.wg.Wait()
	}
	tone := preferenceRequest(h, "PATCH", "/api/members/"+m.ID, map[string]any{"profile": original + "语气轻一点。", "profile_revision": m.ProfileRevision})
	if tone.Code != 200 {
		t.Fatal(tone.Body.String())
	}
	review()
	saved, _ := s.Member(m.ID)
	rhythm, _ := s.WakeRhythm(m.ID)
	if !strings.Contains(saved.Profile, "语气轻一点") || rhythm.Windows[0].MeanMinutes != defaultWakeMinutes {
		t.Fatal("无关修改改动了节律", saved.Profile, rhythm.Windows)
	}
	bad := preferenceRequest(h, "PATCH", "/api/members/"+saved.ID, map[string]any{"profile": saved.Profile + "星期写错。", "profile_revision": saved.ProfileRevision})
	// 模型作息参数非法时只放弃作息更新。手动保存的人格已经提交。
	if bad.Code != 200 {
		t.Fatal("无效的后台判断阻止了保存", bad.Body.String())
	}
	review()
	kept, _ := s.Member(m.ID)
	afterBad, _ := s.WakeRhythm(m.ID)
	if !strings.Contains(kept.Profile, "星期写错") || kept.ProfileRevision != saved.ProfileRevision+1 || !sameRhythm(rhythm, afterBad) {
		t.Fatal("不应回滚已提交的人格，也不应应用非法节律", kept.Profile, afterBad.Windows)
	}
	active := preferenceRequest(h, "PATCH", "/api/members/"+kept.ID, map[string]any{"profile": saved.Profile + "我会自己找事情做。", "profile_revision": kept.ProfileRevision})
	if active.Code != 200 {
		t.Fatal(active.Body.String())
	}
	review()
	finalMember, _ := s.Member(m.ID)
	finalRhythm, _ := s.WakeRhythm(m.ID)
	if !strings.Contains(finalMember.Profile, "自己找事情做") || finalRhythm.Windows[0].MeanMinutes != 45 {
		t.Fatal("后台判断没有应用到已保存的人格版本", finalMember.Profile, finalRhythm.Windows)
	}

	answer := NewHandler(NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.Contains(ms[0].Content, "判断这次人物修改") {
			return jsonReply(map[string]any{"unchanged": false, "enabled": false}), nil
		}
		return jsonReply(map[string]any{"profile": finalMember.Profile + "明确不进行自主活动。"}), nil
	}), testConfig()))
	q := preferenceRequest(answer, "POST", "/api/members/"+finalMember.ID+"/preferences", map[string]any{"question": Question{Text: "平时自己行动吗？"}, "answer": "明确不进行自主活动"})
	if q.Code != 200 {
		t.Fatal(q.Body.String())
	}
	afterAnswer, _ := s.Member(m.ID)
	afterRhythm, _ := s.WakeRhythm(m.ID)
	if !strings.Contains(afterAnswer.Profile, "明确不进行自主活动") || afterRhythm.Enabled || afterRhythm.NextUnix != wakeOff {
		t.Fatal("选择题没有和关闭节律一起保存", afterAnswer.Profile, afterRhythm.Enabled, afterRhythm.NextUnix)
	}

	c, err := s.Conversation(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	source := userMessage(c, "你以后晚上也自己看看。")
	if err = s.AddUserMessage(source, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, err = s.Conversation(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.Contains(ms[0].Content, "判断这次人物修改") {
			return jsonReply(map[string]any{"unchanged": false, "enabled": true, "set_windows": []wakeWindow{{Weekdays: []int{1, 2, 3, 4, 5}, Start: "18:00", End: "23:00", MeanMinutes: 50}}}), nil
		}
		calls++
		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("apply_instruction", map[string]any{"source_id": source.ID, "preference_updates": []PreferenceUpdate{{
				MemberID: m.ID, Profile: afterAnswer.Profile + "晚上也会自己看看。", SourceID: source.ID, ExpectedRevision: &afterAnswer.ProfileRevision,
			}}})}}, nil
		}
		return jsonReply(Outcome{Outcome: "晚上的节律一起改了。"}), nil
	}), testConfig())
	if _, err := e.act(context.Background(), c, m, w, "继续"); err != nil {
		t.Fatal(err)
	}
	chatted, _ := s.Member(m.ID)
	chatRhythm, _ := s.WakeRhythm(m.ID)
	if !strings.Contains(chatted.Profile, "晚上也会自己看看") || !chatRhythm.Enabled {
		t.Fatal("聊天里的人物修改没有重新打开节律", chatted.Profile, chatRhythm.Enabled)
	}
	found := false
	for _, window := range chatRhythm.Windows {
		if window.Start == "18:00" && window.End == "23:00" && window.MeanMinutes == 50 {
			found = true
		}
	}
	if !found {
		t.Fatal("聊天里的节律调整没有写上", chatRhythm.Windows)
	}
}

func TestLiveWakeRhythmPrompts(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_WAKE") != "1" {
		t.Skip("显式开启真实节律提示验收")
	}
	cfg, err := LoadConfig()
	if err != nil || !cfg.ModelReady() {
		t.Fatal("真实模型配置不可用", err)
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	model := &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 3 * time.Minute}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	t.Log("model", cfg.Models.Scenes[sceneMemberSettings].Model)
	personas := []struct {
		name, profile string
		mustEnable    bool
		mustDisable   bool
	}{
		{"安静钻研", "我话不多，也不爱把事情说给别人听。每天都会自己钻进一个问题里，不需要有人来找我。", true, false},
		{"健谈被动", "我很能聊，见面就停不下来。但自己一个人的时候不会特意找事做，习惯等别人先开口。", true, false},
		{"偶尔活动", "我偶尔会自己想起一件事，去看一眼。大多数时候没有特别的安排。", true, false},
		{"明确关闭", "我明确不进行自主活动，只在别人找我时才出现。", false, true},
	}
	for _, persona := range personas {
		rhythm, err := proposeWakeRhythm(ctx, model, Member{ID: newID("member"), Name: persona.name, Profile: persona.profile})
		if err != nil {
			t.Fatal(persona.name, err)
		}
		t.Log(persona.name, "enabled", rhythm.Enabled, "summary", rhythm.summaryLines())
		if persona.mustEnable && !rhythm.Enabled {
			t.Error(persona.name, "被关掉了自主唤醒")
		}
		if persona.mustDisable && rhythm.Enabled {
			t.Error(persona.name, "仍会自主唤醒")
		}
	}
	base := defaultWakeRhythm()
	before := Member{ID: newID("member"), Name: "阿苇", Profile: "我按自己的节奏慢慢来。"}
	tone := before
	tone.Profile += "说话可以更短一点。"
	if _, changed, err := judgeWakeRhythm(ctx, model, before, tone, "今天怎么这么安静", base); err != nil || changed {
		t.Fatal("临时安静改动了长期节律", changed, err)
	}
	evening := before
	evening.Profile += "以后晚上我会自己找事情做。"
	patch, changed, err := judgeWakeRhythm(ctx, model, before, evening, "以后晚上主动一些", base)
	if err != nil || !changed {
		t.Fatal("晚上的长期变化没有调整节律", changed, err)
	}
	updated, err := applyWakePatch(base, patch)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("晚上主动", updated.Enabled, updated.summaryLines())
	if !updated.Enabled {
		t.Error("改成晚上会自己找事情后仍关闭")
	}
}

func meanStd(values []float64) (mean, std float64) {
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	var variance float64
	for _, value := range values {
		delta := value - mean
		variance += delta * delta
	}
	return mean, mathSqrt(variance / float64(len(values)))
}

func mathSqrt(v float64) float64 {
	if v <= 0 {
		return 0
	}
	guess := v
	for range 40 {
		guess = (guess + v/guess) / 2
	}
	return guess
}

func minIndex(values []float64) int {
	best := 0
	for i, value := range values {
		if value < values[best] {
			best = i
		}
	}
	return best
}

func maxIndex(values []float64) int {
	best := 0
	for i, value := range values {
		if value > values[best] {
			best = i
		}
	}
	return best
}

func TestManualProfileSaveOvertakesPendingWakeReview(t *testing.T) {
	// 让第一笔模型请求挂起，再保存第二版。两个 PATCH 都应先落盘，
	// 第一版晚回来的关闭结果不能修改作息，也不能清掉第二版的待办。
	s, _, member := fixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	engine := NewEngine(s, modelFunc(func(ctx context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			close(started)
			select {
			case <-release:
				return jsonReply(map[string]any{"enabled": false}), nil
			case <-ctx.Done():
				return ChatMessage{}, ctx.Err()
			}
		}
		return jsonReply(map[string]any{"set_windows": []wakeWindow{{Start: "00:00", End: "24:00", MeanMinutes: 45}}}), nil
	}), testConfig())
	defer func() { cancel(); engine.wg.Wait() }()
	h := NewHandler(engine)
	path := "/api/members/" + member.ID
	save := func(profile string, revision int) Member {
		t.Helper()
		w := preferenceRequest(h, "PATCH", path, map[string]any{"profile": profile, "profile_revision": revision})
		var result Member
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil ||
			result.Profile != profile || result.ProfileRevision != revision+1 {
			t.Fatalf("保存应直接返回新版本：%d %s", w.Code, w.Body)
		}
		return result
	}

	first := save("我平时很少自己行动。", member.ProfileRevision)
	engine.dispatchProfileWakeReviews(ctx)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("第一版作息判断没有启动")
	}
	second := save("我现在喜欢自己找事情做。", first.ProfileRevision)
	persisted, err := s.Member(member.ID)
	if err != nil || persisted.ProfileRevision != second.ProfileRevision {
		t.Fatal("模型未返回时第二版应已写入", err)
	}

	close(release)
	engine.wg.Wait()
	rhythm, err := s.WakeRhythm(member.ID)
	if err != nil || !rhythm.Enabled || rhythm.Windows[0].MeanMinutes != defaultWakeMinutes {
		t.Fatalf("旧作息结果覆盖了新版本：%+v %v", rhythm, err)
	}

	engine.dispatchProfileWakeReviews(ctx)
	engine.wg.Wait()
	rhythm, err = s.WakeRhythm(member.ID)
	if err != nil || calls != 2 || !rhythm.Enabled || rhythm.Windows[0].MeanMinutes != 45 {
		t.Fatalf("应应用最新一版的判断：调用 %d，作息 %+v，错误 %v", calls, rhythm, err)
	}
}
