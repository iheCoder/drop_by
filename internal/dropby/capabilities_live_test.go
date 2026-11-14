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

// 验收真实模型能把问题、作品和自己的后续连起来。只在显式启用时消耗额度；
// 数据全部为合成内容并保存在隔离目录，不向正式群聊写入测试消息。
func TestLiveCreativeToolsAndFollowup(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_CAPABILITIES") != "1" {
		t.Skip("显式开启工具与主动后续验收")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ModelReady() {
		t.Fatal("缺少模型配置")
	}
	dir := os.Getenv("DROPBY_CAPABILITIES_DATA_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// 保留正式模型和 high 推理；这是一张小卡片，测试输出容量和账本单独有界。
	// 目录已有的用量不会清空，反复运行仍受同一上限约束。
	cfg.WeeklyBudgetMicros = 650_000
	cfg.setModelLimit(2000, 8000)
	m := Member{ID: "capability_amo", Name: "阿墨", Color: "blue", AvatarIcon: "moon", CreatedAt: now(), Tools: []string{"save_artifact"}, Profile: "喜欢把朋友的荒诞点子做成小巧的互动网页。说话简短，不催人回复；喜欢自己试出一个不同的结尾，可以决定不分享。会按明确要求使用选择题和保存实际作品。"}
	if _, err := s.Member(m.ID); err != nil {
		if err := s.AddMember(m); err != nil {
			t.Fatal(err)
		}
	}
	c, err := s.CreateConversation("反攻略工作间 · 工具验收", "group", []string{m.ID}, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 3 * time.Minute}}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	defer func() {
		cancel()
		e.mu.Lock()
		for _, stop := range e.running {
			stop()
		}
		e.mu.Unlock()
		e.wg.Wait()
	}()
	// 使用真实 dispatch 和到期队列，但不提供额外闲时机会，便于归因于本次请求。
	waitUntil := func(check func() bool) {
		t.Helper()
		for ctx.Err() == nil {
			e.dispatch(ctx)
			if check() {
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
		t.Fatal("真实验收到期：", ctx.Err())
	}
	defer func() {
		// 成功与失败都保留实际记录，不把预期写成验收结果。
		messages, _ := s.Messages(c.ID, 0, 100)
		works, _ := s.Works(c.ID)
		artifacts, _ := s.Artifacts(c.ID)
		usage, _ := s.Usage()
		data, _ := json.MarshalIndent(map[string]any{"room_id": c.ID, "messages": messages, "works": works, "artifacts": artifacts, "usage": usage}, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "capabilities-report.json"), data, 0600); err != nil {
			t.Error(err)
		}
		t.Logf("实际账本：%d 次调用，$%.4f；群 %s", usage.Calls, float64(usage.CostMicros)/1e6, c.ID)
	}()
	if err := e.Submit(userMessage(c, "阿墨，我们做一张反攻略小卡片。先用聊天里的选择题工具问我一个风格问题，给两个选项就好，现在先不制作。")); err != nil {
		t.Fatal(err)
	}
	waitUntil(func() bool {
		var n int
		s.db.QueryRow(`SELECT count(*) FROM message_questions q JOIN messages m ON m.id=q.message_id WHERE m.conversation_id=?`, c.ID).Scan(&n)
		return n > 0
	})
	t.Log("真实模型已发布可点击问题")
	if err := e.Submit(userMessage(c, "选荒诞又清爽的风格。请做一张完整的 HTML 小卡片，标题是《允许临阵脱逃》，有按钮可以切换三句反攻略。用 save_artifact 保存，不要联网。完成后用 schedule_followup 给自己安排一分钟后再看一眼：结合群里最新内容判断是否值得换个结尾，也可以保持安静，不用等我继续发话。")); err != nil {
		t.Fatal(err)
	}
	waitUntil(func() bool {
		var artifacts, finished int
		s.db.QueryRow(`SELECT count(*) FROM artifacts WHERE conversation_id=? AND format='html'`, c.ID).Scan(&artifacts)
		s.db.QueryRow(`SELECT count(*) FROM work WHERE conversation_id=? AND kind='revisit' AND status='done'`, c.ID).Scan(&finished)
		return artifacts > 0 && finished > 0
	})
	t.Log("真实模型已保存 HTML；本人没有新消息时，自己的后续重新判断已完成")
}
