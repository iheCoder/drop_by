package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type modelFunc func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error)

func (f modelFunc) Complete(c context.Context, m []ChatMessage, d []ToolDefinition, j bool) (ChatMessage, error) {
	return f(c, m, d, j)
}

func jsonReply(v any) ChatMessage { return ChatMessage{Role: "assistant", Content: asJSON(v)} }
func testConfig() Config {
	return Config{Address: "127.0.0.1:8090", APIKey: "test-only", Model: "gpt-6.1-terra", BaseURL: "http://localhost", SearchKey: "test-only", ExploreInterval: time.Hour, MaxOutputTokens: 400, WeeklyBudgetMicros: 1_000_000, ToolCostMicros: 750}
}
func fixture(t *testing.T) (*Store, Conversation, Member) {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	m := Member{ID: "member_wei", Name: "阿苇", Profile: "喜欢寻找原始材料，也愿意改变判断。", Tools: []string{"search", "read_page", "save_artifact"}, CreatedAt: now()}
	if err = s.AddMember(m); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation("测试群", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, m
}
func addWork(t *testing.T, s *Store, c Conversation, m Member, kind string) Work {
	t.Helper()
	w := Work{ID: newID("work"), ConversationID: c.ID, MemberID: m.ID, Kind: kind, Instruction: "查找那个蓝色自行车的设计来源", Reason: "共同好奇", Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
	if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error { return insertWork(tx, w) }); err != nil {
		t.Fatal(err)
	}
	return w
}
func userMessage(c Conversation, body string) Message {
	return Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: "user", Body: body, CreatedAt: now()}
}
func toolCall(name string, args any) ToolCall {
	var call ToolCall
	call.ID = newID("call")
	call.Type = "function"
	call.Function.Name = name
	call.Function.Arguments = asJSON(args)
	return call
}

func TestUserInterruptionRejectsStaleReplyAndSideEffects(t *testing.T) {
	// 场景：成员基于旧消息完成生成时，用户已经明确停止研究。
	// 不依赖网络取消成功；事务里的修订检查必须挡住旧回复与旧成果。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	if err := s.AddUserMessage(userMessage(c, "停止研究"), time.Hour); err != nil {
		t.Fatal(err)
	}
	stale := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: m.ID, Body: "仍然按蓝色继续", CreatedAt: now()}
	if err := s.Publish(stale, c.Revision); !errors.Is(err, ErrStale) {
		t.Fatalf("过时回复未被拦截: %v", err)
	}
	toolbox := Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	_, err := toolbox.Execute(context.Background(), c, m, w, toolCall("save_artifact", map[string]string{"title": "旧方向", "content": "过时结果"}))
	if !errors.Is(err, ErrStale) {
		t.Fatalf("过时成果未被拦截: %v", err)
	}
	artifacts, err := s.Artifacts(c.ID)
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("不应保存旧成果: %v %v", artifacts, err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) != 1 || messages[0].AuthorID != "user" {
		t.Fatalf("只应保留新用户消息: %+v", messages)
	}
}

func TestExplicitStopsWorkWithoutModelOrBudget(t *testing.T) {
	// 即使模型断线或预算耗尽，“先别聊”也必须当场生效，保留研究；
	// 随后的“不用研究”必须取消那项已经安排的研究，而不是等下一次模型调用。
	s, c, m := fixture(t)
	work := addWork(t, s, c, m, "research")
	if err := s.AddUserMessage(userMessage(c, "先别聊"), time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	if !c.DiscussionPaused || c.ResearchPaused {
		t.Fatalf("停止讨论误伤研究: %+v", c)
	}
	jobs, _ := s.Works(c.ID)
	for _, job := range jobs {
		if job.ID == work.ID && job.Status != "pending" {
			t.Fatalf("研究不应停止: %+v", job)
		}
	}
	if err := s.AddUserMessage(userMessage(c, "不用研究了。"), time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	if !c.ResearchPaused {
		t.Fatal("研究没有立即暂停")
	}
	jobs, _ = s.Works(c.ID)
	for _, job := range jobs {
		if job.ID == work.ID && job.Status != "stopped" {
			t.Fatalf("研究应停止: %+v", job)
		}
	}
}

func TestScopedCorrectionChangesOnlyRelatedWork(t *testing.T) {
	// 用户纠正自行车颜色，并停止另一项研究。模拟的解释结果只替代模型这一边界，
	// 真正检查数据库更新是否局部、执行时是否能读到新方向。
	s, c, m := fixture(t)
	first := addWork(t, s, c, m, "research")
	second := addWork(t, s, c, m, "research")
	model := modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		return jsonReply(Control{Discussion: "resume", Research: "keep", Guidance: "自行车应按红色查找", StopIDs: []string{second.ID}, Updates: []struct {
			ID          string `json:"id"`
			Instruction string `json:"instruction"`
		}{{ID: first.ID, Instruction: "查找红色自行车的设计来源"}}}), nil
	})
	e := NewEngine(s, model, testConfig())
	w := Work{Instruction: "其实是红色，另一件不用找了", SourceID: "correction"}
	var control Control
	response, _ := e.Model.Complete(context.Background(), nil, nil, true)
	if err := parseJSON(response.Content, &control); err != nil {
		t.Fatal(err)
	}
	if err := e.applyControl(context.Background(), c, w, control); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Works(c.ID)
	for _, job := range jobs {
		if job.ID == first.ID && (job.Instruction != "查找红色自行车的设计来源" || job.Status != "pending") {
			t.Fatalf("相关方向未更新: %+v", job)
		}
		if job.ID == second.ID && job.Status != "stopped" {
			t.Fatalf("指定研究未停止: %+v", job)
		}
	}
}

func TestBudgetReservationsAreAtomicAndSurviveRestart(t *testing.T) {
	// 20 个并发请求争用仅够 3 次的预算。请求前预留必须使最多 3 次获准，
	// 关闭再打开数据库后也不能得到一个“全新”的周预算。
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	c.WeeklyBudgetMicros = 300
	var granted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Reserve(c, 100); err == nil {
				granted.Add(1)
			} else if !errors.Is(err, ErrBudget) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 3 {
		t.Fatalf("预算放行次数=%d，预期 3", granted.Load())
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Reserve(c, 1); !errors.Is(err, ErrBudget) {
		t.Fatalf("重启不应恢复预算: %v", err)
	}
	u, _ := s.Usage()
	if u.CostMicros != 300 || u.Calls != 3 {
		t.Fatalf("账本不一致: %+v", u)
	}
}

func TestInterruptedResearchResumesAndCompletedResultPersists(t *testing.T) {
	// 模拟退出时有一项 running 研究。重新打开后应 pending，而已完成结果保持完成。
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := Member{ID: "member_a", Name: "一位朋友"}
	s.AddMember(m)
	c, err := s.CreateConversation("群", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w := addWork(t, s, c, m, "research")
	done := addWork(t, s, c, m, "research")
	s.Claim(w.ID)
	s.Claim(done.ID)
	if err = s.Finish(done, "done", "已保存真实结果", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	jobs, _ := s.Works(c.ID)
	for _, job := range jobs {
		if job.ID == w.ID && job.Status != "pending" {
			t.Fatalf("中断工作未恢复: %+v", job)
		}
		if job.ID == done.ID && (job.Status != "done" || job.Result != "已保存真实结果") {
			t.Fatalf("已完成工作被破坏: %+v", job)
		}
	}
}

func TestNoMemberNeedsToSpeakAndRunLimitIsNotPermanent(t *testing.T) {
	// 全部成员选择 pass 时不能合成占位回复；这次机会结束后，真实后续事件仍可运行。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "share")
	s.Claim(w.ID)
	var calls atomic.Int32
	model := modelFunc(func(_ context.Context, msgs []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls.Add(1)
		return jsonReply(Outcome{}), nil
	})
	e := NewEngine(s, model, testConfig())
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	s.Finish(w, "done", "", time.Now())
	messages, _ := s.Messages(c.ID, 0, 20)
	if len(messages) != 0 {
		t.Fatal("pass 不应生成消息")
	}
	// 后续事件改变判断。这一次只发一条，然后再次选择安静。
	model = modelFunc(func(_ context.Context, msgs []ChatMessage, _ []ToolDefinition, jsonOnly bool) (ChatMessage, error) {
		return jsonReply(Outcome{Message: "新材料让我改变了看法。"}), nil
	})
	e.Model = model
	second := addWork(t, s, c, m, "share")
	s.Claim(second.ID)
	if err := e.process(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	messages, _ = s.Messages(c.ID, 0, 20)
	if len(messages) != 1 {
		t.Fatalf("真实后续应重新引发一次交流: %+v", messages)
	}
}

func TestResearchCompletionDoesNotRequirePublicDiscussion(t *testing.T) {
	// 用户暂停聊天时后台研究仍可保存完成结果；不再另开一次分享审批。
	s, c, m := fixture(t)
	s.db.Exec(`UPDATE conversations SET discussion_paused=1 WHERE id=?`, c.ID)
	c, _ = s.Conversation(c.ID)
	w := addWork(t, s, c, m, "research")
	s.Claim(w.ID)
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		return jsonReply(Outcome{Outcome: "这轮没有找到可靠结果，没有编造候选。"}), nil
	}), testConfig())
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(w, "done", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Works(c.ID)
	foundResult, foundShare := false, false
	for _, job := range jobs {
		if job.ID == w.ID {
			foundResult = job.Status == "done" && strings.Contains(job.Result, "没有找到")
		}
		if job.Kind == "share" {
			foundShare = true
		}
	}
	if !foundResult || foundShare {
		t.Fatalf("研究结果/分享机会丢失: %+v", jobs)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) > 0 {
		t.Fatal("不应绕过暂停公开发言")
	}
}

func TestToolFailureHasRealRecordAndCannotBecomeSuccess(t *testing.T) {
	// 模拟搜索服务返回 503。客户端和成员拿到的必须是失败记录，不能有伪造资料。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer upstream.Close()
	cfg := testConfig()
	cfg.SearchURL = upstream.URL
	tbox := Toolbox{Store: s, Config: cfg, Hub: NewHub()}
	result, err := tbox.Execute(context.Background(), c, m, w, toolCall("search", map[string]string{"query": "纸自行车"}))
	if err != nil || !strings.Contains(result, `"status":"failed"`) {
		t.Fatalf("工具失败应交回给本人: %s %v", result, err)
	}
	runs, _ := s.ToolRuns(c.ID, w.ID)
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("没有真实失败记录: %+v", runs)
	}
}

func TestArtifactsVersionAndMemoryScope(t *testing.T) {
	// 成果必须确实保存、修改产生新版本。跨群仅携带该成员的可携带笔记，
	// 当前群的局部笔记和另一个成员的笔记不应出现在第二个群的上下文。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	tbox := Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	for _, scope := range []string{"conversation", "member"} {
		_, err := tbox.Execute(context.Background(), c, m, w, toolCall("remember", map[string]string{"scope": scope, "body": "来自 " + scope + " 的非敏感测试经历"}))
		if err != nil {
			t.Fatal(err)
		}
	}
	c2, err := s.CreateConversation("另一个群", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	notes, _ := s.Memories(m.ID, c2.ID)
	if len(notes) != 1 || notes[0].ConversationID != "" {
		t.Fatalf("记忆可见范围不正确: %+v", notes)
	}
	_, err = tbox.Execute(context.Background(), c, m, w, toolCall("save_artifact", map[string]string{"title": "草稿", "content": "第一版"}))
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.Artifacts(c.ID)
	if len(list) != 1 {
		t.Fatal("成果没有保存")
	}
	_, err = tbox.Execute(context.Background(), c, m, w, toolCall("save_artifact", map[string]string{"id": list[0].ID, "title": "修改后的草稿", "content": "第二版"}))
	if err != nil {
		t.Fatal(err)
	}
	versions, _ := s.ArtifactVersions(list[0].ID)
	if len(versions) != 2 || versions[0].Content != "第二版" || versions[1].Content != "第一版" {
		t.Fatalf("历史版本不正确: %+v", versions)
	}
	foreign := addWork(t, s, c2, m, "research")
	result, err := tbox.Execute(context.Background(), c2, m, foreign, toolCall("save_artifact", map[string]string{"id": list[0].ID, "title": "越界修改", "content": "不允许"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "只能修改当前聊天") {
		t.Fatalf("跨群成果修改未拦截: %s", result)
	}
}

func TestModelJSONErrorsAndUsageSettlement(t *testing.T) {
	// 请求真实经过 HTTPModel、JSON 解码和预算账本；provider 返回截断或空输出时拒绝发布。
	for _, tc := range []struct {
		name, content, reason string
		wantError             bool
	}{{"valid", `{"message":"你好"}`, "stop", false}, {"empty", "   ", "stop", true}, {"truncated", `{"message":"`, "length", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-only" {
					t.Error("鉴权缺失")
				}
				if r.URL.Path != "/responses" {
					t.Errorf("请求走错接口: %s", r.URL.Path)
				}
				status := "completed"
				if tc.reason == "length" {
					status = "incomplete"
				}
				respond(w, 200, map[string]any{"status": status, "output": []any{map[string]any{"type": "message", "phase": "final_answer", "content": []any{map[string]string{"type": "output_text", "text": tc.content}}}}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 5}})
			}))
			defer upstream.Close()
			cfg := testConfig()
			cfg.BaseURL = upstream.URL
			model := HTTPModel{Config: cfg, Store: s, Client: upstream.Client()}
			_, err := model.Complete(context.Background(), []ChatMessage{{Role: "user", Content: "返回 JSON"}}, nil, true)
			if (err != nil) != tc.wantError {
				t.Fatalf("错误状态不符: %v", err)
			}
			u, _ := s.Usage()
			if u.CostMicros != 35 || u.InputTokens != 10 || u.OutputTokens != 5 {
				t.Fatalf("成功调用应按 usage 结算: %+v", u)
			}
		})
	}
}

func TestHTTPTrustBoundaryAndHistoryPagination(t *testing.T) {
	// 外站不得利用浏览器向本机发请求；完整聊天通过向前分页可逐条恢复。
	s, c, _ := fixture(t)
	e := NewEngine(s, nil, testConfig())
	handler := NewHandler(e)
	for i := 0; i < 75; i++ {
		if err := s.SystemMessage(c.ID, fmt.Sprintf("message_%d", i), fmt.Sprintf("消息 %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, host, origin string
		want               int
	}{{"local", "localhost:8090", "", 200}, {"cross origin", "localhost:8090", "https://evil.example", 403}, {"DNS rebinding", "evil.example", "", 403}} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://"+tc.host+"/api/state", nil)
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "test-only") {
				t.Fatal("响应泄露了配置密钥")
			}
		})
	}
	newest, _ := s.Messages(c.ID, 0, 60)
	older, _ := s.Messages(c.ID, newest[0].Seq, 60)
	if len(newest) != 60 || len(older) != 15 || older[len(older)-1].Seq >= newest[0].Seq {
		t.Fatal("历史分页丢失或重复记录")
	}
}

func TestReferencedMessageMustBelongToConversation(t *testing.T) {
	s, c, m := fixture(t)
	other, _ := s.CreateConversation("别的群", "group", []string{m.ID}, time.Hour)
	first := userMessage(c, "只在这个群里")
	if err := s.AddUserMessage(first, time.Hour); err != nil {
		t.Fatal(err)
	}
	second := userMessage(other, "跨群引用")
	second.ReplyTo = first.ID
	if err := s.AddUserMessage(second, time.Hour); err == nil {
		t.Fatal("不能引用另一个群的原始消息")
	}
	// 模型生成的引用与用户引用遵守相同范围，不能借引用带入其他场景原文。
	aiReply := Message{ID: newID("msg"), ConversationID: other.ID, AuthorID: m.ID, Body: "接着说", ReplyTo: first.ID, CreatedAt: now()}
	if err := s.Publish(aiReply, other.Revision); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Messages(other.ID, 0, 10)
	if len(list) != 1 || list[0].ReplyTo != "" || list[0].Body != "接着说" {
		t.Fatal("跨群引用应移除，正文仍可发表")
	}
	// 同一聊天的真实消息仍能正常引用，避免范围检查误伤成员之间的接话。
	current, _ := s.Conversation(c.ID)
	aiReply.ID = newID("msg")
	aiReply.ConversationID = c.ID
	if err := s.Publish(aiReply, current.Revision); err != nil {
		t.Fatalf("当前聊天的有效引用应可发布: %v", err)
	}
}

func TestMentionRemainsInMembersContext(t *testing.T) {
	s, c, members := groupWithMembers(t, 2)
	msg := userMessage(c, "你怎么看？")
	msg.Mentions = []string{members[1].ID}
	s.AddUserMessage(msg, time.Hour)
	c, _ = s.Conversation(c.ID)
	e := NewEngine(s, nil, testConfig())
	for _, member := range members {
		input, err := e.contextFor(c, member, latestChatWork(t, s, c.ID))
		if err != nil || !strings.Contains(asJSON(input["latest_messages"]), members[1].ID) {
			t.Fatalf("@ 语境丢失: %v", err)
		}
	}
}

func TestBackendWorksWithoutAnyBrowserConnection(t *testing.T) {
	// 从数据库领取后台任务，整个过程没有 SSE/浏览器连接。验证活动属于服务进程。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {

		return jsonReply(Outcome{Outcome: "已完成离线期间的文本探索"}), nil
	}), testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var status, result string
		if err := s.db.QueryRow(`SELECT status,result FROM work WHERE id=?`, w.ID).Scan(&status, &result); err != nil {
			t.Fatal(err)
		}
		if status == "done" && result != "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("无浏览器连接时后台研究没有完成")
}

func TestOnlyOneSchedulerCanOwnADataDirectory(t *testing.T) {
	// 第二个服务不能重置第一个服务的 running 工作。进程关闭后才能取得同一数据目录。
	dir := t.TempDir()
	first, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenStore(dir); err == nil {
		second.Close()
		first.Close()
		t.Fatal("同一数据目录允许两个调度器")
	}
	first.Close()
	third, err := OpenStore(dir)
	if err != nil {
		t.Fatal("关闭后锁未释放", err)
	}
	third.Close()
}

func TestSameMemberActsSequentiallyAcrossRooms(t *testing.T) {
	// 同一成员在两个群都有机会时，不允许两份旧记忆并发改写；等待也必须可取消。
	s, c, m := fixture(t)
	c2, err := s.CreateConversation("第二个群", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return jsonReply(Outcome{}), nil
		case <-ctx.Done():
			return ChatMessage{}, ctx.Err()
		}
	})
	e := NewEngine(s, model, testConfig())
	done := make(chan error, 1)
	go func() { _, err := e.act(context.Background(), c, m, Work{}, "检查成员序列化"); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = e.act(ctx, c2, m, Work{}, "等待应可取消"); !errors.Is(err, context.Canceled) {
		t.Fatalf("等待没有尊重取消: %v", err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGracefulShutdownKeepsAcceptedUserWork(t *testing.T) {
	// 服务正常退出与用户插话不同：正常退出应保留已接收消息的处理机会，重启后续办。
	s, c, _ := fixture(t)
	message := userMessage(c, "请帮我核对这个设计的出处")
	if err := s.AddUserMessage(message, time.Hour); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	e := NewEngine(s, modelFunc(func(ctx context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return ChatMessage{}, ctx.Err()
	}), testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("消息没有被领取")
	}
	cancel()
	<-done
	jobs, err := s.Works(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != "pending" {
		t.Fatalf("正常退出丢失用户工作: %+v", jobs)
	}
}

func TestContextDoesNotDropUnreadHistoryByCountOrLength(t *testing.T) {
	// 未读消息不按条数或字符数裁剪；真正的容量处理留给模型请求边界。
	s, c, m := fixture(t)
	body := strings.Repeat("完整原文", 1800)
	for i := 0; i < 80; i++ {
		if err := s.SystemMessage(c.ID, fmt.Sprintf("long_%d", i), body); err != nil {
			t.Fatal(err)
		}
	}
	w := addWork(t, s, c, m, "research")
	s.RecordTool(ToolRun{ID: "tool_long", WorkID: w.ID, ConversationID: c.ID, MemberID: m.ID, Name: "save_artifact", Arguments: json.RawMessage(asJSON(map[string]string{"content": strings.Repeat("资料", 20000)})), Result: strings.Repeat("结果", 20000), Status: "succeeded", CreatedAt: now()})
	e := NewEngine(s, nil, testConfig())
	input, err := e.contextFor(c, m, w)
	if err != nil {
		t.Fatal(err)
	}
	delivered := input["latest_messages"].([]Message)
	if len(delivered) != 80 || delivered[0].Body != body || delivered[79].Body != body {
		t.Fatal("未读原文被条数或字符数截断")
	}
	if strings.Contains(asJSON(input["tool_runs"]), strings.Repeat("结果", 1000)) {
		t.Fatal("旧工具正文被自动重新注入")
	}
	var saved string
	if err = s.db.QueryRow(`SELECT body FROM messages WHERE id='long_0'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if saved != body {
		t.Fatal("构造模型窗口不应修改原文")
	}
}

func TestLastModelRoundClosesAfterSavingArtifact(t *testing.T) {
	// 回归真实验收发现的问题：最后一个工具保存成功后，必须还有一次收尾输出，
	// 不能因循环结束把已完成成果留到下一轮才有机会分享。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	cfg := testConfig()
	calls := 0
	model := modelFunc(func(_ context.Context, _ []ChatMessage, defs []ToolDefinition, jsonOnly bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("save_artifact", map[string]string{"title": "可查看的草稿", "content": "实际正文"})}}, nil
		}
		if len(defs) == 0 {
			t.Error("保存后仍可继续工具行动，不强迫收尾")
		}
		return jsonReply(Outcome{Message: "草稿已保存，可以继续改。", Outcome: "已保存草稿"}), nil
	})
	e := NewEngine(s, model, cfg)
	out, err := e.act(context.Background(), c, m, w, "保存成果并说明结果")
	if err != nil || out.Message == "" || calls != 2 {
		t.Fatalf("成果保存后没有正常收尾: %+v %v calls=%d", out, err, calls)
	}
	artifacts, _ := s.Artifacts(c.ID)
	if len(artifacts) != 1 {
		t.Fatal("成果并未实际保存")
	}
}

func TestPartialResearchKeepsWorkAndCanShareExistingProgress(t *testing.T) {
	// 旧断点中的 unfinished 仍可继续研究；不再额外生成一次分享审批。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	s.Claim(w.ID)
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		return jsonReply(Outcome{Outcome: "已找到原始资料，还需要核对版本差异", Unfinished: true}), nil
	}), testConfig())
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Works(c.ID)
	pending, sharing := false, false
	for _, job := range jobs {
		if job.ID == w.ID {
			pending = job.Status == "pending" && strings.Contains(job.Result, "原始资料")
		}
		if job.Kind == "share" {
			sharing = true
		}
	}
	if !pending || sharing {
		t.Fatalf("部分进展没有与后续工作分开保存: %+v", jobs)
	}
}

// 首轮可自主联网；用户暂停研究仍关闭联网入口。
func TestFirstReplyAllowsToolsAndRespectsPause(t *testing.T) {
	s, c, m := fixture(t)
	e := NewEngine(s, nil, testConfig())
	first := Work{Kind: "first_reply"}
	names := map[string]bool{}
	for _, d := range e.Tools.Definitions(m, c, first) {
		names[d.Function.Name] = true
	}
	for _, name := range []string{"search", "read_page", "save_artifact", "read_tool_result"} {
		if !names[name] {
			t.Fatalf("missing %s", name)
		}
	}
	c.ResearchPaused = true
	for _, d := range e.Tools.Definitions(m, c, first) {
		if d.Function.Name == "search" || d.Function.Name == "read_page" || d.Function.Name == "schedule_exploration" {
			t.Fatal("pause ignored")
		}
	}
}

func TestReasoningBudgetIsPerRequest(t *testing.T) {
	s, _, _ := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/responses" {
			t.Errorf("请求走错接口: %s", r.URL.Path)
		}
		effort := body["reasoning"].(map[string]any)["effort"]
		outputLimit := body["max_output_tokens"]
		expected := float64(8000)
		if effort == "high" {
			expected = 25000
		} else if effort != "none" {
			t.Errorf("unexpected effort %v", effort)
		}
		if outputLimit != expected {
			t.Errorf("wrong budget: %v", body)
		}
		if _, exists := body["max_tokens"]; exists {
			t.Error("legacy output field sent")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.BaseURL = server.URL
	cfg.Model = "gpt-6.1-terra"
	cfg.Reasoning = "none"
	cfg.DeepReasoning = "high"
	cfg.MaxOutputTokens = 8000
	cfg.DeepMaxOutputTokens = 25000
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	var wg sync.WaitGroup
	for _, deep := range []bool{true, false, true, false} {
		wg.Add(1)
		go func(deep bool) {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), deepReasoningKey{}, deep)
			if _, err := model.Complete(ctx, []ChatMessage{{Role: "user", Content: "JSON"}}, nil, true); err != nil {
				t.Error(err)
			}
		}(deep)
	}
	wg.Wait()
	if model.Config.Reasoning != "none" || model.Config.MaxOutputTokens != 8000 {
		t.Fatal("共享配置被改写")
	}
}

// Responses 工具往返必须同时回传推理项目和 call_id，才能延续同一次深入分析。
func TestResponsesPreservesReasoningAndToolOutput(t *testing.T) {
	s, _, _ := fixture(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/responses" || body["store"] != false {
			t.Error("应使用无服务端保存的 Responses")
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.Write([]byte(`{"status":"completed","output":[{"type":"reasoning","id":"rs_test","encrypted_content":"opaque-test-state","summary":[]},{"type":"function_call","id":"fc_test","call_id":"call_test","name":"read_history","arguments":"{}"}],"usage":{"input_tokens":10,"output_tokens":10}}`))
			return
		}
		items := body["input"].([]any)
		if len(items) != 4 {
			t.Errorf("项目丢失: %v", items)
		} else {
			reasoning := items[1].(map[string]any)
			result := items[3].(map[string]any)
			if reasoning["encrypted_content"] != "opaque-test-state" || result["type"] != "function_call_output" || result["call_id"] != "call_test" || result["output"] != "真实历史" {
				t.Error("推理状态或工具结果未保留")
			}
		}
		w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"message\":\"已核对\"}"}]}],"usage":{"input_tokens":20,"output_tokens":10}}`))
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Model = "gpt-6.1-sol"
	cfg.BaseURL = server.URL
	cfg.DeepReasoning = "high"
	cfg.DeepMaxOutputTokens = 25000
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	ctx := context.WithValue(context.Background(), deepReasoningKey{}, true)
	messages := []ChatMessage{{Role: "user", Content: "请核对历史，返回 JSON"}}
	reply, err := model.Complete(ctx, messages, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].ID != "call_test" {
		t.Fatal("没有解析工具调用")
	}
	messages = append(messages, reply, ChatMessage{Role: "tool", ToolCallID: "call_test", Content: "真实历史"})
	reply, err = model.Complete(ctx, messages, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply.Content, "已核对") {
		t.Fatal("最终结果未解析")
	}
}
