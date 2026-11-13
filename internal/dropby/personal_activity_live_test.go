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

// 显式开启的收费验收。人物资料从指定夹具读取，所有聊天、笔记和用量均在独立库。
// 不安排待办、不告诉成员选哪个题目，也不把实验的目标写进角色提示词。
func TestLivePersonalExploration(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_PERSONAL_ACTIVITY") != "1" {
		t.Skip("需要显式开启真实个人探索验收")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("DROPBY_PERSONAL_TEST_DIR")
	if dir == "" || cfg.APIKey == "" || cfg.SearchKey == "" {
		t.Fatal("需要独立测试目录及 API 配置")
	}
	if _, err := os.Stat(filepath.Join(dir, "dropby.sqlite")); err == nil {
		t.Fatal("不复用旧测试数据")
	}
	raw, err := os.ReadFile(os.Getenv("DROPBY_PERSONAL_TEST_PEOPLE"))
	if err != nil {
		t.Fatal(err)
	}
	var people []Member
	if err = json.Unmarshal(raw, &people); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.DataDir = dir
	// 这是一次验收开销边界，不改变产品的总预算或给角色增加行动配额。
	cfg.WeeklyBudgetMicros = 1_500_000
	cfg.TemporaryBudgetMicros = 0
	ids := []string{}
	for _, m := range people {
		if err := s.AddMember(m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	group, err := s.CreateConversation("一起随便聊聊", "group", ids, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 4 * time.Minute}}, cfg)
	// 给每个人一段确实发生过的实验内私聊，建立对真人的了解。它不是当前任务。
	subjects := []string{"user"}
	for _, m := range people {
		dm, err := s.CreateConversation(m.Name, "direct", []string{m.ID}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		past := Message{ID: newID("msg"), ConversationID: dm.ID, AuthorID: "user", Body: "我喜欢新技术和生活里的小发现，也喜欢能亲手点一点的小作品。别让我一上来填长问卷。", CreatedAt: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)}
		if err = s.Publish(past, dm.Revision); err != nil {
			t.Fatal(err)
		}
		_, err = e.Tools.remember(context.Background(), dm, m, Work{SourceID: past.ID}, memoryWrite{Body: "用户喜欢新技术、生活里的小发现和可亲手玩的作品；不喜欢开场就填长问卷。", SubjectIDs: &subjects, SourceIDs: []string{past.ID}})
		if err != nil {
			t.Fatal(err)
		}
	}
	report := map[string]any{"group_id": group.ID, "people": people, "seed": "只有过去的相处偏好，没有当前任务；群聊没有消息。"}
	defer func() {
		rooms, _ := s.Conversations()
		allMessages := []Message{}
		for _, room := range rooms {
			messages, _ := s.Messages(room.ID, 0, 100)
			allMessages = append(allMessages, messages...)
		}
		jobs, _ := s.personalWorks(ids)
		runs, _ := s.ToolRuns("", "")
		usage, _ := s.Usage()
		focus, _ := s.personalFocuses()
		report["messages"], report["personal_work"], report["personal_tools"], report["usage"], report["personal_focus"] = allMessages, jobs, runs, usage, focus
		raw, _ := json.MarshalIndent(report, "", "  ")
		os.WriteFile(filepath.Join(dir, "report.json"), raw, 0600)
		t.Logf("验收记录 %s；累计模型与工具费用 $%.4f", filepath.Join(dir, "report.json"), float64(usage.CostMicros)/1e6)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err = s.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	e.dispatch(ctx)
	e.wg.Wait()
	// 分享产生的群内消息有正常接话机会；只处理已经到期的真实工作，不快进预约。
	for {
		due, err := s.DueWorks()
		if err != nil {
			t.Fatal(err)
		}
		if len(due) == 0 || ctx.Err() != nil {
			break
		}
		e.dispatch(ctx)
		e.wg.Wait()
	}
	var explored, shared, failed int
	s.db.QueryRow(`SELECT count(*) FROM tool_runs WHERE conversation_id='' AND name IN ('search','read_page') AND status='succeeded'`).Scan(&explored)
	s.db.QueryRow(`SELECT count(*) FROM messages WHERE author_id NOT IN ('user','system')`).Scan(&shared)
	s.db.QueryRow(`SELECT count(*) FROM work WHERE status='failed'`).Scan(&failed)
	report["observed_external_reads"], report["observed_messages"], report["failed_work"] = explored, shared, failed
	if explored == 0 || shared == 0 || failed > 0 || ctx.Err() != nil {
		t.Errorf("真实场景仍有缺口: 外部行动=%d 消息=%d 失败=%d context=%v", explored, shared, failed, ctx.Err())
	}
}

// 在同一实验人物已有经历上继续真实群聊；不重新创建角色，也不指定发言顺序。
// 记录完整实际结果供阅读，消息数量不作为“像真人”的代理指标。
func TestLivePersonalConversationContinuation(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_PERSONAL_CONTINUATION") != "1" {
		t.Skip("需要显式开启连续群聊验收")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("DROPBY_PERSONAL_TEST_DIR")
	if dir == "" {
		t.Fatal("需要已有的隔离实验目录")
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.WeeklyBudgetMicros = 1_500_000
	cfg.TemporaryBudgetMicros = 0
	people, err := s.Members()
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, m := range people {
		ids = append(ids, m.ID)
	}
	group, err := s.CreateConversation("相遇·连续场景", "group", ids, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 4 * time.Minute}}, cfg)
	report := map[string]any{"group_id": group.ID}
	defer func() {
		messages, _ := s.Messages(group.ID, 0, 100)
		work, _ := s.Works(group.ID)
		runs, _ := s.ToolRuns(group.ID, "")
		participation, _ := s.Participations(group.ID)
		focus, _ := s.personalFocuses()
		usage, _ := s.Usage()
		report["messages"], report["work"], report["tools"], report["participation"], report["personal_focus"], report["usage"] = messages, work, runs, participation, focus, usage
		raw, _ := json.MarshalIndent(report, "", "  ")
		os.WriteFile(filepath.Join(dir, "continuation.json"), raw, 0600)
		t.Logf("连续场景记录 %s；实验累计费用 $%.4f", filepath.Join(dir, "continuation.json"), float64(usage.CostMicros)/1e6)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err = e.Submit(userMessage(group, "刚才一个给雨换屋顶，一个让灰块变亮，我有个想法：如果把声音和视觉的错觉放在一起，会发生什么？")); err != nil {
		t.Fatal(err)
	}
	for {
		works, err := s.DueWorks()
		if err != nil {
			t.Fatal(err)
		}
		if len(works) == 0 || ctx.Err() != nil {
			break
		}
		e.dispatch(ctx)
		e.wg.Wait()
	}
	var count, failed int
	s.db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id=? AND author_id<>'user'`, group.ID).Scan(&count)
	s.db.QueryRow(`SELECT count(*) FROM work WHERE conversation_id=? AND status='failed'`, group.ID).Scan(&failed)
	if count == 0 || failed > 0 || ctx.Err() != nil {
		t.Errorf("群聊仍有执行缺口: messages=%d failed=%d context=%v", count, failed, ctx.Err())
	}
}
