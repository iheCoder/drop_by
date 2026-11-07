package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 连续验收复用生产的观察、行动、真实性核对和真实模型。仅从显式提供的文件
// 复制裴船/石粒的资料，所有消息、笔记、成果和消费账本写进隔离目录。
// 场景推进加速了已安排工作的等待，不声称模拟了数天的真实长期相处。
func TestLivePersonalExperienceContinuity(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_MEMORY") != "1" {
		t.Skip("显式开启个人经历连续场景验收")
	}
	cfg, err := LoadConfig()
	if err != nil || cfg.APIKey == "" {
		t.Fatalf("真实模型配置不可用：%v", err)
	}
	raw, err := os.ReadFile(os.Getenv("DROPBY_MEMORY_MEMBERS_FILE"))
	if err != nil {
		t.Fatal("需要显式提供隔离成员资料文件：", err)
	}
	var members []Member
	if err := json.Unmarshal(raw, &members); err != nil || len(members) != 2 {
		t.Fatal("资料文件应仅含裴船、石粒")
	}
	dir := os.Getenv("DROPBY_MEMORY_DATA_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// 同一验收目录的账本累积保留，失败重试也计费；不重置生产或验收消费。
	cfg.WeeklyBudgetMicros = min(cfg.WeeklyBudgetMicros, 3_000_000)
	cfg.MaxOutputTokens, cfg.DeepMaxOutputTokens = 2500, 7000
	runID := newID("memory_eval")
	// 可在同一份隔离经历上继续观察合作理解，不重复前面的付费场景。
	// 继续时保留原成员 ID 与消费账本，不能把新演员当成已经经历过的人。
	continuedFrom := os.Getenv("DROPBY_MEMORY_CONTINUE_RUN")
	feedbackRoom := ""
	feedback := ""
	if continuedFrom != "" {
		data, err := os.ReadFile(filepath.Join(dir, filepath.Base(continuedFrom)+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var previous struct {
			Members []Member `json:"members"`
			Stages  []struct {
				RoomID    string     `json:"room_id"`
				Duration  string     `json:"duration"`
				Artifacts []Artifact `json:"artifacts"`
			} `json:"stages"`
		}
		if err := json.Unmarshal(data, &previous); err != nil || len(previous.Members) != 2 || len(previous.Stages) < 4 {
			t.Fatal("继续验收需要前面四个完整场景")
		}
		members, feedbackRoom = previous.Members, previous.Stages[1].RoomID
		creators := []string{}
		for i, stage := range previous.Stages[:4] {
			if stage.Duration == "" {
				t.Fatal("不能把尚未完成的场景当作既有经历")
			}
			if i > 1 {
				continue
			}
			for _, artifact := range stage.Artifacts {
				if artifact.Format != "html" {
					continue
				}
				for _, member := range members {
					if member.ID == artifact.MemberID {
						creators = append(creators, member.Name)
						break
					}
				}
				break
			}
		}
		if len(creators) != 2 {
			t.Fatal("前两次没有可核对的真实成果作者")
		}
		feedback = fmt.Sprintf("前两次邀请卡的成品分别是%s、%s做的。我喜欢你们接住彼此做好的部分，不用固定谁只能写、谁只能做。下次还是这样自然配合就好。这次不用再做页面，也不用额外安排。", creators[0], creators[1])
	}
	ids := []string{}
	for i := range members {
		if members[i].Name != "裴船" && members[i].Name != "石粒" {
			t.Fatal("成员身份不符")
		}
		if continuedFrom == "" {
			members[i].ID = runID + fmt.Sprintf("_%d", i)
			members[i].Avatar = ""
			if err := s.AddMember(members[i]); err != nil {
				t.Fatal(err)
			}
		} else if _, err := s.Member(members[i].ID); err != nil {
			t.Fatal("原成员已不存在：", err)
		}
		ids = append(ids, members[i].ID)
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 4 * time.Minute}}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	before, _ := s.Usage()
	stages := []map[string]any{}
	saveReport := func() {
		usage, _ := s.Usage()
		payload := map[string]any{"run_id": runID, "continued_from": continuedFrom, "model": cfg.Model, "reasoning": cfg.Reasoning, "deep_reasoning": cfg.DeepReasoning,
			"members": members, "stages": stages, "usage": usage, "run_cost_micros": usage.CostMicros - before.CostMicros, "run_calls": usage.Calls - before.Calls}
		data, _ := json.MarshalIndent(payload, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, runID+".json"), data, 0600); err != nil {
			t.Error(err)
		}
	}
	defer func() {
		saveReport()
		usage, _ := s.Usage()
		t.Logf("累计账本 $%.6f / %d 次；本次 $%.6f / %d 次；报告 %s", float64(usage.CostMicros)/1e6, usage.Calls, float64(usage.CostMicros-before.CostMicros)/1e6, usage.Calls-before.Calls, filepath.Join(dir, runID+".json"))
	}()
	newRoom := func(name string) Conversation {
		room, err := s.CreateConversation(name, "group", ids, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return room
	}
	round := func(room Conversation, label, prompt string) []Artifact {
		t.Log("开始场景：", label)
		start := time.Now()
		msg := userMessage(room, prompt)
		if err := e.Submit(msg); err != nil {
			t.Fatal(err)
		}
		room, _ = s.Conversation(room.ID)
		entry := map[string]any{"stage": label, "room_id": room.ID, "prompt": prompt, "source_id": msg.ID}
		stages = append(stages, entry)
		incoming := map[string]any{}
		for _, member := range members {
			input, err := e.contextFor(room, member, Work{SourceID: msg.ID, Instruction: prompt})
			if err != nil {
				t.Fatal(err)
			}
			incoming[member.Name] = input["memory"]
		}
		entry["entry_recall"] = incoming
		saveReport()
		// 已安排的短研究立即推进，避免把测试耗时浪费在十秒的入队等待上。
		// 不自动运行全天探索，不改真实用户的队列，也不替模型安排成果或台词。
		for steps := 0; ; steps++ {
			works, err := s.Works(room.ID)
			if err != nil {
				t.Fatal(err)
			}
			var next Work
			for i := len(works) - 1; i >= 0; i-- {
				if works[i].Status == "pending" && works[i].DueAt <= time.Now().Add(time.Minute).UnixMilli() {
					next = works[i]
					break
				}
			}
			if next.ID == "" {
				break
			}
			if steps >= 10 {
				t.Fatal("场景产生过多重复工作，已保留记录")
			}
			claimed, err := s.Claim(next.ID)
			if err != nil || !claimed {
				t.Fatalf("不能接收工作：%v", err)
			}
			if err := e.process(ctx, next); err != nil {
				s.Finish(next, "failed", err.Error(), time.Now())
				entry["error"] = err.Error()
				entry["tool_runs"], _ = s.ToolRuns(room.ID, "")
				saveReport()
				t.Fatal(err)
			}
			if err := s.Finish(next, "done", "", time.Now()); err != nil {
				t.Fatal(err)
			}
			t.Logf("完成 %s，场景已运行 %s", next.Kind, time.Since(start).Round(time.Second))
		}
		entry["messages"], _ = s.Messages(room.ID, 0, 100)
		entry["work"], _ = s.Works(room.ID)
		entry["tool_runs"], _ = s.ToolRuns(room.ID, "")
		entry["participation"], _ = s.Participations(room.ID)
		entry["states"], _ = s.MemberStates(room.ID)
		notes := map[string][]Memory{}
		for _, member := range members {
			notes[member.Name], _ = s.Memories(member.ID, room.ID)
		}
		entry["memories"] = notes
		artifacts, _ := s.Artifacts(room.ID)
		entry["artifacts"] = artifacts
		entry["duration"] = time.Since(start).String()
		saveReport()
		return artifacts
	}
	assertHTML := func(artifacts []Artifact, required ...string) {
		for _, a := range artifacts {
			if a.Format != "html" {
				continue
			}
			ok := true
			for _, word := range required {
				ok = ok && strings.Contains(a.Content, word)
			}
			if ok {
				return
			}
		}
		t.Fatalf("实际 HTML 没有同时体现当前条件和记忆约定：%v", required)
	}
	if feedbackRoom != "" {
		room, err := s.Conversation(feedbackRoom)
		if err != nil {
			t.Fatal(err)
		}
		// 这是实验中的真人反馈，不替角色编写理解。它评价实际发生过的两次
		// 合作，不要求固定谁写文案、谁做页面；之后检查各人留下的不同视角。
		if os.Getenv("DROPBY_MEMORY_ACTION_ONLY") != "1" {
			round(room, "合作反馈与个人理解", feedback)
		}
		for _, member := range members {
			notes, err := s.relevantMemories(member.ID, room.ID, "同伴 合作 配合")
			if err != nil {
				t.Fatal(err)
			}
			if len(notes) == 0 {
				t.Fatalf("%s 没有召回合作相关理解", member.Name)
			}
			// 不把“人人都写一条对同伴的看法”设为通过条件。是否形成具体
			// 个人视角、是否只复述用户偏好，必须审阅内容，不能靠名字命中判定。
			t.Logf("%s 的合作理解需人工核对：%s", member.Name, asJSON(notes))
		}
		next := newRoom("桥边小聚 · 合作方式续接")
		artifacts := round(next, "带着合作经历再次行动", "做一张桥边小聚邀请卡，时间21:50，地点石桥旁。沿用后来商量好的邀请方式，给我一份小巧HTML。你们自己接着配合就好，不用联网或预约。")
		assertHTML(artifacts, "21:50", "石桥旁", "留个位")
		t.Log("合作连续场景完成；记忆如何影响实际分工和是否减少重复，另按消息与工具记录审阅。")
		return
	}
	first := newRoom("邀请小样 · 经历形成")
	artifacts := round(first, "共同制作与个人经历", "裴船、石粒，我们做一张周末散步邀请卡的小样，保留时间18:30、地点河边。裴船你照看邀请语气，石粒你把它做成小巧HTML。以后我们做这种朋友邀请页，我希望打开就看到内容，先不做动画，主按钮用‘一起去’，别写‘立即报名’。你们可以相互补充，不用每人重复一份方案。不用联网，也不用预约将来的跟进。")
	assertHTML(artifacts, "18:30", "一起去")
	for _, member := range members {
		notes, err := s.Memories(member.ID, "")
		if err != nil || len(notes) == 0 {
			t.Fatalf("%s 没有形成可携带的个人经历：%v", member.Name, err)
		}
	}
	second := newRoom("露台夜聊 · 相关召回")
	artifacts = round(second, "跨场景沿用并完成新任务", "换到这个群，想做一张露台夜聊的邀请卡，时间20:15、地点楼顶。按我们相处下来舒服的方式做，给我能直接看的小巧HTML，文字短一点。你们自己接着配合就好，不用联网或预约。")
	assertHTML(artifacts, "20:15", "一起去")
	prior := map[string]int{}
	for _, member := range members {
		notes, _ := s.Memories(member.ID, second.ID)
		for _, note := range notes {
			prior[note.ID] = note.Revision
		}
	}
	round(second, "纠正旧理解", "我上次把‘先不做动画’说得太绝对了。以后邀请卡可以有自己点击才出现的小动效，默认仍然直接看到内容。主按钮也改成‘留个位’，不再固定用‘一起去’。这次先改好理解，不用重做刚才的页面，也不用每个人都回应。")
	for _, member := range members {
		notes, _ := s.Memories(member.ID, second.ID)
		updated := false
		for _, note := range notes {
			if previous, ok := prior[note.ID]; ok && note.Revision > previous && strings.Contains(note.Body, "留个位") {
				updated = true
			}
		}
		if !updated {
			t.Fatalf("%s 未把纠正写回原笔记", member.Name)
		}
	}
	// 大量与邀请卡无关的较新档案不能把相关经历从本地检索候选中挤走。
	for _, member := range members {
		for i := 0; i < 100; i++ {
			if _, err := s.db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?)`, newID("memory"), member.ID, "", fmt.Sprintf("隔离检索干扰 %d：旧电影里的紫色星云与深海鲸鱼。", i), "", now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	third := newRoom("晚风野餐 · 纠正后的行动")
	artifacts = round(third, "在新场景实际使用修正", "再做一张晚风野餐邀请卡，带上21:10和南门草地，沿用我们后来商量好的邀请方式。给这张卡加一点能让我主动点一下的小惊喜。其它保持简洁，不用联网或预约。")
	assertHTML(artifacts, "21:10", "南门草地", "留个位")
	for _, member := range members {
		current, _ := s.Member(member.ID)
		if current.Profile != member.Profile {
			t.Fatalf("%s 的稳定设定被实验改写", member.Name)
		}
	}
	t.Log("真实连续场景完成：个人经历、跨群召回、原笔记纠正和新场景 HTML 行动均已形成；自然度仍需按实际对话审阅。")
}
