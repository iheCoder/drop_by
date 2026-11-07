package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveNaturalConversation(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_NATURAL") != "1" {
		t.Skip("显式开启真实群聊观察")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("DROPBY_NATURAL_DATA_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.WeeklyBudgetMicros = 1000000
	cfg.MaxOutputTokens, cfg.DeepMaxOutputTokens = 2000, 6000
	people := []Member{
		{ID: "natural_lin", Name: "林间", Profile: "说话不多，喜欢安静、风景和电影里的留白。有感触才接话，完全可以什么也不说。", Color: "moss", AvatarIcon: "sprout", CreatedAt: now()},
		{ID: "natural_tao", Name: "桃子", Profile: "喜欢开玩笑，也愿意顺着朋友的话胡思乱想。表达轻松，不喜欢会议式讨论和完整答题。", Color: "rose", AvatarIcon: "cat", CreatedAt: now()},
		{ID: "natural_yu", Name: "周屿", Profile: "对同一个问题常有另一种观察角度，但并不刻意反驳。喜欢生活中的小细节，有时只是简单认同。", Color: "blue", AvatarIcon: "owl", CreatedAt: now()},
	}
	ids := []string{}
	for _, m := range people {
		if err := s.AddMember(m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	c, err := s.CreateConversation("风与电影 · 真实对话观察", "group", ids, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 3 * time.Minute}}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	message := userMessage(c, "刚看完一部特别慢的电影，剧情没怎么动，窗帘倒是被风吹了十几分钟。我居然觉得挺舒服的，又怀疑自己是不是只是困了。")
	if err := e.Submit(message); err != nil {
		t.Fatal(err)
	}
	e.dispatch(ctx)
	e.wg.Wait()
	messages, _ := s.Messages(c.ID, 0, 100)
	work, _ := s.Works(c.ID)
	participation, _ := s.Participations(c.ID)
	usage, _ := s.Usage()
	report, _ := json.MarshalIndent(map[string]any{"messages": messages, "work": work, "participation": participation, "usage": usage}, "", "  ")
	os.WriteFile(filepath.Join(dir, "natural-chat-report.json"), report, 0600)
	t.Logf("真实群聊观察：%d 条公开消息（含真人），估算 $%.4f，%d 次调用", len(messages), float64(usage.CostMicros)/1e6, usage.Calls)
	for _, w := range work {
		if w.Status == "failed" || w.Status == "waiting_budget" || w.Status == "paused" {
			t.Fatalf("真实群聊没有完成: %s", w.Result)
		}
	}
	var assignments int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE assigned_by<>''`).Scan(&assignments)
	if assignments != 0 {
		t.Fatal("普通闲聊被变成组织分工")
	}
	// 不对引用率、每人消息数或必须全员开口设通过线。记录真实结果供人工读，
	// 这个测试只证明对话链没有被技术故障打断，不能替代长期体验判断。
}
