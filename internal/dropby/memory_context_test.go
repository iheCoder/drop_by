package dropby

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTopicWindowRetainsIdentityGoalsAndRetrievableHistory(t *testing.T) {
	// 旧话题退出默认输入后，人物特质、仍挂念的事与旧的未完成工作必须留下。
	// 截断只影响本次输入，实际原话、另一个人的阅读窗口和私聊仍相互独立。
	s, c, members := groupWithMembers(t, 2)
	m := members[0]
	old := userMessage(c, "旧话题：做一张蓝色的海报，但不要催促观众。")
	s.AddUserMessage(old, time.Hour)
	c, _ = s.Conversation(c.ID)
	job := addWork(t, s, c, m, "research")
	for i := 0; i < 60; i++ {
		finished := addWork(t, s, c, m, "research")
		s.db.Exec(`UPDATE work SET status='done' WHERE id=?`, finished.ID)
	}
	newer := userMessage(c, "换个话题，今天的晚霞很像桃子。")
	s.AddUserMessage(newer, time.Hour)
	c, _ = s.Conversation(c.ID)
	var seq int64
	s.db.QueryRow(`SELECT seq FROM messages WHERE id=?`, newer.ID).Scan(&seq)
	focus := "海报还没完成；现在更想聊晚霞。"
	if err := s.recordObservation(c, m.ID, seq, &focus, []string{newer.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.rememberContextStart(c, m.ID, seq, &newer.ID); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, nil, testConfig())
	input, err := e.contextFor(c, m, Work{SourceID: newer.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range input["latest_messages"].([]Message) {
		if message.ID == old.ID {
			t.Fatal("无关旧话题仍在默认窗口")
		}
	}
	if input["self"].(map[string]any)["profile"] != m.Profile || input["current_state"].(MemberState).Body != focus {
		t.Fatal("切话题丢掉了身份或持续关注")
	}
	found := false
	for _, w := range input["work"].([]Work) {
		if w.ID == job.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("旧的未完成工作被已完成记录挤掉")
	}
	raw, err := s.searchHistory(c.ID, "", old.ID, 0, 0)
	if err != nil || !strings.Contains(asJSON(raw), old.Body) {
		t.Fatalf("旧话题无法查回：%v %v", raw, err)
	}
	// 多轮交流后，早期约定不在近期16条里，到期工作仍须单独拿到原始来由。
	for i := 0; i < 20; i++ {
		s.SystemMessage(c.ID, newID("later"), "最近另一段闲聊")
	}
	input, err = e.contextFor(c, m, Work{SourceID: old.ID, Kind: "revisit"})
	if err != nil || input["trigger_message"].(*Message).ID != old.ID || !strings.Contains(asJSON(input["trigger_message"]), "不要催促") {
		t.Fatalf("旧触发消息没有独立找回：%v", err)
	}
	other, _ := s.contextStart(c.ID, members[1].ID)
	if other != 0 {
		t.Fatal("一个成员的注意窗口改变了另一个人")
	}
	private, _ := s.CreateConversation("私聊", "direct", []string{m.ID}, time.Hour)
	secret := userMessage(private, "不能跨群的秘密")
	s.AddUserMessage(secret, time.Hour)
	if _, err := s.searchHistory(c.ID, "", secret.ID, 0, 0); err == nil {
		t.Fatal("历史读取越过会话边界")
	}
	if err := s.rememberContextStart(c, m.ID, seq, &secret.ID); err == nil {
		t.Fatal("私聊消息成为群上下文起点")
	}
}

func TestArtifactDiscoveryDoesNotRequireKnownID(t *testing.T) {
	// 旧作品不在最近四份索引中，也没有公开分享消息，仍然能够按标题找到。
	s, c, m := fixture(t)
	for i := 0; i < 18; i++ {
		title := fmt.Sprintf("近期作品 %d", i)
		if i == 0 {
			title = "星灯卡"
		}
		_, err := s.db.Exec(`INSERT INTO artifacts(id,version,conversation_id,member_id,title,content,source_id,created_at,format) VALUES(?,1,?,?,?,'原文不应进入索引','',?,'html')`, fmt.Sprintf("artifact_%d", i), c.ID, m.ID, title, now())
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.artifactIndex(c.ID, "星灯", 0)
	if err != nil || !strings.Contains(asJSON(result), "artifact_0") || strings.Contains(asJSON(result), "原文不应进入索引") {
		t.Fatalf("旧作品无法发现或正文挤占索引：%v %v", result, err)
	}
	private, _ := s.CreateConversation("私聊", "direct", []string{m.ID}, time.Hour)
	result, err = s.artifactIndex(private.ID, "星灯", 0)
	if err != nil || strings.Contains(asJSON(result), "artifact_0") {
		t.Fatal("作品发现越过会话范围")
	}
}

func TestObservationWaitsForSameMembersMemoryUpdate(t *testing.T) {
	// 模拟同一个成员正在另一个会话更新经历。观察不能绕过这把锁读取旧值，
	// 也不能因等待而忽略取消；不同成员仍使用独立的锁。
	s, c, m := fixture(t)
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		t.Error("等待同一成员的经历更新时不应提前请求模型")
		return jsonReply(Outcome{}), nil
	}), testConfig())
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	e.memberGates.Store(m.ID, gate)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.act(ctx, c, m, Work{}, "")
	if err != context.Canceled {
		t.Fatalf("等待取消未返回：%v", err)
	}
	<-gate
}

func TestLongTermMemoryDiscoveryAndHistoryPagination(t *testing.T) {
	// 想找的经历已不在最近索引中。检索仍能找到，而且只看自己的可见范围；
	// 原话列表有游标，长消息可读尾部，避免最后一句更正永远被裁掉。
	s, c, members := groupWithMembers(t, 2)
	m := members[0]
	for i := 0; i < 35; i++ {
		body := fmt.Sprintf("普通记录 %d", i)
		if i == 0 {
			body = "星灯卡只用两色，不要倒计时。"
		}
		s.db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?)`, fmt.Sprintf("memory_%d", i), m.ID, c.ID, body, "", now())
		s.SystemMessage(c.ID, fmt.Sprintf("page_%d", i), "可翻页的聊天")
	}
	s.db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?)`, "other_person", members[1].ID, c.ID, "星灯卡秘密", "", now())
	s.db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?)`, "other_room", m.ID, "private_room", "星灯卡秘密", "", now())
	result, err := s.searchMemories(m.ID, c.ID, "星灯 倒计时", 0)
	if err != nil || !strings.Contains(asJSON(result), "memory_0") || strings.Contains(asJSON(result), "other_person") || !strings.Contains(asJSON(result), "other_room") {
		t.Fatalf("记忆范围/发现出错：%v %v", result, err)
	}
	page, err := s.searchHistory(c.ID, "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := page.(map[string]any)
	page, err = s.searchHistory(c.ID, "", "", first["before_seq"].(int64), 0)
	if err != nil {
		t.Fatal(err)
	}
	second := page.(map[string]any)
	if !first["has_more"].(bool) || first["messages"].([]Message)[11].Seq <= second["messages"].([]Message)[0].Seq {
		t.Fatal("历史分页重复或无法向前")
	}
	long := strings.Repeat("旧描述", 2000) + "最后更正：改用暖白。"
	s.SystemMessage(c.ID, "long_correction", long)
	if !strings.Contains(contextExcerpt(long, 700), "最后更正") {
		t.Fatal("近期摘录丢掉尾部更正")
	}
	full, err := s.searchHistory(c.ID, "", "long_correction", 0, 5000)
	if err != nil || !strings.Contains(asJSON(full), "最后更正") {
		t.Fatalf("完整原文尾部不能读取: %v", err)
	}
}

func TestMemberCanRecallBeforeChoosingSilence(t *testing.T) {
	// 成员直接查找旧经历后结束本次行动。工具齐全，不需要预先承诺发言，
	// 也没有只准查记忆的判断阶段。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "explore")
	s.db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?)`, "old_note", m.ID, c.ID, "书签上的鲸鱼是旧约定", "", now())
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, defs []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		hasSend := false
		for _, d := range defs {
			hasSend = hasSend || d.Function.Name == "send_message"
		}
		if !hasSend {
			t.Error("沉默前的行动不应剥夺消息工具")
		}

		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("search_memory", map[string]string{"query": "鲸鱼"})}}, nil
		}
		if !strings.Contains(messages[len(messages)-1].Content, "旧约定") {
			t.Error("检索结果没有回到判断上下文")
		}
		return jsonReply(Outcome{Outcome: "旧约定没有需要打扰的新进展"}), nil
	}), testConfig())
	result, err := e.act(context.Background(), c, m, w, "")
	if err != nil || result.Message != "" || calls != 2 {
		t.Fatalf("检索后沉默失败：%+v calls=%d", result, calls)
	}
	messages, _ := s.Messages(c.ID, 0, 20)
	if len(messages) != 0 {
		t.Fatal("检索强行产生了聊天消息")
	}
}

func TestTimedFollowupAndPrivateActivityWording(t *testing.T) {
	at := time.Date(2026, 11, 7, 23, 0, 0, 0, time.FixedZone("CST", 8*3600))
	due, err := followupTime(at, 0, "2026-11-08T09:00:00+08:00")
	if err != nil || due.Sub(at) != 10*time.Hour {
		t.Fatalf("绝对时区预约错误：%v %v", due, err)
	}
	for _, raw := range []string{"明天九点", "2026-11-07T09:00:00+08:00"} {
		if _, err := followupTime(at, 0, raw); err == nil {
			t.Fatal("错误/过期时间被接受")
		}
	}
	if _, err := followupTime(at, 60, "2026-11-08T09:00:00+08:00"); err == nil {
		t.Fatal("歧义预约被接受")
	}
	if strings.Contains(observationLabel(Conversation{Kind: "direct"}, Work{Kind: "chat"}), "群") {
		t.Fatal("私聊活动说成读群消息")
	}
}

func TestOptionalToolParametersHaveValidRequiredArray(t *testing.T) {
	// 真实服务拒绝 required:null，即使 strict=false。没有必填项也必须编码为 []。
	s, c, m := fixture(t)
	e := NewEngine(s, nil, testConfig())
	for _, def := range e.Tools.Definitions(m, c, Work{Kind: "chat"}) {
		required, ok := def.Function.Parameters.(map[string]any)["required"].([]string)
		if !ok || required == nil {
			t.Fatalf("%s 的 required 不是有效数组", def.Function.Name)
		}
	}
}
