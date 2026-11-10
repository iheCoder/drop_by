package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 没有聊天、待办或浏览器时，成员也能真实调用外部接口、留下个人经历、制作并分享。
// 故障在消息发布之后注入：原行动恢复后只接着做，消息和唤醒都不能重复。
func TestPersonalExplorationSharingAndRecovery(t *testing.T) {
	s, room, people := groupWithMembers(t, 2)
	m := people[0]
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/search" || request["query"] != "纸结构与折叠机构" {
			t.Errorf("没有传递本人选择的探索方向: %s %v", r.URL.Path, request)
		}
		json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"url": "https://example.org/folding", "title": "折叠结构", "snippet": "改变折痕，可以改变纸结构展开时的运动。"}}})
	}))
	defer service.Close()
	cfg := testConfig()
	cfg.SearchURL = service.URL
	if err := s.ScheduleExploration(time.Hour); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.personalWorks([]string{m.ID})
	if len(jobs) != 1 {
		t.Fatal(jobs)
	}
	work := jobs[0]
	s.Claim(work.ID)
	var calls int
	var source, artifactRun string
	var sentCall ToolCall
	failure := errors.New("模拟发送后网络断开")
	model := modelFunc(func(_ context.Context, messages []ChatMessage, defs []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if strings.Contains(messages[0].Content, "此时只决定") || strings.Contains(messages[1].Content, "already_spoke") || strings.Contains(messages[1].Content, "initial_reply_opportunity") {
			return ChatMessage{}, errors.New("旧发言暗示仍存在")
		}
		if calls == 1 {
			var input map[string]any
			json.Unmarshal([]byte(latestSceneContent(t, messages)), &input)
			if len(input["latest_messages"].([]any)) != 0 || input["personal_focus"] == nil {
				t.Error("个人活动仍以房间历史为主")
			}
			available := map[string]bool{}
			for _, d := range defs {
				available[d.Function.Name] = true
			}
			for _, name := range []string{"search", "read_page", "save_artifact", "send_message", "read_conversation", "remember"} {
				if !available[name] {
					t.Errorf("行动缺少工具 %s", name)
				}
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("search", map[string]any{"query": "纸结构与折叠机构", "count": 2})}}, nil
		}
		if calls == 2 {
			var result struct {
				ID string `json:"tool_run_id"`
			}
			json.Unmarshal([]byte(messages[len(messages)-1].Content), &result)
			source = result.ID
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
				toolCall("remember", memoryWrite{Body: "折痕方向可以改变运动，想动手试试不同折法。", SourceIDs: []string{source}}),
				toolCall("save_artifact", map[string]string{"title": "纸的运动", "format": "html", "content": "<!doctype html><button>换一个折法</button>"}),
			}}, nil
		}
		if calls == 3 {
			var result struct {
				ID string `json:"tool_run_id"`
			}
			json.Unmarshal([]byte(messages[len(messages)-1].Content), &result)
			artifactRun = result.ID
			sentCall = toolCall("send_message", map[string]any{"conversation_id": room.ID, "message": "纸折个方向，动作就变了。我做了个小东西。", "evidence": []string{artifactRun}})
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{sentCall}}, nil
		}
		if calls == 4 {
			return ChatMessage{}, failure
		}
		focus := "想试试折纸怎样从平面展开成小机关。"
		return jsonReply(Outcome{PersonalFocus: &focus, Outcome: "分享了自己的折纸发现。"}), nil
	})
	e := NewEngine(s, model, cfg)
	if err := e.process(context.Background(), work); !errors.Is(err, failure) {
		t.Fatalf("没有注入后续故障: %v", err)
	}
	resumed := NewEngine(s, model, cfg)
	if err := resumed.process(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if err := resumed.process(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(room.ID, 0, 10)
	if len(messages) != 1 || len(messages[0].Artifacts) != 1 {
		t.Fatalf("分享丢失或重复: %+v", messages)
	}
	var wakeups int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='followup' AND conversation_id=?`, room.ID).Scan(&wakeups)
	if wakeups != 1 || calls != 5 {
		t.Fatalf("恢复重复唤醒或重演完成行动: wakes=%d calls=%d", wakeups, calls)
	}
	focus, _ := s.personalActivity(m.ID)
	if focus.Body == "" {
		t.Fatal("个人关注未保留")
	}
	notes, _ := s.Memories(m.ID, "")
	if len(notes) != 1 || notes[0].SourceID != source {
		t.Fatalf("探索没有形成可追溯个人经历: %+v", notes)
	}
	// 个人行动的实际来源可以用于本人在分享目的地写下的经历。
	if _, err := resumed.Tools.remember(context.Background(), room, m, work, memoryWrite{Body: "把自己的纸结构发现分享到了这里。", SourceIDs: []string{source}}); err != nil {
		t.Fatalf("本人个人材料不能用于聊天经历: %v", err)
	}
	// 模拟进程在消息已插入、工具结果未登记的间隙退出。相同 call ID 可以确认原发送。
	s.db.Exec(`UPDATE tool_runs SET status='running' WHERE name='send_message'`)
	personal, _ := s.personalConversation(m.ID)
	// 个人活动发到群里的话仍是本人的亲历。曾经会因来源不在空 room 中而
	// 拒绝保存，导致已分享的成员还得绕路复制自己的发送结果。
	if _, err := resumed.Tools.remember(context.Background(), personal, m, work, memoryWrite{Body: "今天把折纸发现分享给了同伴。", SourceIDs: []string{messages[0].ID}}); err != nil {
		t.Fatalf("个人活动不能记住自己的分享: %v", err)
	}
	// 这不开放没有加入的聊天；成员只能使用自己实际可访问的来源。
	foreign, _ := s.CreateConversation("同伴的私聊", "direct", []string{people[1].ID}, time.Hour)
	private := userMessage(foreign, "只在这段私聊里出现的事情。")
	if err := s.Publish(private, foreign.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Tools.remember(context.Background(), personal, m, work, memoryWrite{Body: "没有参与过的事情。", SourceIDs: []string{private.ID}}); err == nil {
		t.Fatal("个人经历读取了未加入的聊天")
	}
	if _, err := resumed.Tools.Execute(context.Background(), personal, m, work, sentCall); err != nil {
		t.Fatal(err)
	}
	messages, _ = s.Messages(room.ID, 0, 10)
	if len(messages) != 1 {
		t.Fatal("工具恢复重复发送")
	}
	// 分享的第一版可被同伴阅读；后来安静修改的第二版没有自动公开。
	saved := messages[0].Artifacts[0]
	if _, err := resumed.Tools.saveArtifact(context.Background(), personal, m, work, Artifact{ID: saved.ID, Title: "纸的运动", Content: "未分享的第二版"}); err != nil {
		t.Fatal(err)
	}
	result, err := resumed.Tools.perform(context.Background(), room, people[1], work, toolCall("read_artifact", map[string]any{"id": saved.ID}))
	if err != nil || strings.Contains(asJSON(result), "未分享") || !strings.Contains(asJSON(result), "换一个折法") {
		t.Fatalf("分享版本不可读或泄漏了草稿: %v %v", result, err)
	}
	visible, err := s.Artifacts(room.ID)
	if err != nil || len(visible) != 1 || visible[0].Version != 1 {
		t.Fatalf("聊天成果列表丢失已分享的个人作品或展示未发布版: %v %v", visible, err)
	}
	index, err := s.artifactIndexForMember(room.ID, people[1].ID, "纸", 0)
	if err != nil || !strings.Contains(asJSON(index), saved.ID) {
		t.Fatalf("不能发现已分享的个人作品: %v", err)
	}
	if _, err := resumed.Tools.perform(context.Background(), room, people[1], work, toolCall("read_artifact", map[string]any{"id": saved.ID, "version": 2})); err == nil {
		t.Fatal("同伴读到了未分享版本")
	}
}

// 对人的认识属于形成这份认识的成员；个人活动可想起自己在聊天中形成的理解。
// 更正修改原记录，下一次召回用新版本，不把旧印象从工具历史里重新带回来。
func TestPersonalRelationshipRecallUsesCorrection(t *testing.T) {
	s, room, people := groupWithMembers(t, 2)
	m := people[0]
	source := userMessage(room, "做取舍时我喜欢选项，探索时先放开想。")
	s.Publish(source, room.Revision)
	work := addWork(t, s, room, m, "followup")
	work.SourceID = source.ID
	e := NewEngine(s, nil, testConfig())
	subjects := []string{"user"}
	raw, err := e.Tools.remember(context.Background(), room, m, work, memoryWrite{Body: "用户总想用选择题。", SubjectIDs: &subjects, SourceIDs: []string{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		Memory Memory `json:"memory"`
	}
	json.Unmarshal([]byte(asJSON(raw)), &first)
	input, err := e.personalContext(m, Work{})
	found, readErr := s.searchMemories(m.ID, "", "选择题", 0)
	if err != nil || readErr != nil || input["relationships"] != nil || !strings.Contains(asJSON(found), first.Memory.ID) {
		t.Fatalf("自己对人的认识没有被召回: %v", err)
	}
	_, err = e.Tools.remember(context.Background(), room, m, work, memoryWrite{ID: first.Memory.ID, Revision: &first.Memory.Revision, Body: "做取舍时用选项，开放探索时先一起发散。", SubjectIDs: &subjects, SourceIDs: []string{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	input, _ = e.personalContext(m, Work{})
	updated, readErr := s.readMemory(m.ID, "", first.Memory.ID, nil, false)
	if readErr != nil || input["relationships"] != nil || strings.Contains(asJSON(input), "总想用选择题") || !strings.Contains(asJSON(updated), "开放探索时先一起发散") {
		t.Fatal("个人活动仍使用旧标签")
	}
	other, _ := e.personalContext(people[1], Work{})
	if strings.Contains(asJSON(other), first.Memory.ID) {
		t.Fatal("另一位成员继承了别人的理解")
	}
}

// 生成期间收到同伴发言，先发送已形成的消息，下一次请求再读增量并自行决定补充。
func TestSendMessagePostsThenReadsNewPeer(t *testing.T) {
	s, c, people := groupWithMembers(t, 2)
	m := people[1]
	w := addWork(t, s, c, m, "followup")
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			if err := s.Publish(Message{ID: "new_peer", ConversationID: c.ID, AuthorID: people[0].ID, Body: "我先想到声音能改变视觉。", CreatedAt: now()}, c.Revision); err != nil {
				return ChatMessage{}, err
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("send_message", map[string]string{"message": "尚未看见同伴时的草稿"})}}, nil
		}
		if calls == 2 {
			if strings.Contains(asJSON(messages), `"status":"not_sent"`) || !strings.Contains(messages[len(messages)-1].Content, "声音能改变视觉") {
				t.Error("新现场没有交给发言本人")
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("send_message", map[string]string{"message": "我想反过来试试，画面会怎样改变声音。"})}}, nil
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	if _, err := e.act(context.Background(), c, m, w, ""); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) != 3 || !strings.Contains(messages[1].Body, "草稿") {
		t.Fatalf("普通新消息不应撤回已形成的草稿: %+v", messages)
	}
}

func TestCoalescedMessagesStillWakeTheirAuthorsForEachOther(t *testing.T) {
	// A 的消息形成一份待执行通知，B 在通知处理前回复。合并通知不能让 A 漏读 B。
	s, c, people := groupWithMembers(t, 2)
	e := NewEngine(s, nil, testConfig())
	for _, m := range people {
		personal, _ := s.personalConversation(m.ID)
		work := addWork(t, s, personal, m, "explore")
		if _, err := e.Tools.Execute(context.Background(), personal, m, work, toolCall("send_message", map[string]string{"conversation_id": c.ID, "message": m.Name + "的新发现"})); err != nil {
			t.Fatal(err)
		}
	}
	var wake Work
	jobs, _ := s.Works(c.ID)
	if len(jobs) != 1 {
		t.Fatalf("通知没有合并: %+v", jobs)
	}
	wake = jobs[0]
	var calls atomic.Int32
	e.Model = modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		calls.Add(1)
		return jsonReply(Outcome{}), nil
	})
	if err := e.process(context.Background(), wake); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("合并后只有 %d 人读到彼此的新话", calls.Load())
	}
	if err := e.process(context.Background(), wake); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("已经读完的通知再次唤醒了成员")
	}
}

func TestResearchPublicationInvitesPeersBeforeItsOwnActionEnds(t *testing.T) {
	// 研究者先分享一条想法、继续行动时阻塞并失败。别人应在研究结束之前回复，
	// 研究自己的失败状态保留，群内回应不被它覆盖。
	s, c, people := groupWithMembers(t, 2)
	a, b := people[0], people[1]
	work := addWork(t, s, c, a, "research")
	s.db.Exec(`UPDATE personal_activity SET next_at=?`, time.Now().Add(time.Hour).UnixMilli())
	hold := make(chan struct{})
	started := make(chan struct{})
	var steps int
	e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		var input struct {
			Self    Member `json:"self"`
			Trigger Work   `json:"trigger"`
		}
		json.Unmarshal([]byte(latestSceneContent(t, ms)), &input)
		if input.Trigger.ID == work.ID {
			steps++
			if steps == 1 {
				return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("send_message", map[string]string{"message": "先分享一个方向，我接着查。"})}}, nil
			}
			close(started)
			select {
			case <-hold:
				return ChatMessage{}, errors.New("本人后续请求失败")
			case <-ctx.Done():
				return ChatMessage{}, ctx.Err()
			}
		}
		if input.Self.ID == b.ID && !testMemberSpoke(s, c.ID, b.ID) {
			return jsonReply(Outcome{Message: "我倒想从另一个方向试。"}), nil
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("研究没有开始")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !testMemberSpoke(s, c.ID, b.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !testMemberSpoke(s, c.ID, b.ID) {
		t.Fatal("研究占住了整群，其他人不能接话")
	}
	close(hold)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, _ := s.ReadWork(c.ID, work.ID)
		if job.Status == "failed" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("本人的研究失败没有留下可恢复记录")
}
