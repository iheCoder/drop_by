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

// 真实模型验收仅写独立数据库，角色设定从本地夹具读取，不预写谁先说或谁选哪边。
func TestLiveIndependentCoordination(t *testing.T) {
	dir := os.Getenv("DROPBY_INITIATIVE_LIVE_DIR")
	if dir == "" {
		t.Skip("显式开启独立收费验收")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(os.Getenv("DROPBY_INITIATIVE_MEMBERS"))
	if err != nil {
		t.Fatal(err)
	}
	var members []Member
	if err = json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	cfg.WeeklyBudgetMicros = 2_000_000
	cfg.TemporaryBudgetMicros = 0 // 仅限定本次测试支出，不改正式预算。
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var ids []string
	for _, m := range members {
		if err = s.AddMember(m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	c, err := s.CreateConversation("独立协调验收", "group", ids, cfg.ExploreInterval)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE personal_activity SET next_at=?`, time.Now().Add(time.Hour).UnixMilli()) // 本次只验收指定群的自然交流。
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 4 * time.Minute}}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	if err = e.Submit(userMessage(c, "@青雉，这次你来组织一个小辩论：AI 对程序员利大于弊，还是弊大于利？裴船和石粒各说自己的想法，再相互回应。爱情导师这次不用参与。")); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	// 等实际调度收束，不用固定发言条数截断角色。测试时间到只结束隔离实验。
	for ctx.Err() == nil {
		time.Sleep(200 * time.Millisecond)
		var active int
		s.db.QueryRow(`SELECT count(*) FROM work WHERE conversation_id=? AND status IN ('pending','running') AND due_at<=?`, c.ID, time.Now().UnixMilli()).Scan(&active)
		if active == 0 {
			break
		}
	}
	cancel()
	<-done
	messages, _ := s.Messages(c.ID, 0, -1)
	jobs, _ := s.Works(c.ID)
	runs, _ := s.ToolRuns(c.ID, "")
	usage, _ := s.Usage()
	room, _ := s.Conversation(c.ID)
	report := map[string]any{"messages": messages, "work": jobs, "tool_runs": runs, "usage": usage, "conversation": room}
	raw, _ = json.MarshalIndent(report, "", "  ")
	os.WriteFile(filepath.Join(dir, "report.json"), raw, 0600)
	t.Logf("report=%s cost=$%.6f", filepath.Join(dir, "report.json"), float64(usage.CostMicros)/1e6)
	counts := map[string]int{}
	for _, msg := range messages {
		counts[msg.AuthorID]++
	}
	for _, m := range members {
		t.Logf("%s messages=%d", m.Name, counts[m.ID])
		if (m.Name == "裴船" || m.Name == "石粒") && counts[m.ID] == 0 {
			t.Errorf("%s 未发言", m.Name)
		}
	}
	// 临时职责在讨论结束后可以正常关闭；验收查历史记录，不要求永久保留为活跃。
	var agreements int
	s.db.QueryRow(`SELECT count(*) FROM responsibilities WHERE conversation_id=?`, c.ID).Scan(&agreements)
	if agreements == 0 {
		t.Error("组织职责未实际保存")
	}
	t.Logf("recorded responsibilities=%d active=%d", agreements, len(room.Responsibilities))
	for _, job := range jobs {
		if job.Status == "failed" || job.Status == "waiting_budget" {
			t.Errorf("验收工作未完成：%s %s %s", job.ID, job.Status, job.Result)
		}
	}
}
