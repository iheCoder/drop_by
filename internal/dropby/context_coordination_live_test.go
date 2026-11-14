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

// 真实验收使用独立数据库和合成角色，不把测试委托或测试记忆写进用户的群。
// 这条检查的是模型实际选择工具并完成接力；确定性的越权和并发另有回归测试。
func TestLiveContextRecallAndCoordination(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_CONTEXT") != "1" {
		t.Skip("显式开启真实记忆与协调验收")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ModelReady() {
		t.Fatal("缺少模型配置")
	}
	dir := os.Getenv("DROPBY_CONTEXT_DATA_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.WeeklyBudgetMicros = 1_100_000
	cfg.setModelLimit(2000, 8000)
	members := []Member{
		{ID: "recall_lan", Name: "阿岚", Color: "blue", AvatarIcon: "owl", CreatedAt: now(), Tools: []string{}, Profile: "沉稳、具体，愿意帮朋友把点子落地。做协调者时先核对以前约好的边界，再挑合适的朋友做，不抢同伴创作。结果到了以后能简短接住，不催人、不重复派相同任务。"},
		{ID: "recall_ya", Name: "小芽", Color: "jade", AvatarIcon: "sprout", CreatedAt: now(), Tools: []string{"save_artifact"}, Profile: "有一点顽皮的创作者，喜欢做小巧的互动网页，文字很短。接到清楚委托就自己制作并用save_artifact保存；已有同类工作时不要重复安排，没必要参与时可以安静。"},
	}
	for _, m := range members {
		if _, err := s.Member(m.ID); err != nil {
			if err := s.AddMember(m); err != nil {
				t.Fatal(err)
			}
		}
	}
	c, err := s.CreateConversation("星灯工作间 · 记忆与协调验收", "group", []string{members[0].ID, members[1].ID}, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// 不通过设置接口预先任命。下面的真人话语必须产生当前事项的实际职责。
	// 旧约在近期窗口和记忆索引之外，必须真正调用发现工具才能拿到。
	old := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: "user", Body: "记一下我们的星灯卡约定：只用深蓝和暖白两种颜色；可以有切换一句短句的按钮，但不要做倒计时。", CreatedAt: now()}
	if err := s.Publish(old, c.Revision); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		body := fmt.Sprintf("另一件旧事的片段 %d：那天我们在聊纸风车。", i)
		if err := s.SystemMessage(c.ID, newID("history"), body); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 32; i++ {
		body := fmt.Sprintf("纸风车旧经历 %d，不是目前的工作。", i)
		if i == 0 {
			body = old.Body
		}
		if _, err := s.db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?)`, newID("memory"), members[0].ID, c.ID, body, old.ID, now()); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 3 * time.Minute}}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer func() {
		cancel()
		e.mu.Lock()
		for _, stop := range e.running {
			stop()
		}
		e.mu.Unlock()
		e.wg.Wait()
		messages, _ := s.Messages(c.ID, 0, 100)
		works, _ := s.Works(c.ID)
		artifacts, _ := s.Artifacts(c.ID)
		runs, _ := s.ToolRuns(c.ID, "")
		usage, _ := s.Usage()
		states, _ := s.MemberStates(c.ID)
		raw, _ := json.MarshalIndent(map[string]any{"room_id": c.ID, "messages": messages, "work": works, "artifacts": artifacts, "tool_runs": runs, "member_states": states, "usage": usage}, "", "  ")
		os.WriteFile(filepath.Join(dir, "context-coordination-report.json"), raw, 0600)
		t.Logf("验收实际估算 $%.4f / %d 次调用；房间 %s", float64(usage.CostMicros)/1e6, usage.Calls, c.ID)
	}()
	message := userMessage(c, "阿岚，还记得我们以前给星灯卡约好了什么吗？请先找回旧约定，再请小芽做一张符合约定的小巧 HTML 卡片，你帮忙协调。做完之后接一下结果即可，不需要问我更多问题，也不用联网。")
	message.Mentions = []string{members[0].ID}
	if err := e.Submit(message); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		e.dispatch(ctx)
		var recalled, assigned, reviewed, artifacts int
		s.db.QueryRow(`SELECT count(*) FROM tool_runs WHERE conversation_id=? AND member_id=? AND name IN ('search_memory','read_history') AND status='succeeded'`, c.ID, members[0].ID).Scan(&recalled)
		s.db.QueryRow(`SELECT count(*) FROM work WHERE conversation_id=? AND assigned_by=? AND member_id=? AND status='done'`, c.ID, members[0].ID, members[1].ID).Scan(&assigned)
		// 接住结果可以发生在自动复看，也可以发生在同伴公开分享后的正常交流。
		// 两条路径都必须真的 read_work 核对完成结果；不能用固定回调状态代替行为证据。
		s.db.QueryRow(`SELECT count(*) FROM tool_runs r JOIN work w ON w.id=json_extract(r.arguments,'$.id')
 WHERE r.conversation_id=? AND r.member_id=? AND r.name='read_work' AND r.status='succeeded'
 AND w.assigned_by=? AND w.member_id=? AND json_extract(r.result,'$.status')='done'`, c.ID, members[0].ID, members[0].ID, members[1].ID).Scan(&reviewed)
		s.db.QueryRow(`SELECT count(*) FROM artifacts WHERE conversation_id=? AND member_id=? AND format='html'`, c.ID, members[1].ID).Scan(&artifacts)
		var remaining, completed int
		s.db.QueryRow(`SELECT count(*) FROM work WHERE conversation_id=? AND status IN ('pending','running','waiting_budget')`, c.ID).Scan(&remaining)
		s.db.QueryRow(`SELECT count(*) FROM work WHERE conversation_id=? AND status='done'`, c.ID).Scan(&completed)
		// 不能在 read_work 刚返回时就取消仍在生成的收尾；等实际行动完整退出。
		if recalled > 0 && assigned > 0 && reviewed > 0 && artifacts > 0 && remaining == 0 {
			var instruction string
			s.db.QueryRow(`SELECT instruction FROM work WHERE conversation_id=? AND assigned_by=? AND member_id=? ORDER BY created_at LIMIT 1`, c.ID, members[0].ID, members[1].ID).Scan(&instruction)
			if !strings.Contains(instruction, "倒计时") || !strings.Contains(instruction, "暖白") {
				t.Fatalf("找回的具体约定未进入委托：%s", instruction)
			}
			var roles int
			s.db.QueryRow(`SELECT count(*) FROM responsibilities WHERE conversation_id=? AND member_id=? AND scope_id<>''`, c.ID, members[0].ID).Scan(&roles)
			if roles == 0 {
				t.Fatal("实际委托没有来自对话形成的临时职责")
			}
			t.Log("实际从真人对话形成临时职责、找回旧约、按约委托、同伴保存 HTML、负责人完成结果复看")
			return
		}
		if remaining == 0 && completed > 0 {
			t.Fatalf("真实行动已经结束但未形成完整委托接力: recalled=%d assigned=%d reviewed=%d artifacts=%d", recalled, assigned, reviewed, artifacts)
		}
		var stalled int
		s.db.QueryRow(`SELECT count(*) FROM work WHERE conversation_id=? AND status IN ('failed','waiting_budget','paused')`, c.ID).Scan(&stalled)
		e.mu.Lock()
		running := len(e.running)
		e.mu.Unlock()
		if stalled > 0 && running == 0 {
			t.Fatal("真实链路停住，详细记录保留在验收报告")
		}
		select {
		case <-ctx.Done():
		case <-time.After(400 * time.Millisecond):
		}
	}
	t.Fatal("真实链路未在验收时限内完成")
}
