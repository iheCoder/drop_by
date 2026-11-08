package dropby

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAnotherMemberCanReplyAfterFirstSchedulesResearch(t *testing.T) {
	// 复现真人第一句话触发研究时全群提前结束的故障。第一人安排研究且标 unfinished，
	// 第二人仍愿意回应；预期两人的话都被发布，研究只安排一份，最后可以一起安静。
	s, c, first := fixture(t)
	second := first
	second.ID = "member_second"
	second.Name = "石粒"
	if err := s.AddMember(second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO participants VALUES(?,?)`, c.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	msg := userMessage(c, "最近这件事你们怎么看？") // 没有 @，不靠点名强迫第二人开口。
	if err := s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	model := modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{Discussion: "keep", Research: "keep"}), nil
		}
		var input struct {
			Self Member `json:"self"`
		}
		if err := json.Unmarshal([]byte(ms[1].Content), &input); err != nil {
			t.Fatal(err)
		}

		if testMemberSpoke(s, c.ID, input.Self.ID) {
			return jsonReply(Outcome{Outcome: "已经表达，没有新补充"}), nil
		}
		if input.Self.ID == second.ID {
			time.Sleep(10 * time.Millisecond)
		}

		if input.Self.ID == first.ID {
			if len(ms) == 3 {
				return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("schedule_exploration", map[string]any{"instruction": "查找公开原始材料", "reason": "需要核实", "delay_seconds": 10})}}, nil
			}
			return jsonReply(Outcome{Message: "我先说个看法，再去核对原始材料。", Unfinished: true}), nil
		}
		return jsonReply(Outcome{Message: "我更关注另一面的影响，不必等资料才开始讨论。"}), nil
	})
	e := NewEngine(s, model, testConfig())
	works, _ := s.DueWorks()
	if err := e.process(context.Background(), works[0]); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) != 3 || !testMemberSpoke(s, c.ID, first.ID) || !testMemberSpoke(s, c.ID, second.ID) {
		t.Fatalf("其他成员没有获得接话机会: %+v", messages)
	}
	var research int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='research'`).Scan(&research)
	if research != 1 {
		t.Fatalf("安排了 %d 份研究，预期一份", research)
	}
	decisions, err := s.Participations(c.ID)
	if err != nil || len(decisions) < 2 {
		t.Fatalf("缺少每位成员每次判断记录: %+v %v", decisions, err)
	}
	if len(e.activities) != 0 {
		t.Fatal("结束后仍伪装为正在行动")
	}
}

func TestQuietRoomCanExploreWithoutHumanMessageOrBrowser(t *testing.T) {
	// 新群没有任何消息，另有一项明天才执行的研究。不能因此阻止今天的自主机会。
	// 全程直接运行服务调度器，没有浏览器、SSE 或新真人消息。
	s, c, m := fixture(t)
	future := addWork(t, s, c, m, "research")
	s.db.Exec(`UPDATE work SET due_at=? WHERE id=?`, time.Now().Add(24*time.Hour).UnixMilli(), future.ID)
	s.db.Exec(`UPDATE conversations SET next_explore=0 WHERE id=?`, c.ID)
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {

		var input struct {
			Messages []Message `json:"latest_messages"`
		}
		json.Unmarshal([]byte(ms[1].Content), &input)
		if len(input.Messages) > 0 {
			return jsonReply(Outcome{Outcome: "这次已经分享过"}), nil
		}

		return jsonReply(Outcome{Message: "纸飞机的机翼长短为什么会影响它的转弯？这个取舍让我很好奇。"}), nil
	}), testConfig())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		s.db.QueryRow(`SELECT count(*) FROM messages WHERE author_id=?`, m.ID).Scan(&count)
		if count > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("没有真人消息和浏览器时，成员未获得主动发言机会")
}

func TestExplorationQueueHonorsPausesAndDoesNotAccumulate(t *testing.T) {
	// 定时器每秒检查，不能每秒创建一批机会；预算等待和明确停止必须继续生效。
	for _, mode := range []string{"normal", "waiting_budget", "research_paused"} {
		t.Run(mode, func(t *testing.T) {
			s, c, m := fixture(t)
			s.db.Exec(`UPDATE conversations SET next_explore=0 WHERE id=?`, c.ID)
			if mode == "waiting_budget" {
				w := addWork(t, s, c, m, "research")
				s.db.Exec(`UPDATE work SET conversation_id=NULL,status='waiting_budget' WHERE id=?`, w.ID)
			}
			if mode == "research_paused" {
				s.db.Exec(`UPDATE conversations SET research_paused=1 WHERE id=?`, c.ID)
			}
			for range 3 {
				if err := s.ScheduleExploration(time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='explore'`).Scan(&count)
			expected := 0
			if mode == "normal" {
				expected = 1
			}
			if count != expected {
				t.Fatalf("获得 %d 次探索机会，预期 %d", count, expected)
			}
		})
	}
}

func TestToolExcerptKeepsRecoverableResults(t *testing.T) {
	// 资料索引展示摘录时保留回读 ID，重复生成摘录不套娃。
	// SQLite 原文可分段找回，行动链不再按最近工具条数自动裁剪。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	messages := []ChatMessage{{Role: "user", Content: "最近这件事结果出来了呀"}}
	raw := strings.Repeat("原始资料。", 4000)
	for i := 0; i < 17; i++ {
		id := newID("tool")
		callID := newID("call")
		if err := s.RecordTool(ToolRun{ID: id, WorkID: w.ID, ConversationID: c.ID, MemberID: m.ID, Name: "read_page", Arguments: json.RawMessage(`{}`), Result: raw, Status: "succeeded", CreatedAt: now()}); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, ChatMessage{Role: "tool", ToolCallID: callID, Content: asJSON(map[string]any{"tool_run_id": id, "status": "succeeded", "result": raw})})
	}
	messages[1].Content = toolExcerpt(messages[1].Content, 700)
	before := asJSON(messages)
	messages[1].Content = toolExcerpt(messages[1].Content, 700)
	if asJSON(messages) != before {
		t.Fatal("反复压缩改变已形成的摘录")
	}
	var first map[string]any
	json.Unmarshal([]byte(messages[1].Content), &first)
	id := first["tool_run_id"].(string)
	if len([]rune(messages[1].Content)) > 1000 || !strings.Contains(messages[17].Content, raw) {
		t.Fatal("正文没有按新旧收缩")
	}
	tb := Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	result, err := tb.perform(context.Background(), c, m, w, toolCall("read_tool_result", map[string]any{"id": id, "offset": 5000}))
	if err != nil {
		t.Fatal(err)
	}
	part := result.(map[string]any)
	if part["content"] != string([]rune(raw)[5000:10000]) || part["has_more"] != true {
		t.Fatal("无法找回完整原文的后续部分")
	}
	other, _ := s.CreateConversation("其他群", "group", []string{m.ID}, time.Hour)
	if _, err := tb.perform(context.Background(), other, m, w, toolCall("read_tool_result", map[string]any{"id": id})); err != nil {
		t.Fatalf("本人已知的工具结果不能跨群回读: %v", err)
	}
}

func TestLargeEncryptedReasoningIsNotUserContextOrBudget(t *testing.T) {
	// 加密推理的 Base64 超过旧的 180KB 门槛，但只有少量实际 token；应原样回传
	// Responses，按真实输出量估算预留。旧实现会先误报上下文过长或用掉虚假的预算。
	s, _, _ := fixture(t)
	opaque := strings.Repeat("opaque", 40000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/responses" || !bytes.Contains(raw, []byte(opaque)) || !bytes.Contains(raw, []byte("call_long")) {
			t.Error("Responses 推理状态或工具 ID 丢失")
		}
		io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"已核对"}]}],"usage":{"input_tokens":100,"output_tokens":10}}`)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.Model = "gpt-6.1-sol"
	cfg.Reasoning = "high"
	cfg.BaseURL = server.URL
	cfg.WeeklyBudgetMicros = 50_000
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	messages := []ChatMessage{{Role: "user", Content: "核对这件事"}, {Role: "assistant", OutputTokens: 100, ResponseItems: []json.RawMessage{json.RawMessage(asJSON(map[string]any{"type": "reasoning", "id": "rs_long", "encrypted_content": opaque, "summary": []any{}})), json.RawMessage(`{"type":"function_call","id":"fc_long","call_id":"call_long","name":"read_history","arguments":"{}"}`)}}, {Role: "tool", ToolCallID: "call_long", Content: "已保存的事实"}}
	if _, err := model.Complete(context.Background(), messages, nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestNameOnlyCreationAndOptionalPreference(t *testing.T) {
	// 没有模型和额度，也能立即创建身份。跳过偏好不调用模型、不更改资料；
	// 自愿回答只修改原身份，既有私聊仍引用同一个成员 ID。
	s, _, _ := fixture(t)
	e := NewEngine(s, nil, testConfig())
	h := NewHandler(e)
	request := func(path string, body any) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost"+path, strings.NewReader(asJSON(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("/api/members", map[string]any{"name": "简简"})
	if w.Code != 201 {
		t.Fatalf("只填名字无法创建: %d %s", w.Code, w.Body.String())
	}
	var m Member
	json.Unmarshal(w.Body.Bytes(), &m)
	room, err := s.CreateConversation(m.Name, "direct", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w = request("/api/members/"+m.ID+"/preferences", map[string]any{"answer": ""})
	if w.Code != 200 {
		t.Fatal("跳过仍要求模型或问卷")
	}
	saved, _ := s.Member(m.ID)
	if saved.Profile != m.Profile {
		t.Fatal("跳过修改了偏好")
	}
	e.Model = modelFunc(func(_ context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		return jsonReply(map[string]any{"profile": "喜欢主动提出有具体内容的想法。", "tools": []string{"search", "read_page", "save_artifact"}}), nil
	})
	w = request("/api/members/"+m.ID+"/preferences", map[string]any{"question": Question{Text: "怎么开启话题？"}, "answer": "主动分享自己的新发现"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	saved, _ = s.Member(m.ID)
	same, _ := s.Conversation(room.ID)
	if saved.ID != m.ID || saved.CreatedAt != m.CreatedAt || same.MemberIDs[0] != m.ID || !strings.Contains(saved.Profile, "主动") {
		t.Fatal("偏好更新破坏了身份或没有生效")
	}
}

func TestAvatarAcceptsOnlySmallRasterImages(t *testing.T) {
	// 上传图片不能引入可执行 SVG、任意 URL 或巨大图片；正常小 PNG 可以持久化。
	var pngData bytes.Buffer
	png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 32, 32)))
	if err := validateAvatar("data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData.Bytes())); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"https://example.com/a.png", "data:image/svg+xml;base64,PHN2Zz4=", "data:image/png;base64,broken"} {
		if validateAvatar(invalid) == nil {
			t.Fatalf("放行非法头像 %q", invalid)
		}
	}
}

func TestResearchSavesRequestedNoteBeforeClosing(t *testing.T) {
	// 先分段读取十二次，再由成员自行保存和结束；超过旧八轮边界仍有全部工具。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	cfg := testConfig()
	calls := 0
	model := modelFunc(func(_ context.Context, ms []ChatMessage, defs []ToolDefinition, final bool) (ChatMessage, error) {
		calls++
		if len(defs) == 0 {
			t.Fatal("不应按轮数剥夺工具或强迫收尾")
		}
		if calls <= 12 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("read_history", map[string]any{"query": ""})}}, nil
		}
		if calls == 13 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("save_artifact", map[string]string{"title": "已核对的短笔记", "content": "记录实际材料缺口。"})}}, nil
		}
		return jsonReply(Outcome{Outcome: "已保存短笔记"}), nil
	})
	e := NewEngine(s, model, cfg)
	out, err := e.act(context.Background(), c, m, w, "用户要求研究并保存短笔记")
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := s.Artifacts(c.ID)
	if err != nil || len(artifacts) != 1 || out.Unfinished {
		t.Fatalf("收尾未形成实际成果: %+v %+v %v", artifacts, out, err)
	}
}
