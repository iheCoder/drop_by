package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 网页不存在是一次工具结果，不是发言禁令。真实工具走 HTTP 404，随后
// 同一个模型行动收到失败结果、换成自己的看法；不需要再次调用恢复入口。
func TestWebFailureReturnsToMember(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	cfg := testConfig()
	cfg.SearchURL = server.URL
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, jsonOutput bool) (ChatMessage, error) {
		calls++
		if !jsonOutput {
			t.Fatal("行动未使用 JSON 输出协议")
		}
		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("read_page", map[string]string{"url": "https://8.8.8.8/missing"})}}, nil
		}
		if !strings.Contains(ms[len(ms)-1].Content, `"status":"failed"`) {
			t.Fatal("工具失败没有交回本人")
		}
		return jsonReply(Outcome{Message: "我就吃这种重逢的拉扯感。"}), nil
	}), cfg)
	out, err := e.act(context.Background(), c, m, w, "")
	if err != nil || out.Message != "我就吃这种重逢的拉扯感。" || calls != 2 {
		t.Fatalf("工具失败打断发言或追加审核: %+v %v calls=%d", out, err, calls)
	}
	var status string
	if err := s.db.QueryRow("select status from tool_runs where work_id=?", w.ID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("失败记录丢失: %s %v", status, err)
	}
}

// 第一位的模型彻底失败，第二位仍能发言。恢复只接上第一位保存的现场；
// 已成功的同伴不会被重新发布，也不会把错误伪装成第一位自己选择沉默。
func TestMemberFailureDoesNotStopPeersAndResumesOnce(t *testing.T) {
	s, c, members := groupWithMembers(t, 2)
	msg := userMessage(c, "各自说说你们的看法")
	msg.Mentions = []string{members[0].ID} // 明确提及失败者也不应阻断同伴；不规定谁先执行。
	if err := s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Conversation(c.ID)
	w := latestChatWork(t, s, c.ID)
	w.Kind = "followup"
	fail := true
	fault := errors.New("成员请求失败")
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		var input struct {
			Self Member `json:"self"`
		}
		if err := json.Unmarshal([]byte(latestSceneContent(t, ms)), &input); err != nil {
			return ChatMessage{}, err
		}

		if testMemberSpoke(s, c.ID, input.Self.ID) {
			return jsonReply(Outcome{}), nil
		}

		if input.Self.ID == members[0].ID && fail {
			return ChatMessage{}, fault
		}
		return jsonReply(Outcome{Message: input.Self.Name + "的想法"}), nil
	}), testConfig())
	if err := e.process(context.Background(), w); !errors.Is(err, fault) {
		t.Fatalf("本人故障未留下: %v", err)
	}
	messages, _ := s.Messages(c.ID, 0, 20)
	if len(messages) != 2 || messages[1].AuthorID != members[1].ID {
		t.Fatalf("同伴被阻断: %+v", messages)
	}
	fail = false
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if err := e.process(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	messages, _ = s.Messages(c.ID, 0, 20)
	if len(messages) != 3 || messages[2].AuthorID != members[0].ID {
		t.Fatalf("恢复丢失或重复: %+v", messages)
	}
}

// 已生成的旧版纯正文可以直接发布；旧断点中的公共审核提示不应继续生效。
func TestPlainTextCheckpointAndMemberWordingSurvive(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		t.Fatal("已有正文不应调用审核或重新生成")
		return ChatMessage{}, nil
	}), testConfig())
	text := "我就认这个说法！哪怕你们不同意。"
	cp := actionCheckpoint{ID: newID("msg"), WorkID: w.ID, MemberID: m.ID, Revision: c.Revision, Messages: []ChatMessage{{Role: "system", Content: "旧的统一审核指令"}, {Role: "user", Content: "{}"}, {Role: "assistant", Content: text}}}
	if err := s.saveAction(cp); err != nil {
		t.Fatal(err)
	}
	out, err := e.act(context.Background(), c, m, w, "")
	if err != nil || out.Message != text {
		t.Fatalf("正文被拦截或改写: %+v %v", out, err)
	}
	saved, _ := s.loadAction(w.ID, m.ID)
	if strings.Contains(saved.Messages[0].Content, "旧的统一审核指令") {
		t.Fatal("沿用了已删除的公共提示")
	}
}

// 保存作品应保留作者的原文，不再调用模型统一改口。
func TestContentToolsPreserveMemberWordsWithoutReview(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		t.Fatal("工具保存不应另请模型审核")
		return ChatMessage{}, nil
	}), testConfig())
	text := "这对就是我今年的快乐源泉！"
	if _, err := e.Tools.Execute(context.Background(), c, m, w, toolCall("save_artifact", map[string]string{"title": "我的看法", "content": text})); err != nil {
		t.Fatal(err)
	}
	artifacts, _ := s.Artifacts(c.ID)
	if len(artifacts) != 1 || artifacts[0].Content != text {
		t.Fatal("保存时改写了作者")
	}
}
