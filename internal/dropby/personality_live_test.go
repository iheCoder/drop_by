package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 真实模型的连续场景验收：通过真实创建、修改、工具行动、后台整理和后续聊天
// 观察效果。使用独立数据和累计账本；默认跳过，不在普通单测中消费模型额度。
func TestLivePersonalityEvolution(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_PERSONALITY") != "1" {
		t.Skip("显式开启人格连续场景验收")
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.APIKey == "" {
		t.Fatal("真实模型配置不可用", err)
	}
	dir := os.Getenv("DROPBY_PERSONALITY_DATA_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.WeeklyBudgetMicros = min(cfg.WeeklyBudgetMicros, 3_000_000)
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 4 * time.Minute}}, cfg)
	h := NewHandler(e)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	runID := newID("personality_eval")
	stages := []map[string]any{}
	before, _ := s.Usage()
	// 失败也保存已发生的行为与费用，便于区分没有更新、模型故障和实现错误。
	defer func() {
		after, _ := s.Usage()
		report := map[string]any{"run_id": runID, "model": cfg.Model, "chat_model": cfg.ChatModel, "stages": stages, "usage_before": before, "usage_after": after}
		raw, _ := json.MarshalIndent(report, "", "  ")
		path := filepath.Join(dir, runID+".json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Error(err)
		}
		t.Log("连续场景记录：", path)
	}()
	create := func(name, direction string) Member {
		t.Helper()
		response := preferenceRequest(h, "POST", "/api/members", map[string]any{"name": name, "direction": direction})
		if response.Code != 201 {
			t.Fatal("创建失败", response.Code, response.Body.String())
		}
		var member Member
		if err := json.Unmarshal(response.Body.Bytes(), &member); err != nil {
			t.Fatal(err)
		}
		stages = append(stages, map[string]any{"scenario": "创建", "direction": direction, "member": member})
		return member
	}
	nameOnly := create("小禾", "")
	if nameOnly.Profile == "" {
		t.Fatal("只填名字没有产生特质")
	}
	m := create("阿澈", "喜欢简洁设计的设计师，倾向先自己尝试，有一点干幽默，也会被有趣的新事物吸引。")
	c, err := s.CreateConversation("人格连续体验", "direct", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureWelcome(c); err != nil {
		t.Fatal(err)
	}
	// 每轮沿用生产成员行动与工作窗口，不替模型写回复或伪造工具成功。
	talk := func(label, body string) Member {
		t.Helper()
		c, err = s.Conversation(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		var beforeSeq int64
		if err := s.db.QueryRow(`SELECT coalesce(max(seq),0) FROM messages WHERE conversation_id=?`, c.ID).Scan(&beforeSeq); err != nil {
			t.Fatal(err)
		}
		message := userMessage(c, body)
		if err := s.AddUserMessage(message, time.Hour); err != nil {
			t.Fatal(err)
		}
		w := latestChatWork(t, s, c.ID)
		if ok, err := s.Claim(w.ID); err != nil || !ok {
			t.Fatal("无法开始场景", err)
		}
		start := time.Now()
		_, actionErr := e.memberAction(ctx, c, m, w)
		status, reason := "done", ""
		if actionErr != nil {
			status, reason = "failed", actionErr.Error()
		}
		if err := s.Finish(w, status, reason, time.Now()); err != nil {
			t.Fatal(err)
		}
		messages, _ := s.messagesAfter(c.ID, beforeSeq, 0, -1)
		current, _ := s.Member(m.ID)
		runs, _ := s.ToolRuns(c.ID, w.ID)
		ownRuns := []ToolRun{}
		for _, run := range runs {
			if run.WorkID == w.ID {
				ownRuns = append(ownRuns, run)
			}
		}
		stages = append(stages, map[string]any{"scenario": label, "prompt": body, "duration": time.Since(start).String(), "messages": messages, "profile": current.Profile, "profile_revision": current.ProfileRevision, "tools": ownRuns, "error": reason})
		t.Log(label, "完成", time.Since(start).Round(time.Second))
		if actionErr != nil {
			t.Fatal(label, actionErr)
		}
		return current
	}
	talk("职业之外聊猫", "猫又把我的杯子碰倒了，还一脸是杯子自己想不开的表情。")
	beforeTemporary, _ := s.Member(m.ID)
	afterTemporary := talk("一次要求", "这次说短点：我今晚煮面还是煎蛋？")
	if afterTemporary.Profile != beforeTemporary.Profile {
		t.Error("一次说短点改写了人物特质")
	}
	afterDislike := talk("审美不同", "我不喜欢极简设计，觉得空得没意思。你自己怎么看？")
	if afterDislike.Profile != afterTemporary.Profile {
		t.Error("普通审美反馈直接改写了人物")
	}
	afterEdit := talk("明确改特质", "我是在调整你这个角色本身：把慢热、等人开口的部分改得更外向、愿意主动开话题；保留你自己的审美和其他特点。请把特质资料也一起改好。")
	if afterEdit.Profile == afterDislike.Profile {
		t.Error("明确修改没有进入人物说明")
	}
	// 情景回答的例子故意没有说明原因，验证不会把品牌或产品类别固化成兴趣。
	response := preferenceRequest(h, "POST", "/api/members/"+m.ID+"/preferences", map[string]any{
		"question": Question{Text: "你希望我所说的有趣更像哪一种？可以举例，也可以先保持宽泛。", Options: []string{"奇特的日常发现", "新的玩法", "暂时说不清"}},
		"answer":   "比如 Grok bot 这样的吧，但我也不清楚究竟有趣在哪里，先保持有趣这个宽泛描述就好。"})
	if response.Code != 200 {
		t.Fatal("情景回答失败", response.Body.String())
	}
	ambiguous, _ := s.Member(m.ID)
	stages = append(stages, map[string]any{"scenario": "不明确的例子", "profile": ambiguous.Profile})
	if strings.Contains(strings.ToLower(ambiguous.Profile), "grok") {
		t.Error("举例被固化成产品兴趣")
	}
	// 实际读取产品材料并实际保存作品，后台只能读取这些真实动作与结果。
	talk("真实探索与行动", "我想做个大家愿意接着玩的共享画布。你自己看看 https://docs.excalidraw.com 和 https://tldraw.dev ，找一个你想尝试的点，做一个能在浏览器里点着玩的单人演示，保存成 HTML 作品。界面简单些，但玩法和取舍你来决定。做完聊聊你的发现。")
	beforeReflection, _ := s.Member(m.ID)
	if err := s.scheduleProfileReflections(time.Hour); err != nil {
		t.Fatal(err)
	}
	works, err := s.DueWorks()
	if err != nil {
		t.Fatal(err)
	}
	var reflected bool
	for _, w := range works {
		if w.Kind != "reflect" || w.MemberID != m.ID {
			continue
		}
		reflected = true
		if ok, err := s.Claim(w.ID); err != nil || !ok {
			t.Fatal("无法开始后台整理", err)
		}
		if err := e.process(ctx, w); err != nil {
			t.Fatal("后台整理", err)
		}
	}
	if !reflected {
		t.Fatal("真实行动后没有后台机会")
	}
	afterReflection, _ := s.Member(m.ID)
	changes, _ := s.profileHistory(m.ID)
	stages = append(stages, map[string]any{"scenario": "后台自我整理", "before": beforeReflection.Profile, "after": afterReflection.Profile, "changes": changes})
	t.Log("后台变化：", beforeReflection.Profile != afterReflection.Profile)
	// 不告诉成员刚才更新了什么，观察新话题里的选择是否体现已有认识。
	talk("后续选择", "我们再做个朋友一起写故事的小玩具，你会先尝试什么？先聊想法，不用制作。")
	talk("换回日常", "先不聊作品了。我今天打游戏连输五把，猫还坐在旁边看。")
	// 另一位成员实际接触同一个新话题，各自读取自己的说明；不要求所有人附和。
	group, err := s.CreateConversation("相同话题不同成员", "group", []string{m.ID, nameOnly.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	source := userMessage(group, "周末如果只有一个下午可以自由玩，你现在想试点什么？有想法就聊。")
	if err := s.AddUserMessage(source, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, group.ID)
	for _, person := range []Member{m, nameOnly} {
		if _, err := e.memberAction(ctx, group, person, w); err != nil {
			t.Fatal(err)
		}
	}
	messages, _ := s.Messages(group.ID, 0, -1)
	stages = append(stages, map[string]any{"scenario": "不同成员与同一话题", "messages": messages})
}

// 将同一次真实更新的前后说明放入没有旧聊天、笔记和作品的新身份，比较同一
// 新问题。它用于排除“只是原聊天还在上下文里”的解释，不把两次生成当成统计结论。
func TestLivePersonalityTransfer(t *testing.T) {
	file := os.Getenv("DROPBY_PERSONALITY_TRANSFER_REPORT")
	if file == "" {
		t.Skip("显式提供已完成的真实人格场景记录")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Stages []struct {
			Scenario string `json:"scenario"`
			Before   string `json:"before"`
			After    string `json:"after"`
		} `json:"stages"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	var profiles []string
	for _, stage := range report.Stages {
		if stage.Scenario == "后台自我整理" {
			profiles = []string{stage.Before, stage.After}
		}
	}
	if len(profiles) != 2 || profiles[0] == profiles[1] {
		t.Fatal("需要一次实际发生的自我更新")
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.APIKey == "" {
		t.Fatal("真实模型不可用", err)
	}
	dir := filepath.Dir(file)
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.WeeklyBudgetMicros = min(cfg.WeeklyBudgetMicros, 3_000_000)
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 4 * time.Minute}}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	observed := []map[string]any{}
	before, _ := s.Usage()
	defer func() {
		after, _ := s.Usage()
		body, _ := json.MarshalIndent(map[string]any{"source_report": filepath.Base(file), "observed": observed, "usage_before": before, "usage_after": after}, "", "  ")
		path := filepath.Join(dir, newID("personality_transfer")+".json")
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Error(err)
		}
		t.Log("新场景对照：", path)
	}()
	for i, profile := range profiles {
		m := Member{ID: newID("transfer"), Name: "阿澈", Profile: profile, CreatedAt: now()}
		if err := s.AddMember(m); err != nil {
			t.Fatal(err)
		}
		c, err := s.CreateConversation("新场景对照", "direct", []string{m.ID}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		msg := userMessage(c, "我们想弄个邻居互相认识的小活动，你会先怎么做？先聊想法，不用制作或联网。")
		if err := s.AddUserMessage(msg, time.Hour); err != nil {
			t.Fatal(err)
		}
		w := latestChatWork(t, s, c.ID)
		if _, err := e.memberAction(ctx, c, m, w); err != nil {
			t.Fatal(err)
		}
		messages, _ := s.Messages(c.ID, 0, -1)
		observed = append(observed, map[string]any{"variant": i, "profile": profile, "messages": messages})
	}
}

func TestLivePersonalityScenarioAnswer(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_PERSONALITY_QUESTION") != "1" {
		t.Skip("显式开启真实情景卡片验收")
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.APIKey == "" {
		t.Fatal("真实模型不可用", err)
	}
	dir := os.Getenv("DROPBY_PERSONALITY_DATA_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.WeeklyBudgetMicros = min(cfg.WeeklyBudgetMicros, 3_000_000)
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 4 * time.Minute}}, cfg)
	m := Member{ID: newID("scenario"), Name: "小洛", Profile: "我喜欢有趣的新事物，通常自己先试一轮，也喜欢用一点干幽默交流。", CreatedAt: now()}
	if err := s.AddMember(m); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation("情景卡片验收", "direct", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(e)
	response := preferenceRequest(h, "POST", "/api/members/"+m.ID+"/question", map[string]any{"conversation_id": c.ID})
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	var question Message
	if err := json.Unmarshal(response.Body.Bytes(), &question); err != nil || question.Question == nil || len(question.Question.Options) == 0 {
		t.Fatal("没有实际可回答的问题", err)
	}
	answer := userMessage(c, question.Question.Options[0])
	answer.ReplyTo = question.ID
	if err := s.AddUserMessage(answer, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := e.memberAction(ctx, c, m, w); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Member(m.ID)
	messages, _ := s.Messages(c.ID, 0, -1)
	changes, _ := s.profileHistory(m.ID)
	usage, _ := s.Usage()
	raw, _ := json.MarshalIndent(map[string]any{"before": m.Profile, "after": current.Profile, "question": question, "answer": answer, "messages": messages, "changes": changes, "usage": usage}, "", "  ")
	path := filepath.Join(dir, newID("personality_question")+".json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("情景卡片记录：", path)
	if current.ProfileRevision == 0 {
		t.Error("这次新增的具体情景选择没有进入人物说明，需检查问题和实际回答")
	}
}
