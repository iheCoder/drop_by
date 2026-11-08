package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 普通真人消息直接由成员判断；伪造一个“公共控制故障”不能再阻断所有人。
func TestOrdinaryChatHasNoSharedModelGate(t *testing.T) {
	s, c, people := groupWithMembers(t, 2)
	s.AddUserMessage(userMessage(c, "有什么新发现？"), time.Hour)
	var calls atomic.Int32
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return ChatMessage{}, errors.New("公共模型故障")
		}
		calls.Add(1)
		return jsonReply(Outcome{}), nil
	}), testConfig())
	if err := e.process(context.Background(), latestChatWork(t, s, c.ID)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != int32(len(people)) {
		t.Fatal("成员没有独立获得机会", calls.Load())
	}
}

// 在恢复前新增条件。旧 send_message 尚未执行，已经保存的作品应仍可利用，
// 原角色先看到新要求再形成消息，不能先发旧稿。
func TestRecoveryReconsidersUnsentDraftWithLatestMessage(t *testing.T) {
	s, c, m := fixture(t)
	s.AddUserMessage(userMessage(c, "查查户外去处"), time.Hour)
	w := latestChatWork(t, s, c.ID)
	s.Claim(w.ID)
	c, _ = s.Conversation(c.ID)
	e := NewEngine(s, nil, testConfig())
	input, _ := e.contextFor(c, m, w)
	seen := observedThrough(input)
	draft := toolCall("send_message", map[string]string{"message": "旧的户外方案"})
	cp := actionCheckpoint{ID: newID("msg"), WorkID: w.ID, MemberID: m.ID, Revision: c.Revision, Seen: seen, Messages: []ChatMessage{{Role: "system", Content: "原身份"}, {Role: "user", Content: asJSON(input)}, {Role: "assistant", ToolCalls: []ToolCall{draft}}}}
	s.saveAction(cp)
	s.AddUserMessage(userMessage(c, "改成室内的，之前资料能用就用"), time.Hour)
	var checked bool
	e.Model = modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		raw := asJSON(ms)
		if !strings.Contains(raw, "改成室内的") || !strings.Contains(raw, "not_executed") {
			t.Fatal("恢复没有先交付新情况", raw)
		}
		checked = true
		return jsonReply(Outcome{Message: "我改查室内的，已有资料保留。"}), nil
	})
	if _, err := e.memberAction(context.Background(), c, m, w); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.Messages(c.ID, 0, -1)
	if !checked || len(msgs) != 3 || msgs[2].Body == "旧的户外方案" {
		t.Fatal("旧计划先发出了", msgs)
	}
}

// 暂时网络故障保留原工作身份；下次时钟不能另开一份探索将其搁置。
func TestNetworkFailureSchedulesSamePersonalAction(t *testing.T) {
	s, _, m := fixture(t)
	s.ScheduleExploration(time.Hour)
	jobs, _ := s.personalWorks([]string{m.ID})
	w := jobs[0]
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		return ChatMessage{}, &requestFailure{Stage: "暂时断网", Temporary: true}
	}), testConfig())
	e.dispatch(context.Background())
	e.wg.Wait()
	var status string
	s.db.QueryRow(`SELECT status FROM work WHERE id=?`, w.ID).Scan(&status)
	if status != "pending" {
		t.Fatal("未安排原工作恢复", status)
	}
	s.db.Exec(`UPDATE personal_activity SET next_at=0`)
	s.ScheduleExploration(time.Hour)
	jobs, _ = s.personalWorks([]string{m.ID})
	if len(jobs) != 1 || jobs[0].ID != w.ID {
		t.Fatal("重复创建探索", jobs)
	}
}

// 个人活动能在指定群发问题卡，也能回应另一房间消息；故障重放不重复发卡。
func TestPersonalSocialToolsTargetConversation(t *testing.T) {
	s, c, m := fixture(t)
	p, _ := s.personalConversation(m.ID)
	w := addWork(t, s, p, m, "explore")
	e := NewEngine(s, nil, testConfig())
	call := toolCall("ask_question", map[string]any{"conversation_id": c.ID, "text": "想看哪种？", "options": []string{"声音", "图像"}})
	for range 2 {
		r, err := e.Tools.Execute(context.Background(), p, m, w, call)
		if err != nil || !strings.Contains(r, "succeeded") {
			t.Fatal(r, err)
		}
	}
	msgs, _ := s.Messages(c.ID, 0, -1)
	if len(msgs) != 1 || msgs[0].Question == nil {
		t.Fatal(msgs)
	}
	r, err := e.Tools.Execute(context.Background(), p, m, w, toolCall(reactionTool, map[string]any{"message_id": msgs[0].ID, "emoji": "👍", "present": true}))
	if err != nil || !strings.Contains(r, "succeeded") {
		t.Fatal(r, err)
	}
	var wakes int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE conversation_id=? AND kind='followup'`, c.ID).Scan(&wakes)
	if wakes != 1 {
		t.Fatal("问题没有唤醒同伴或重复唤醒", wakes)
	}
}

// 成员自行理解任命，工具落地后其他人能读到同一职责，无需公共控制调用。
func TestMemberInstructionRecordsSharedResponsibility(t *testing.T) {
	s, c, ms := groupWithMembers(t, 2)
	msg := userMessage(c, "阿苇，这次你来组织辩论")
	s.AddUserMessage(msg, time.Hour)
	w := latestChatWork(t, s, c.ID)
	e := NewEngine(s, nil, testConfig())
	r, err := e.Tools.Execute(context.Background(), c, ms[0], w, toolCall("apply_instruction", map[string]any{"source_id": msg.ID, "responsibility_changes": []ResponsibilityChange{{MemberID: ms[0].ID, ScopeID: "this", Body: "组织本次辩论"}}}))
	if err != nil || !strings.Contains(r, "succeeded") {
		t.Fatal(r, err)
	}
	c, _ = s.Conversation(c.ID)
	input, _ := e.contextFor(c, ms[1], w)
	if !strings.Contains(asJSON(input), "组织本次辩论") || !s.CanDelegate(c, ms[0], w) {
		t.Fatal("共享职责没有生效")
	}
}

// 等待中的本人聊天在现有调用结束后接手。没有同时运行两个会覆盖状态的分身。
func TestPersonalActivityYieldsAtSavedBoundary(t *testing.T) {
	s, c, m := fixture(t)
	p, _ := s.personalConversation(m.ID)
	w := addWork(t, s, p, m, "explore")
	chat := addWork(t, s, c, m, "followup")
	entered, release := make(chan struct{}), make(chan struct{})
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		var scene struct {
			Trigger Work `json:"trigger"`
		}
		json.Unmarshal([]byte(ms[1].Content), &scene)
		if scene.Trigger.ID == w.ID {
			close(entered)
			<-release
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("search_memory", map[string]string{"query": ""})}}, nil
		}
		return jsonReply(Outcome{Message: "我看到你了。"}), nil
	}), testConfig())
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := e.memberAction(context.Background(), p, m, w); first <- err }()
	<-entered
	go func() { _, err := e.memberAction(context.Background(), c, m, chat); second <- err }()
	deadline := time.Now().Add(time.Second)
	for e.gate(m.ID).waiting.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-first; !errors.Is(err, errActionYield) {
		t.Fatal("未保存让出", err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if !testMemberSpoke(s, c.ID, m.ID) {
		t.Fatal("等待聊天没有执行")
	}
	if _, err := s.loadAction(w.ID, m.ID); err != nil {
		t.Fatal("探索断点丢失", err)
	}
}

// A 的短暂故障只影响已尝试的消息。B 随后发言时 A 能再次判断，不必等真人催促。
func TestFailedMemberReceivesNewPeerMessage(t *testing.T) {
	s, c, people := groupWithMembers(t, 2)
	a, b := people[0], people[1]
	w := addWork(t, s, c, Member{}, "followup")
	failed := make(chan struct{})
	var ac, bc atomic.Int32
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		var scene struct {
			Self Member `json:"self"`
		}
		json.Unmarshal([]byte(ms[1].Content), &scene)
		if scene.Self.ID == a.ID {
			if ac.Add(1) == 1 {
				close(failed)
				return ChatMessage{}, errors.New("短暂故障")
			}
			return jsonReply(Outcome{}), nil
		}
		if scene.Self.ID == b.ID && bc.Add(1) == 1 {
			<-failed
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("send_message", map[string]string{"message": "另一个想法，你怎么看？"})}}, nil
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	if err := e.converse(context.Background(), c, w); err != nil {
		t.Fatal(err)
	}
	if ac.Load() != 2 {
		t.Fatal("新消息机会丢失或重复", ac.Load())
	}
}

// 个人行动在恢复后读到停止指示，成员通过已有共享写入终止本人的行动；
// 同批的旧发送不会执行，已经保存的成果不删除。
func TestPersonalStopInstructionPreventsSameBatchPublication(t *testing.T) {
	s, c, m := fixture(t)
	p, _ := s.personalConversation(m.ID)
	w := addWork(t, s, p, m, "explore")
	s.Claim(w.ID)
	msg := userMessage(c, "我让你停下来，这件事不做了")
	s.AddUserMessage(msg, time.Hour)
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if !strings.Contains(asJSON(ms), msg.Body) {
			t.Fatal("个人行动没看到停止指示")
		}
		return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
			toolCall("apply_instruction", map[string]any{"conversation_id": c.ID, "source_id": msg.ID, "stop_ids": []string{w.ID}}),
			toolCall("send_message", map[string]any{"conversation_id": c.ID, "message": "不该发出的旧成果"}),
		}}, nil
	}), testConfig())
	if _, err := e.memberAction(context.Background(), p, m, w); err != nil {
		t.Fatal(err)
	}
	var status string
	s.db.QueryRow(`SELECT status FROM work WHERE id=?`, w.ID).Scan(&status)
	msgs, _ := s.Messages(c.ID, 0, -1)
	if status != "stopped" || len(msgs) != 1 {
		t.Fatal(status, msgs)
	}
}
