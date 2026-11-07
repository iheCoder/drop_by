package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
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
	// 可选记录真实模型接力的请求正文，方便确认各职责和工具续接；不保存凭据。
	client := &http.Client{Timeout: 4 * time.Minute}
	var requests atomic.Int64
	trace := &selectionTransport{total: &requests}
	if os.Getenv("DROPBY_PACING_REPORT") != "" {
		client.Transport = trace
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: client}, cfg)
	defer func() {
		if path := os.Getenv("DROPBY_PACING_REPORT"); path != "" {
			messages, _ := s.Messages(c.ID, 0, 100)
			work, _ := s.Works(c.ID)
			artifacts, _ := s.Artifacts(c.ID)
			runs, _ := s.ToolRuns(c.ID, "")
			usage, _ := s.Usage()
			raw, _ := json.MarshalIndent(map[string]any{"messages": messages, "work": work, "artifacts": artifacts, "tools": runs, "usage": usage, "trace": trace.traces}, "", "  ")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Error(err)
			}
		}
	}()
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
		// 简短原理比较也可在后续聊天机会直接保存。用户要的是实际草稿，
		// 不能为了满足旧测试而强迫每份短稿都创建后台研究。
		artifacts, err := s.Artifacts(c.ID)
		if err != nil || len(artifacts) == 0 || len(messages) < 3 {
			t.Fatal("既没有后续研究，也没有实际保存并分享草稿")
		}
		var linked int
		err = s.db.QueryRow(`SELECT count(*) FROM messages msg JOIN json_each(msg.evidence) ev
 JOIN tool_runs tr ON tr.id=ev.value AND tr.status='succeeded' AND tr.name='save_artifact'
 JOIN artifacts a ON a.id=json_extract(tr.result,'$.artifact_id')
 WHERE msg.conversation_id=? AND a.conversation_id=?`, c.ID, c.ID).Scan(&linked)
		if err != nil || linked == 0 {
			t.Fatal("草稿没有通过成功的保存记录分享回群")
		}
		t.Log("实际在后续发言中保存并分享草稿，没有创建不必要的后台研究")
		return
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
