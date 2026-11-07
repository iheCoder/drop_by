package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMessageReactionsOnOlderPage(t *testing.T) {
	// 原消息已离开首屏。先由成员添加回应，再由真人重复添加同一个表情，
	// 从历史页、实时刷新和成员回读三条路径核对同一组回应者。
	s, c, m := fixture(t)
	old := userMessage(c, "较早的一条消息")
	if err := s.Publish(old, c.Revision); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if err := s.Publish(userMessage(c, fmt.Sprintf("后来第 %d 条", i+1)), c.Revision); err != nil {
			t.Fatal(err)
		}
	}
	c, _ = s.Conversation(c.ID)
	w := addWork(t, s, c, m, "chat")
	e := NewEngine(s, nil, testConfig())
	handler := NewHandler(e)
	engineEvents, unsubscribe := e.Hub.Subscribe()
	defer unsubscribe()
	reactionEvents, unsubscribeReactions := e.Hub.SubscribeReactions()
	defer unsubscribeReactions()

	request := func(method, path, body string, expected int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Code != expected {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	expectReactionEvent := func() {
		t.Helper()
		select {
		case room := <-reactionEvents:
			if room != c.ID {
				t.Fatalf("表情通知发到了 %s", room)
			}
		default:
			t.Fatal("没有收到网页表情刷新通知")
		}
	}
	runTool := func(call ToolCall, status string) string {
		t.Helper()
		raw, err := e.Tools.Execute(context.Background(), c, m, w, call)
		var receipt struct {
			Status string `json:"status"`
		}
		if err != nil || json.Unmarshal([]byte(raw), &receipt) != nil || receipt.Status != status {
			t.Fatalf("工具回执不符: %s %v", raw, err)
		}
		return raw
	}
	add := toolCall(reactionTool, map[string]any{"message_id": old.ID, "emoji": "👍", "present": true})
	originalReceipt := runTool(add, "succeeded")
	expectReactionEvent()
	base := "/api/conversations/" + c.ID
	reactionURL := base + "/messages/" + old.ID + "/reaction"
	request(http.MethodPut, reactionURL, `{"emoji":"👍","present":true}`, 200)
	expectReactionEvent()
	request(http.MethodPut, reactionURL, `{"emoji":"👍","present":true}`, 200)
	request(http.MethodPut, reactionURL, `{"emoji":"👍"}`, 400)
	request(http.MethodPut, reactionURL, `{"emoji":"not an emoji","present":true}`, 400)
	request(http.MethodPut, reactionURL, `{"emoji":"👍","present":false,"actor_id":"`+m.ID+`"}`, 400)
	select {
	case <-reactionEvents:
		t.Fatal("重复写入或无效请求发布了表情事件")
	default:
	}
	// 首屏没有原消息。加载更早的记录后，相同表情合并，两个身份各计一次。
	var page struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(request(http.MethodGet, base, "", 200), &page); err != nil || len(page.Messages) != 60 || page.Messages[0].ID == old.ID {
		t.Fatalf("测试需要让目标消息处于首屏之外: %v", err)
	}
	lastSeq := page.Messages[len(page.Messages)-1].Seq
	older := fmt.Sprintf("%s?before=%d", base, page.Messages[0].Seq)
	if err := json.Unmarshal(request(http.MethodGet, older, "", 200), &page); err != nil || len(page.Messages) != 1 || page.Messages[0].ID != old.ID {
		t.Fatalf("无法翻到原消息: %+v %v", page, err)
	}
	expectActors := func(groups []MessageReaction, actors ...string) {
		t.Helper()
		if len(actors) == 0 && len(groups) == 0 {
			return
		}
		if len(groups) != 1 || groups[0].Emoji != "👍" || !slices.Equal(groups[0].ActorIDs, actors) {
			t.Fatalf("回应者应为 %v，实际 %+v", actors, groups)
		}
	}
	expectActors(page.Messages[0].Reactions, m.ID, "user")
	snapshot := func() []MessageReaction {
		t.Helper()
		var groups map[string][]MessageReaction
		path := fmt.Sprintf("%s/reactions?from_seq=%d&through_seq=%d", base, page.Messages[0].Seq, lastSeq)
		if err := json.Unmarshal(request(http.MethodGet, path, "", 200), &groups); err != nil {
			t.Fatal(err)
		}
		return groups[old.ID]
	}
	expectActors(snapshot(), m.ID, "user")

	// 真人只能撤回自己的身份；成员也能通过原工具撤回，并按 ID 看到最新结果。
	request(http.MethodPut, reactionURL, `{"emoji":"👍","present":false}`, 200)
	expectReactionEvent()
	expectActors(snapshot(), m.ID)
	select {
	case <-engineEvents:
		t.Fatal("表情写入或撤回向群聊引擎发布了消息事件")
	default:
	}
	history := runTool(toolCall("read_history", map[string]string{"id": old.ID}), "succeeded")
	var receipt struct {
		Result struct {
			Message Message `json:"message"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(history), &receipt); err != nil {
		t.Fatal(err)
	}
	expectActors(receipt.Result.Message.Reactions, m.ID)
	// read_history 使用原有工具事件；表情写入和撤回单独验证不会发布这类事件。
	for len(engineEvents) > 0 {
		<-engineEvents
	}
	runTool(toolCall(reactionTool, map[string]any{"message_id": old.ID, "emoji": "👍", "present": false}), "succeeded")
	expectReactionEvent()
	if replay := runTool(add, "succeeded"); replay != originalReceipt {
		t.Fatalf("重复调用没有返回原回执: %s", replay)
	}
	expectActors(snapshot())

	// 换一个聊天的路径或消息 ID 都不能写入；成功的回应也不产生新工作或新序号。
	other, err := s.CreateConversation("另一个群", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request(http.MethodPut, "/api/conversations/"+other.ID+"/messages/"+old.ID+"/reaction", `{"emoji":"👍","present":true}`, 404)
	foreign := userMessage(other, "另一群的消息")
	if err := s.Publish(foreign, other.Revision); err != nil {
		t.Fatal(err)
	}
	runTool(toolCall(reactionTool, map[string]any{"message_id": foreign.ID, "emoji": "👍", "present": true}), "failed")
	select {
	case <-engineEvents:
		t.Fatal("表情工具或用户回应向群聊引擎发布了消息事件")
	default:
	}
	updated, err := s.Conversation(c.ID)
	jobs, workErr := s.Works(c.ID)
	latest, messageErr := s.Messages(c.ID, 0, 1)
	if err != nil || workErr != nil || messageErr != nil || len(latest) != 1 || latest[0].Seq != lastSeq ||
		updated.UpdatedAt != c.UpdatedAt || len(jobs) != 1 || jobs[0].ID != w.ID {
		t.Fatalf("表情改变了会话位置或产生新工作: %+v %+v %v %v", updated, jobs, err, workErr)
	}
}

func TestQuietMemberCanReactDuringExistingAction(t *testing.T) {
	// 两位成员上一轮都沉默。这次真人消息仍给两人各一次行动机会，
	// 其中一人只选表情，另一人沉默；单独的表情不能再叫醒任何人。
	s, c, members := groupWithMembers(t, 2)
	var mu sync.Mutex
	modelCalls := map[string]int{}
	var target string
	e := NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, defs []ToolDefinition, _ bool) (ChatMessage, error) {
		var member string
		for _, m := range members {
			if strings.Contains(messages[0].Content, "（"+m.ID+"）") {
				member = m.ID
				break
			}
		}
		if member == "" {
			return jsonReply(Control{}), nil
		}
		mu.Lock()
		modelCalls[member]++
		mu.Unlock()
		if target != "" && member == members[0].ID && messages[len(messages)-1].Role != "tool" {
			if !slices.ContainsFunc(defs, func(def ToolDefinition) bool { return def.Function.Name == reactionTool }) {
				return ChatMessage{}, fmt.Errorf("安静成员的正常行动没有表情工具")
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall(reactionTool, map[string]any{"message_id": target, "emoji": "😊", "present": true})}}, nil
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := userMessage(c, "先看看这里。")
	if err := s.AddUserMessage(first, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := e.converse(ctx, c, latestChatWork(t, s, c.ID)); err != nil {
		t.Fatal(err)
	}
	second := userMessage(c, "照片里那只猫睡着了。")
	target = second.ID
	if err := s.AddUserMessage(second, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := e.converse(ctx, c, latestChatWork(t, s, c.ID)); err != nil {
		t.Fatal(err)
	}
	if modelCalls[members[0].ID] != 3 || modelCalls[members[1].ID] != 2 {
		t.Fatalf("应只有两次原有行动和一次工具回传: %v", modelCalls)
	}
	messages, err := s.Messages(c.ID, 0, 20)
	if err != nil || len(messages) != 2 || len(messages[1].Reactions) != 1 || !slices.Equal(messages[1].Reactions[0].ActorIDs, []string{members[0].ID}) {
		t.Fatalf("仅表情行动应留在原消息上: %+v %v", messages, err)
	}
}
