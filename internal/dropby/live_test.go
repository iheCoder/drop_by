package dropby

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 显式开启才使用真实模型和额度。输入全为合成的非敏感验收文本，
// 用于观察发布核对能否识别一次实际暴露过的虚构身体经历；不是绝对防幻觉证明。
func TestLiveGroundingOfEmbodiedClaim(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_TEST") != "1" {
		t.Skip("设置 DROPBY_LIVE_TEST=1 后运行真实服务验收")
	}
	s, c, m := fixture(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey == "" {
		t.Fatal("缺少真实模型配置")
	}
	model := &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 75 * time.Second}}
	e := NewEngine(s, model, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	text, err := e.reviewText(ctx, c, m, Work{}, "公开消息", "我坐过钢管椅，手碰到金属觉得凉，这是我真实的亲身使用体验。布面的弹性可能影响舒适度。")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("真实服务核对结果：", text)
	if strings.Contains(text, "我坐过钢管椅") || strings.Contains(text, "这是我真实的亲身使用体验") {
		t.Fatal("虚构身体经历仍被放行")
	}
}

// 真实模型验收先发布简短回应，再用高推理完成已持久化任务；不依赖浏览器。
func TestLiveFirstReplyThenResearch(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_PACING") != "1" {
		t.Skip("显式开启真实节奏验收")
	}
	s, c, m := fixture(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 4 * time.Minute}}, cfg)
	msg := userMessage(c, "@阿苇 我想比较纸飞机为什么有的飞得远、有的飞得稳。你先说一句初步想法，然后认真推敲两种设计的取舍，保存一份短草稿；不需要联网，不要假装做过实体实验。")
	msg.Mentions = []string{m.ID}
	if err := e.Submit(msg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	start := time.Now()
	work, _ := s.DueWorks()
	if len(work) == 0 {
		t.Fatal("无用户工作")
	}
	if err := e.process(ctx, work[0]); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) < 2 {
		t.Fatal("没有发布初步回应")
	}
	t.Logf("首句耗时 %s: %s", time.Since(start), messages[1].Body)
	jobs, _ := s.Works(c.ID)
	var research Work
	for _, w := range jobs {
		if w.Kind == "research" {
			research = w
			break
		}
	}
	if research.ID == "" {
		t.Fatal("没有持久化后续研究")
	}
	if err := e.process(ctx, research); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM artifacts WHERE conversation_id=?`, c.ID).Scan(&n)
	if n == 0 {
		t.Fatal("没有保存实际草稿")
	}
	t.Logf("深入行动完成，累计耗时 %s，成果版本数 %d", time.Since(start), n)
	jobs, _ = s.Works(c.ID)
	for _, w := range jobs {
		if w.Kind == "share" {
			if err := e.process(ctx, w); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	messages, _ = s.Messages(c.ID, 0, 20)
	if len(messages) < 3 {
		t.Fatal("研究结果尚未分享回群")
	}
	t.Logf("后续分享完成，累计耗时 %s: %s", time.Since(start), messages[2].Body)
}
