package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunningCoordinationReviewMovesToNewCoordinator(t *testing.T) {
	// 回报已在旧协调者的模型请求中时换人。旧请求即使正常返回也不能发言或
	// 把工作改回旧成员；同一条回报应留给新协调者判断，不额外制造通知。
	s, c, members := groupWithMembers(t, 3)
	c, err := s.SetCoordinator(c, members[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	root := addWork(t, s, c, members[0], "explore")
	assigned, err := s.DelegateWork(c, members[0], root, members[1].ID, "核对一个事实", "你关心证据")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE work SET status='done' WHERE id IN (?,?)`, root.ID, assigned.ID); err != nil {
			return err
		}
		return queueCoordinationReview(tx, assigned, "done", "核对已完成")
	}); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var oldCalls, newCalls atomic.Int32
	e := NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		var input struct {
			Self Member `json:"self"`
		}
		json.Unmarshal([]byte(messages[1].Content), &input)
		if !strings.Contains(messages[0].Content, "此时只决定自己的下一步") {
			return ChatMessage{}, errors.New("old coordinator's stale decision reached an action")
		}
		if input.Self.ID == members[0].ID {
			oldCalls.Add(1)
			close(started)
			<-release
			return jsonReply(Decision{Action: "speak", Intent: "安排下一位", Reason: "这是更换职责之前的想法"}), nil
		}
		if input.Self.ID == members[2].ID {
			newCalls.Add(1)
			return jsonReply(Decision{Action: "pass", Reason: "已有成果够用了，先不安排下一步"}), nil
		}
		return ChatMessage{}, errors.New("completion review woke a non-coordinator")
	}), testConfig())
	e.dispatch(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("old coordinator did not begin review")
	}
	response := preferenceRequest(NewHandler(e), "PATCH", "/api/conversations/"+c.ID+"/coordinator", map[string]string{"member_id": members[2].ID})
	close(release)
	e.wg.Wait()
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	review, err := s.ReadWork(c.ID, "coordination_"+assigned.ID)
	if err != nil || review.MemberID != members[2].ID || review.Status != "pending" {
		t.Fatalf("old execution overwrote reassignment: %+v %v", review, err)
	}
	e.dispatch(context.Background())
	e.wg.Wait()
	review, _ = s.ReadWork(c.ID, review.ID)
	if review.Status != "done" || oldCalls.Load() != 1 || newCalls.Load() != 1 {
		t.Fatalf("review did not move once to new role: %+v old=%d new=%d", review, oldCalls.Load(), newCalls.Load())
	}
	messages, _ := s.Messages(c.ID, 0, 20)
	if len(messages) != 0 {
		t.Fatalf("stale coordinator posted after replacement: %+v", messages)
	}
}

func TestCoordinatorChangeDuringControlKeepsUnprocessedPreference(t *testing.T) {
	// 真人刚提出长期偏好，控制模型还未返回时用户改了群协调者。旧控制结果
	// 因修订号失效而重读，但这次并没有更晚的人物编辑，原偏好不能被跳过。
	s, c, members := groupWithMembers(t, 2)
	msg := userMessage(c, "阿苇，以后表达更幽默一点吧。也聊聊今天的雨。")
	if err := s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	original := latestChatWork(t, s, c.ID)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	cfg := testConfig()
	cfg.MaxTurns = 1
	e := NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(messages[0].Content, "你只解释") {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			} else {
				var input struct {
					IdentityUpdated bool `json:"identity_updated_since_trigger"`
				}
				json.Unmarshal([]byte(messages[1].Content), &input)
				if input.IdentityUpdated {
					return ChatMessage{}, errors.New("coordinator edit was mistaken for a newer profile edit")
				}
			}
			return jsonReply(Control{Discussion: "keep", Research: "keep", PreferenceUpdates: []PreferenceUpdate{{MemberID: members[0].ID, Profile: "愿意核对来源，也喜欢幽默地表达", SourceID: msg.ID}}}), nil
		}
		var input struct {
			Self Member `json:"self"`
		}
		json.Unmarshal([]byte(messages[1].Content), &input)
		if strings.Contains(messages[0].Content, "此时只决定自己的下一步") {
			if input.Self.ID == members[0].ID {
				return jsonReply(Decision{Action: "speak", Intent: "按新偏好接今天的雨", Reason: "这句话在对自己说"}), nil
			}
			return jsonReply(Decision{Action: "pass", Reason: "先听同伴回应"}), nil
		}
		return jsonReply(Outcome{Message: "记住了，我会" + input.Self.Profile + "。这场雨像给城市按了慢放。"}), nil
	}), cfg)
	e.dispatch(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("control request never began")
	}
	response := preferenceRequest(NewHandler(e), "PATCH", "/api/conversations/"+c.ID+"/coordinator", map[string]string{"member_id": members[1].ID})
	close(release)
	e.wg.Wait()
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	retry, err := s.ReadWork(c.ID, original.ID)
	if err != nil || retry.Status != "pending" || strings.HasPrefix(retry.Result, identityRevisionRetry) {
		t.Fatalf("ordinary role change lost chat or falsely suppressed preference: %+v %v", retry, err)
	}
	if _, err := s.db.Exec(`UPDATE work SET due_at=0 WHERE id=?`, original.ID); err != nil {
		t.Fatal(err)
	}
	e.dispatch(context.Background())
	e.wg.Wait()
	saved, err := s.Member(members[0].ID)
	if err != nil || !strings.Contains(saved.Profile, "幽默") {
		t.Fatalf("unprocessed human preference was lost: %+v %v", saved, err)
	}
	messages, _ := s.Messages(c.ID, 0, 20)
	if len(messages) != 2 || messages[1].AuthorID != members[0].ID || !strings.Contains(messages[1].Body, "幽默") {
		t.Fatalf("original human request did not receive the updated reply: %+v", messages)
	}
}

func TestProfileEditBeforeBudgetFailureKeepsLaterPreference(t *testing.T) {
	// 模型请求因预算失败，没有机会触发 ErrStale。资料编辑事务已写下的较晚
	// 偏好事实仍必须在错误落库后保留；恢复预算不能重放原消息里的旧人物要求。
	s, c, m := fixture(t)
	msg := userMessage(c, "以后严肃一点，然后聊聊雨吧")
	if err := s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e := NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(messages[0].Content, "你只解释") {
			if calls.Add(1) == 1 {
				close(started)
				<-release
				return ChatMessage{}, ErrBudget
			}
			return jsonReply(Control{PreferenceUpdates: []PreferenceUpdate{{MemberID: m.ID, Profile: "严肃地表达", SourceID: msg.ID}}}), nil
		}
		return jsonReply(Decision{Action: "pass", Reason: "这次先安静听"}), nil
	}), testConfig())
	e.dispatch(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("control request never began")
	}
	response := preferenceRequest(NewHandler(e), "PATCH", "/api/members/"+m.ID, map[string]string{"profile": "现在更喜欢用诗意和想象交流"})
	close(release)
	e.wg.Wait()
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	stored, err := s.ReadWork(c.ID, w.ID)
	if err != nil || stored.Status != "waiting_budget" || !strings.HasPrefix(stored.Result, identityRevisionRetry) {
		t.Fatalf("budget failure erased newer edit provenance: %+v %v", stored, err)
	}
	if _, err := s.db.Exec(`UPDATE work SET due_at=0 WHERE id=?`, w.ID); err != nil {
		t.Fatal(err)
	}
	e.dispatch(context.Background())
	e.wg.Wait()
	member, err := s.Member(m.ID)
	if err != nil || member.Profile != "现在更喜欢用诗意和想象交流" {
		t.Fatalf("budget recovery replayed older preference: %+v %v", member, err)
	}
}
