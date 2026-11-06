package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 这项验收使用真正的生产模型、参与判断和发布路径；只把非敏感的开场设定写入
// 隔离数据目录。它记录真实对话供人审阅，不用“每个人必须说话”之类断言代替趣味。
// 默认跳过，避免普通 go test 消耗额度。保留目录时可以再用服务打开同一份聊天。
func TestLiveCharacterConversation(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_CHARACTERS") != "1" {
		t.Skip("显式开启真实角色群聊验收")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey == "" {
		t.Fatal("缺少真实模型配置")
	}
	dir := os.Getenv("DROPBY_CHARACTER_DATA_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// 单个验收最多使用这份独立账本中的 1.25 美元；保留生产选用的模型和推理强度。
	// 不通过降低模型档次制造“快速”，也不清空既有账本后反复绕过预算。
	if cfg.DailyBudgetMicros > 1_250_000 {
		cfg.DailyBudgetMicros = 1_250_000
	}
	cfg.MaxTurns = 4
	people := []Member{
		{ID: "eval_momo", Name: "桃子", Color: "rose", Profile: "温柔但不端着，敏感于话里没说完的感受，喜欢轻巧的比喻和接梗。不会把随口疲惫变成心理辅导，也不负责总结。对具体创意会认真接着改，已经有人安慰过就不再复述安慰。"},
		{ID: "eval_zhou", Name: "周屿", Color: "blue", Profile: "喜欢随兴发现和设计里有趣的取舍，善于把过于复杂的点子缩成一个小动作。有时嘴欠，会顺着朋友的玩笑再拐一下，不把聊天变成方案会。不为了活跃每条都答，别人说完自己的意思时可以不说。"},
		{ID: "eval_lin", Name: "林间", Color: "moss", Profile: "平时话少，喜欢荒诞短篇、语言游戏和反常识的意象。普通日程建议常常只看不回；遇到有意思的文字创作会主动接续别人的句子，甚至推翻自己之前的版本。没有固定的反对职责，也不用解释自己为何沉默。"},
	}
	for i := range people {
		people[i].CreatedAt = now()
		people[i].Tools = []string{"save_artifact"}
		if _, err := s.Member(people[i].ID); err == nil {
			continue
		}
		if err := s.AddMember(people[i]); err != nil {
			t.Fatal(err)
		}
	}
	room, err := s.CreateConversation("周末不赶路 · 体验验收", "group", []string{people[0].ID, people[1].ID, people[2].ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// 以下三条是明确的测试开场，并非声称真实模型曾经说过这些话。它们让参与判断
	// 面对多条、有不同说话人的语境，能选择更早的话，而不只盯着最后一个问题。
	seed := []Message{
		{ID: newID("msg"), AuthorID: "user", Body: "这周已经被排期榨干了。周末想出去走走，但不想再做攻略。"},
		{ID: newID("msg"), AuthorID: people[0].ID, Body: "那先别把休息也安排成一门绩效课，能发呆的地方就挺好。"},
		{ID: newID("msg"), AuthorID: "user", Body: "对，而且我说想出去，也不一定非得去景点。"},
	}
	for i := range seed {
		seed[i].ConversationID = room.ID
		seed[i].CreatedAt = now()
		if err := s.Guard(room.ID, room.Revision, func(tx *sql.Tx) error { return insertMessage(tx, seed[i]) }); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 4 * time.Minute}}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	type observedRound struct {
		Prompt        string          `json:"prompt"`
		Duration      string          `json:"duration"`
		Messages      []Message       `json:"messages"`
		Participation []Participation `json:"participation"`
		MemberStates  any             `json:"member_states"`
	}
	observed := []observedRound{}
	for _, prompt := range []string{
		"要是这趟出门也要写攻略，我可能会临阵脱逃。谁有想法就接着聊吧，不用都回答我，也不用联网。",
		"行，不要继续给我安慰或安排了。我们一起给这次出门写一个荒诞的‘反攻略’标题吧，你们也可以改对方的梗，不用做完整方案。",
	} {
		start := time.Now()
		if err := e.Submit(userMessage(room, prompt)); err != nil {
			t.Fatal(err)
		}
		jobs, err := s.Works(room.ID)
		if err != nil {
			t.Fatal(err)
		}
		var work Work
		for _, job := range jobs {
			if job.Kind == "chat" && job.Status == "pending" {
				work = job
				break
			}
		}
		if work.ID == "" {
			t.Fatal("真人消息没有生成可执行机会")
		}
		if claimed, err := s.Claim(work.ID); err != nil || !claimed {
			t.Fatalf("不能接收机会: claimed=%v err=%v", claimed, err)
		}
		if err := e.process(ctx, work); err != nil {
			s.Finish(work, "failed", err.Error(), time.Now())
			t.Fatal(err)
		}
		if err := s.Finish(work, "done", "", time.Now()); err != nil {
			t.Fatal(err)
		}
		messages, err := s.Messages(room.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		decisions, err := s.Participations(room.ID)
		if err != nil {
			t.Fatal(err)
		}
		states, err := s.MemberStates(room.ID)
		if err != nil {
			t.Fatal(err)
		}
		observed = append(observed, observedRound{prompt, time.Since(start).String(), messages, decisions, states})
		t.Logf("完成一轮真实群聊，耗时 %s；累计 %d 条消息", time.Since(start).Round(time.Second), len(messages))
		// 每轮立即落盘，后续额度或网络中断也不会让已经观察到的证据消失。
		report := map[string]any{"room_id": room.ID, "seed_message_ids": []string{seed[0].ID, seed[1].ID, seed[2].ID}, "members": people, "rounds": observed}
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "character-conversation.json"), encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("真实角色验收账本：%d 次调用，估算 $%.4f", usage.Calls, float64(usage.CostMicros)/1e6)
}
