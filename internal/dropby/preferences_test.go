package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func preferenceRequest(h interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, method, path string, body any) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(asJSON(body)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWelcomeQuestionIsOptionalChatAndDoesNotNeedModel(t *testing.T) {
	// 前置条件：尚无模型连接。仅凭名字创建，打开私聊仍可立即看到一个相识问题。
	s, _, _ := fixture(t)
	h := NewHandler(NewEngine(s, nil, testConfig()))
	created := preferenceRequest(h, "POST", "/api/members", map[string]any{"name": "小月", "avatar_icon": "moon"})
	if created.Code != 201 {
		t.Fatal(created.Body.String())
	}
	var m Member
	json.Unmarshal(created.Body.Bytes(), &m)
	opened := preferenceRequest(h, "POST", "/api/conversations", map[string]any{"kind": "direct", "member_ids": []string{m.ID}})
	if opened.Code != 201 {
		t.Fatal(opened.Body.String())
	}
	var c Conversation
	json.Unmarshal(opened.Body.Bytes(), &c)
	messages, err := s.Messages(c.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Question == nil || messages[0].QuestionStatus != "open" || messages[0].AuthorID != m.ID {
		t.Fatalf("没有单条可选问题: %+v", messages)
	}

	// 用户无需回答或点击跳过，直接聊别的；正文保留，选项立即收起，也不改人物偏好。
	response := preferenceRequest(h, "POST", "/api/conversations/"+c.ID+"/messages", map[string]any{"body": "今天看到一只很可爱的猫"})
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	messages, _ = s.Messages(c.ID, 0, 10)
	if len(messages) != 2 || messages[0].QuestionStatus != "dismissed" {
		t.Fatalf("普通消息没有自然略过: %+v", messages)
	}
	saved, _ := s.Member(m.ID)
	if saved.Profile != m.Profile {
		t.Fatal("聊天改写了尚未明确的偏好")
	}

	// 再次打开同一私聊不重复问入门题，已有消息与同一身份保持连续。
	opened = preferenceRequest(h, "POST", "/api/conversations", map[string]any{"kind": "direct", "member_ids": []string{m.ID}})
	messages, _ = s.Messages(c.ID, 0, 10)
	if opened.Code != 201 || len(messages) != 2 {
		t.Fatal("重新打开私聊重复创建欢迎问题")
	}
}

func TestQuestionAnswerBelongsToAskerAndUpdatesSameMember(t *testing.T) {
	// 群里两位成员均可发言；一个人的选择题不能意外修改另一个人的性格。
	s, _, first := fixture(t)
	second := Member{ID: "member_two", Name: "另一位", Profile: "喜欢安静地听。", CreatedAt: now()}
	if err := s.AddMember(second); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation("两个人", "group", []string{first.ID, second.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.AskQuestion(c, first, Question{Text: "怎么相处？", Options: []string{"主动分享", "慢慢听"}}, "怎么相处？", true)
	if err != nil {
		t.Fatal(err)
	}
	answer := userMessage(c, "主动分享")
	answer.ReplyTo = q.ID
	if err = s.AddUserMessage(answer, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	input, err := s.PreferenceContext(c, answer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asJSON(input), first.ID) || !strings.Contains(asJSON(input), q.ID) {
		t.Fatal("控制上下文丢失问题归属")
	}
	wrong := PreferenceUpdate{MemberID: second.ID, Profile: "变得非常主动。", SourceID: answer.ID}
	if err = s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error { return applyPreferenceUpdates(tx, c, answer.ID, []PreferenceUpdate{wrong}) }); err == nil {
		t.Fatal("允许问题回答修改另一成员")
	}

	// 正确归属的明确回答只更新原成员 profile；其 ID、创建时间、历史消息不变。
	right := PreferenceUpdate{MemberID: first.ID, Profile: first.Profile + " 愿意主动分享自己的发现。", SourceID: answer.ID}
	if err = s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error { return applyPreferenceUpdates(tx, c, answer.ID, []PreferenceUpdate{right}) }); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Member(first.ID)
	unchanged, _ := s.Member(second.ID)
	messages, _ := s.Messages(c.ID, 0, 10)
	if saved.ID != first.ID || saved.CreatedAt != first.CreatedAt || !strings.Contains(saved.Profile, "主动分享") || unchanged.Profile != second.Profile || messages[0].QuestionStatus != "answered" {
		t.Fatal("偏好更新破坏人物身份或问题归属")
	}
	otherRoom, _ := s.CreateConversation("同一朋友", "direct", []string{first.ID}, time.Hour)
	if otherRoom.MemberIDs[0] != saved.ID {
		t.Fatal("跨会话不是同一个人物")
	}
}

func TestQuestionFreeAnswerAndDistinctCalls(t *testing.T) {
	// 普通文字既可以答题，也可以换话题。这里只测试语义判断之后的持久化边界，
	// 不把固定关键词当成“模型真的理解了”的证据。
	s, c, m := fixture(t)
	q := Question{Text: "喜欢怎样接话？", Options: []string{"短一点", "多聊一点"}}
	first, err := s.AskQuestion(c, m, q, q.Text, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AskQuestion(c, m, q, q.Text, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("新调用被错误合并到旧问题")
	}
	answer := userMessage(c, "你以后可以短一点，像朋友一样就好。")
	if err = s.AddUserMessage(answer, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	update := PreferenceUpdate{MemberID: m.ID, Profile: m.Profile + " 平时简短地接话。", SourceID: answer.ID}
	if err = s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error { return applyPreferenceUpdates(tx, c, answer.ID, []PreferenceUpdate{update}) }); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if messages[1].QuestionStatus != "answered" {
		t.Fatal("自由文字回答没有归入原问题")
	}
	// 每次新调用都保留自己的问题，不能用已有问题替换新意图。
	third, err := s.AskQuestion(c, m, q, q.Text, false)
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := s.AskQuestion(c, m, q, q.Text, false)
	if err != nil || third.ID == fourth.ID {
		t.Fatal("新提问被合并")
	}
}

func TestMemberEditorPreservesConversationAndInvalidatesOldDraft(t *testing.T) {
	// 在创建之后换名字、默认头像和人物说明，不新建身份，也不会让旧人物草稿继续发布。
	s, _, m := fixture(t)
	c, err := s.CreateConversation(m.Name, "direct", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	old := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: m.ID, Body: "这是我们已有的聊天。", CreatedAt: now()}
	if err = s.Publish(old, c.Revision); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(NewEngine(s, nil, testConfig()))
	w := preferenceRequest(h, "PATCH", "/api/members/"+m.ID, map[string]any{"name": "阿苇的新名字", "avatar_icon": "fox", "color": "blue", "profile": "爱开玩笑，也认真听对方说话。"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	saved, _ := s.Member(m.ID)
	current, _ := s.Conversation(c.ID)
	messages, _ := s.Messages(c.ID, 0, 10)
	if saved.ID != m.ID || saved.CreatedAt != m.CreatedAt || saved.AvatarIcon != "fox" || current.Name != saved.Name || len(messages) != 1 || messages[0].Body != old.Body {
		t.Fatal("编辑损坏原人物、头像或聊天")
	}
	if err = s.Publish(Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: m.ID, Body: "旧设定草稿", CreatedAt: now()}, c.Revision); err == nil {
		t.Fatal("旧人物草稿越过编辑提交")
	}
	bad := preferenceRequest(h, "PATCH", "/api/members/"+m.ID, map[string]any{"avatar": "https://tracker.invalid/avatar.png"})
	if bad.Code != 400 {
		t.Fatal("头像编辑绕过了本地图片验证")
	}
	// 外观修改不打断正在聊的话；只有真正改变名字/人物说明才废弃旧上下文。
	appearance := preferenceRequest(h, "PATCH", "/api/members/"+m.ID, map[string]any{"avatar_icon": "sun", "color": "rose"})
	afterAppearance, _ := s.Conversation(c.ID)
	if appearance.Code != 200 || afterAppearance.Revision != current.Revision {
		t.Fatal("只换头像也中断了聊天")
	}
}

func TestAskQuestionToolPublishesOneMessage(t *testing.T) {
	// 首次接话可以使用即时本地问题工具；不需要为了显示选择题启动搜索或表单流程。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "first_reply")
	tb := Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	result, err := tb.Execute(context.Background(), c, m, w, toolCall("ask_question", map[string]any{"text": "更想聊哪一面？", "options": []string{"日常里的小事", "一个没想通的问题"}}))
	if err != nil || !strings.Contains(result, "succeeded") {
		t.Fatalf("问题工具失败: %s %v", result, err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) != 1 || messages[0].Question == nil || messages[0].QuestionStatus != "open" {
		t.Fatal("工具没有保存可交互问题消息")
	}
}

func TestDelayedLegacyPreferenceDoesNotOverwriteNewIdentity(t *testing.T) {
	// 模拟旧版偏好请求仍在等模型时，用户在新资料面板换了名字和默认头像。
	// 后返回的模型只能合并偏好，不能把旧身份外观整份写回。
	s, _, m := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	model := modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		close(started)
		<-release
		return jsonReply(map[string]any{"profile": "喜欢主动分享原始材料。", "tools": []string{"search"}}), nil
	})
	h := NewHandler(NewEngine(s, model, testConfig()))
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- preferenceRequest(h, "POST", "/api/members/"+m.ID+"/preferences", map[string]any{"question": Question{Text: "怎样相处？"}, "answer": "主动分享"})
	}()
	<-started
	edit := preferenceRequest(h, "PATCH", "/api/members/"+m.ID, map[string]any{"name": "新名字", "avatar_icon": "owl"})
	if edit.Code != 200 {
		close(release)
		t.Fatal(edit.Body.String())
	}
	close(release)
	merged := <-result
	if merged.Code != 200 {
		t.Fatal(merged.Body.String())
	}
	saved, _ := s.Member(m.ID)
	if saved.Name != "新名字" || saved.AvatarIcon != "owl" || !strings.Contains(saved.Profile, "主动分享") || saved.ID != m.ID {
		t.Fatal("迟到的偏好结果覆盖了用户刚编辑的身份")
	}
}

func TestDelayedLegacyPreferenceRespectsNewProfile(t *testing.T) {
	// 人物说明本身已被用户更新时，旧模型答案必须给出冲突，保留最新明确设定。
	s, _, m := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	model := modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		close(started)
		<-release
		return jsonReply(map[string]any{"profile": "旧请求的主动风格。"}), nil
	})
	h := NewHandler(NewEngine(s, model, testConfig()))
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- preferenceRequest(h, "POST", "/api/members/"+m.ID+"/preferences", map[string]any{"answer": "主动一点"})
	}()
	<-started
	edit := preferenceRequest(h, "PATCH", "/api/members/"+m.ID, map[string]any{"profile": "现在希望更安静，认真听。"})
	close(release)
	merged := <-result
	saved, _ := s.Member(m.ID)
	if edit.Code != 200 || merged.Code != 409 || saved.Profile != "现在希望更安静，认真听。" {
		t.Fatal("迟到的模型覆盖了新的明确人物设定")
	}
}
