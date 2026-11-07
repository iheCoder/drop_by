package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func groupWithMembers(t *testing.T, count int) (*Store, Conversation, []Member) {
	t.Helper()
	s, c, first := fixture(t)
	members := []Member{first}
	for i := 1; i < count; i++ {
		m := first
		m.ID = newID("member")
		m.Name = "同伴" + strings.Repeat("甲", i)
		if err := s.AddMember(m); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO participants VALUES(?,?)`, c.ID, m.ID); err != nil {
			t.Fatal(err)
		}
		members = append(members, m)
	}
	c, _ = s.Conversation(c.ID)
	return s, c, members
}

func latestChatWork(t *testing.T, s *Store, room string) Work {
	t.Helper()
	works, err := s.Works(room)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range works {
		if w.Kind == "chat" && w.Status == "pending" {
			return w
		}
	}
	t.Fatal("缺少真人消息产生的聊天机会")
	return Work{}
}

func TestConcurrentMembersSeePeersWithoutWaitingForSlowMember(t *testing.T) {
	// B 正在思考时 A 发来新消息；B 保留已形成的回复，C 的慢请求不形成队列屏障。
	s, c, members := groupWithMembers(t, 3)
	msg := userMessage(c, "这周很累，不太想再做计划。")
	s.AddUserMessage(msg, time.Hour)
	c, _ = s.Conversation(c.ID)
	aMaySend := make(chan struct{})
	bThinking := make(chan struct{})
	slowRelease := make(chan struct{})
	var bOnce sync.Once
	e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{}), nil
		}
		var input struct {
			Self     Member    `json:"self"`
			Messages []Message `json:"latest_messages"`
		}
		for _, entry := range ms {
			if entry.Role == "user" {
				raw := strings.TrimPrefix(entry.Content, "刚收到的新现场：")
				_ = json.Unmarshal([]byte(raw), &input)
			}
		}
		if testMemberSpoke(s, c.ID, input.Self.ID) {
			return jsonReply(Outcome{}), nil
		}
		switch input.Self.ID {
		case members[0].ID:
			select {
			case <-bThinking:
			case <-ctx.Done():
				return ChatMessage{}, ctx.Err()
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("send_message", map[string]string{"message": "像又添了一份周末作业。"})}}, nil
		case members[1].ID:
			bOnce.Do(func() { close(bThinking) })
			if !testMemberSpoke(s, c.ID, members[0].ID) {
				select {
				case <-aMaySend:
				case <-ctx.Done():
					return ChatMessage{}, ctx.Err()
				}
			}
			for _, message := range input.Messages {
				if message.AuthorID == members[0].ID {
					return jsonReply(Outcome{Message: "那就留一个随时能回家的选项。", ReplyTo: message.ID}), nil
				}
			}
			return jsonReply(Outcome{Message: "尚未读到同伴的旧草稿"}), nil
		default:
			select {
			case <-slowRelease:
				return ChatMessage{}, errors.New("C 网络失败")
			case <-ctx.Done():
				return ChatMessage{}, ctx.Err()
			}
		}
	}), testConfig())
	events, unsub := e.Hub.Subscribe()
	defer unsub()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.process(ctx, latestChatWork(t, s, c.ID)) }()
	for !testMemberSpoke(s, c.ID, members[0].ID) {
		select {
		case <-events:
		case <-ctx.Done():
			t.Fatal("A 被慢成员挡住")
		}
	}
	close(aMaySend)
	for !testMemberSpoke(s, c.ID, members[1].ID) {
		select {
		case <-events:
		case <-ctx.Done():
			t.Fatal("B 被慢成员挡住")
		}
	}
	close(slowRelease)
	if err := <-done; err == nil {
		t.Fatal("C 故障没有记录")
	}
	messages, _ := s.Messages(c.ID, 0, 20)
	if len(messages) != 3 || !strings.Contains(messages[2].Body, "旧草稿") {
		t.Fatalf("已形成的表达被新消息撤回: %+v", messages)
	}
}

func TestWithdrawalsDoNotConsumeOtherMembersMessageOpportunity(t *testing.T) {
	// 三个人读到上下文后撤回意图，第四个人仍然愿意说。即使公开消息上限为一，
	// 也不能把前三人的沉默算成三条消息，或在第一人安静时结束整群。
	s, c, members := groupWithMembers(t, 4)
	s.AddUserMessage(userMessage(c, "各位最近在想什么？"), time.Hour)
	cfg := testConfig()
	acted := []string{}
	var actedMu sync.Mutex
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{}), nil
		}
		var input struct {
			Self Member `json:"self"`
		}
		json.Unmarshal([]byte(ms[1].Content), &input)

		if testHasPublished(s, c.ID) {
			return jsonReply(Outcome{Outcome: "本测试成员已完成这次回应"}), nil
		}
		for i, m := range members {
			if m.ID == input.Self.ID {
				time.Sleep(time.Duration(i*10) * time.Millisecond)
			}
		}

		actedMu.Lock()
		acted = append(acted, input.Self.ID)
		actedMu.Unlock()
		if input.Self.ID != members[3].ID {
			return jsonReply(Outcome{Outcome: "想了想，先听其他人说。"}), nil
		}
		return jsonReply(Outcome{Message: "我在琢磨，有没有一种地图能只画让人想停留的地方。"}), nil
	}), cfg)
	if err := e.process(context.Background(), latestChatWork(t, s, c.ID)); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(acted) != 4 || len(messages) != 2 || messages[1].AuthorID != members[3].ID {
		t.Fatalf("沉默挤掉了其他人的机会: acted=%v messages=%+v", acted, messages)
	}
}

func TestFirstMemberMayResearchWithoutAnnouncingIt(t *testing.T) {
	// 真人刚开口，第一人判断先查资料，第二人想聊天。探索不能被强改为 speak，
	// 也不能以“全群在等研究”为由挡住第二人的发言。
	s, c, members := groupWithMembers(t, 2)
	s.AddUserMessage(userMessage(c, "为什么纸飞机有时会突然扎头？"), time.Hour)
	cfg := testConfig()
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{}), nil
		}
		var input struct {
			Self Member `json:"self"`
		}
		json.Unmarshal([]byte(ms[1].Content), &input)

		if testHasPublished(s, c.ID) {
			return jsonReply(Outcome{Outcome: "本测试成员已完成这次回应"}), nil
		}
		if input.Self.ID == members[0].ID {
			if len(ms) == 2 {
				return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("schedule_exploration", map[string]any{"instruction": "查找纸飞机失速与重心的原始资料", "delay_seconds": 60})}}, nil
			}
			return jsonReply(Outcome{}), nil
		}
		time.Sleep(10 * time.Millisecond)

		if input.Self.ID == members[0].ID {
			t.Error("安静研究被强制改成了公开发言")
		}
		return jsonReply(Outcome{Message: "我会先怀疑重心位置，不过还只是个猜想。"}), nil
	}), cfg)
	if err := e.process(context.Background(), latestChatWork(t, s, c.ID)); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	var research int
	s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='research' AND member_id=?`, members[0].ID).Scan(&research)
	if research != 1 || len(messages) != 2 || messages[1].AuthorID != members[1].ID {
		t.Fatalf("安静研究与同伴交流未同时成立: research=%d messages=%+v", research, messages)
	}
}

func TestObservationAndStateRespectRevisionAndConversation(t *testing.T) {
	// 阅读位置与持续关注属于真实处理过的那份上下文。新真人消息到来后，旧结果
	// 不能覆盖它；另一条私聊的消息也不能被拿来支撑本群的公开状态。
	s, c, m := fixture(t)
	msg := userMessage(c, "我希望周末能轻松一点。")
	s.AddUserMessage(msg, time.Hour)
	c, _ = s.Conversation(c.ID)
	messages, _ := s.Messages(c.ID, 0, 10)
	seq := messages[0].Seq
	state := "正在想怎样让周末更省心。"
	if err := s.recordObservation(c, m.ID, seq, &state, []string{msg.ID}); err != nil {
		t.Fatal(err)
	}
	other, _ := s.CreateConversation("私聊", "direct", []string{m.ID}, time.Hour)
	secret := userMessage(other, "另一个场景的内容")
	s.AddUserMessage(secret, time.Hour)
	unsupported := "不应由其他场景支撑的状态"
	if err := s.recordObservation(c, m.ID, seq, &unsupported, []string{secret.ID}); err != nil {
		t.Fatal(err)
	}
	_, saved, _ := s.observation(c.ID, m.ID)
	if saved.Body != state {
		t.Fatal("跨场景来源覆盖了当前群状态")
	}
	if _, err := s.searchHistory(c.ID, "", secret.ID, 0, 0); err == nil {
		t.Fatal("可以选择另一个场景的消息")
	}
	s.AddUserMessage(userMessage(c, "暂停讨论"), time.Hour)
	if err := s.recordObservation(c, m.ID, seq, &unsupported, []string{msg.ID}); !errors.Is(err, ErrStale) {
		t.Fatalf("过时判断未拦截: %v", err)
	}
	c, _ = s.Conversation(c.ID)
	empty := ""
	if err := s.recordObservation(c, m.ID, seq, &empty, nil); err != nil {
		t.Fatal(err)
	}
	states, _ := s.MemberStates(c.ID)
	if len(states) != 0 {
		t.Fatalf("过时关注无法自然清除: %+v", states)
	}
}

func TestAllPassPersistsObservationAcrossRestartAndIsNotFailure(t *testing.T) {
	// 全员不说话是正常结果，并不等于未处理消息。重启后仍知道读到了哪里、
	// 在意什么；下一次 API 失败则应明确返回错误，不能伪装成性格安静。
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := Member{ID: "member_quiet", Name: "慢慢", Profile: "习惯先听，只有在意时才开口。"}
	if err = s.AddMember(m); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation("安静的群", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	msg := userMessage(c, "今天不需要建议，我只是想自己坐一会儿。")
	if err = s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	state := "在意留出不必回应的空间。"
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{}), nil
		}
		return jsonReply(Outcome{Outcome: "对方现在希望有一点空间", State: &state, StateSourceIDs: []string{msg.ID}}), nil
	}), testConfig())
	w := latestChatWork(t, s, c.ID)
	if err = e.process(context.Background(), w); err != nil {
		t.Fatalf("全员 pass 被当作失败: %v", err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) != 1 {
		t.Fatalf("安静时生成了多余消息: %+v", messages)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seen, restored, err := s.observation(c.ID, m.ID)
	if err != nil || seen != messages[0].Seq || restored.Body != state {
		t.Fatalf("重启丢失观察记录: %d %+v %v", seen, restored, err)
	}
	c, _ = s.Conversation(c.ID)
	modelErr := errors.New("模型服务暂时不可用")
	e = NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		return ChatMessage{}, modelErr
	}), testConfig())
	if _, err = e.act(context.Background(), c, m, w, ""); !errors.Is(err, modelErr) {
		t.Fatalf("判断失败被误当作自愿安静: %v", err)
	}
}

func TestQuestionToolIsARealContributionAndKeepsQuestionContext(t *testing.T) {
	// 提问工具本身会发布一条聊天消息。最终 message 为空表示避免重复问题，
	// 不能登记为保持安静；后来引用该消息时也必须看到问题而不只是欢迎语。
	s, c, m := fixture(t)
	if err := s.AddUserMessage(userMessage(c, "我们可以聊聊以后怎么相处。"), time.Hour); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(ms[0].Content, "你只解释") {
			return jsonReply(Control{}), nil
		}

		if testHasPublished(s, c.ID) {
			return jsonReply(Outcome{Outcome: "本测试成员已完成这次回应"}), nil
		}

		if len(ms) == 2 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("ask_question", map[string]any{"text": "你喜欢我怎样分享新的想法？", "options": []string{"直接说出来", "先听我说完"}})}}, nil
		}
		return jsonReply(Outcome{}), nil
	}), cfg)
	if err := e.process(context.Background(), latestChatWork(t, s, c.ID)); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) != 2 || messages[1].AuthorID != m.ID || messages[1].Question == nil {
		t.Fatalf("真实问题没有成为唯一回复: %+v", messages)
	}
	selected, err := s.searchHistory(c.ID, "", messages[1].ID, 0, 0)
	if err != nil || !strings.Contains(asJSON(selected), "你喜欢我怎样分享新的想法") {
		t.Fatalf("较早问题不能回查: %v", err)
	}

	decisions, err := s.Participations(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decisions {
		if d.Selected && d.Action == "pass" {
			t.Fatal("已经发出的问题被误登记为沉默")
		}
	}
}

func TestProfileEditRetriesButOrdinaryHumanDoesNotReplaceRunningChat(t *testing.T) {
	// 在模型已经读到旧人物说明、尚未发布时修改人物。旧稿必须丢弃，原用户问题
	// 仍应立即用新身份重新处理。对照场景是普通真人补充：正在形成的回复继续。
	for _, change := range []string{"profile", "profile_then_budget", "new_human"} {
		t.Run(change, func(t *testing.T) {
			s, c, m := fixture(t)
			firstHuman := userMessage(c, "聊聊海边吧")
			profileChanged := strings.HasPrefix(change, "profile")
			if profileChanged {
				firstHuman.Body = "以后说话严肃一点，然后聊聊海边吧"
			}
			if err := s.AddUserMessage(firstHuman, time.Hour); err != nil {
				t.Fatal(err)
			}
			originalWork := latestChatWork(t, s, c.ID)
			started, release := make(chan struct{}), make(chan struct{})
			var acts atomic.Int32
			var controls atomic.Int32
			cfg := testConfig()
			e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
				if strings.HasPrefix(ms[0].Content, "你只解释") {
					controlCall := controls.Add(1)
					if change == "profile_then_budget" && controlCall == 2 {
						return ChatMessage{}, ErrBudget
					}
					if profileChanged {
						// 模拟模型始终重放原消息里的旧偏好，执行边界也必须保护
						// 更晚的人工编辑；不能仅靠模型自觉理解设置的先后关系。
						return jsonReply(Control{PreferenceUpdates: []PreferenceUpdate{{MemberID: m.ID, Profile: "现在认真严肃地交流", SourceID: firstHuman.ID}}}), nil
					}
					return jsonReply(Control{}), nil
				}

				if testHasPublished(s, c.ID) {
					return jsonReply(Outcome{Outcome: "本测试成员已完成这次回应"}), nil
				}

				var input struct {
					Self    Member `json:"self"`
					Trigger Work   `json:"trigger"`
				}
				if err := json.Unmarshal([]byte(ms[1].Content), &input); err != nil {
					return ChatMessage{}, err
				}
				if acts.Add(1) == 1 {
					close(started)
					<-release // 故意模拟无法立即取消的网络返回，最后仍须通过修订检查。
					return jsonReply(Outcome{Message: "这是一份已经过时的旧人物回复。"}), nil
				}
				return jsonReply(Outcome{Message: input.Self.Profile + "；" + input.Trigger.Instruction}), nil
			}), cfg)
			e.dispatch(context.Background())
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("模型没有进入待发布阶段")
			}
			if profileChanged {
				request := httptest.NewRequest("PATCH", "http://localhost/api/members/"+m.ID, strings.NewReader(`{"profile":"现在更喜欢用诗意和想象来交流"}`))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				NewHandler(e).ServeHTTP(response, request)
				if response.Code != 200 {
					t.Fatalf("无法修改人物: %d %s", response.Code, response.Body.String())
				}
			} else {
				if err := e.Submit(userMessage(c, "改聊山里的雨吧")); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			e.wg.Wait()
			var status string
			if err := s.db.QueryRow(`SELECT status FROM work WHERE id=?`, originalWork.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if profileChanged && status != "pending" {
				t.Fatalf("人物修改丢掉了用户原问题: %s", status)
			}
			if change == "new_human" && status != "done" {
				t.Fatalf("普通补充不应替代正在完成的行动: %s", status)
			}
			// 不依赖定时器实等一秒，让已经安排的重试/新消息立即进入下一次调度。
			if _, err := s.db.Exec(`UPDATE work SET due_at=0 WHERE status='pending'`); err != nil {
				t.Fatal(err)
			}
			e.dispatch(context.Background())
			e.wg.Wait()
			if change == "profile_then_budget" {
				if err := s.db.QueryRow(`SELECT status FROM work WHERE id=?`, originalWork.ID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != "waiting_budget" {
					t.Fatalf("未保留预算等待: %s", status)
				}
				if _, err := s.db.Exec(`UPDATE work SET due_at=0 WHERE id=?`, originalWork.ID); err != nil {
					t.Fatal(err)
				}
				e.dispatch(context.Background())
				e.wg.Wait()
			}
			messages, err := s.Messages(c.ID, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			var replies []Message
			for _, message := range messages {
				if message.AuthorID == m.ID {
					replies = append(replies, message)
				}
			}
			if len(replies) != 1 || (profileChanged && strings.Contains(replies[0].Body, "过时")) {
				t.Fatalf("旧稿发布或最新回复缺失: %+v", replies)
			}
			if profileChanged && (!strings.Contains(replies[0].Body, "诗意") || !strings.Contains(replies[0].Body, "海边")) {
				t.Fatalf("未用新人物接原话: %s", replies[0].Body)
			}
			if change == "new_human" && !strings.Contains(replies[0].Body, "旧人物回复") {
				t.Fatalf("没有转向最新真人消息: %s", replies[0].Body)
			}
		})
	}
}

func TestRevisionChangesKeepOpportunityUntilUserStops(t *testing.T) {
	// 自主观察/成果分享也会被人物修改影响，不能直接丢掉；若设定连续改变，
	// 每次以最新设置重新考虑，不因累计次数丢掉；真人停止仍可立即替代旧机会。
	for _, kind := range []string{"explore", "share"} {
		t.Run(kind, func(t *testing.T) {
			s, c, m := fixture(t)
			w := addWork(t, s, c, m, kind)
			e := NewEngine(s, modelFunc(func(_ context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
				if _, err := s.db.Exec(`UPDATE conversations SET revision=revision+1 WHERE id=?`, c.ID); err != nil {
					return ChatMessage{}, err
				}
				return jsonReply(Outcome{Outcome: "当前先听听"}), nil
			}), testConfig())
			for attempt := 1; attempt <= 3; attempt++ {
				e.dispatch(context.Background())
				e.wg.Wait()
				var status string
				if err := s.db.QueryRow(`SELECT status FROM work WHERE id=?`, w.ID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				want := "pending"
				if status != want {
					t.Fatalf("第 %d 次修订变化后状态=%s，预期 %s", attempt, status, want)
				}
				if _, err := s.db.Exec(`UPDATE work SET due_at=0 WHERE id=?`, w.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Submit(userMessage(c, "先别聊")); err != nil {
				t.Fatal(err)
			}
			stopped, err := s.ReadWork(c.ID, w.ID)
			if err != nil || stopped.Status != "superseded" {
				t.Fatalf("用户停止没有替代旧机会: %+v %v", stopped, err)
			}

		})
	}
}

func TestBudgetResumeStillAppliesUnprocessedHumanPreference(t *testing.T) {
	// attempts 不是“已经理解过偏好”的证据：首次控制判断就用完预算时，恢复后
	// 仍须记住原消息里的明确设定，不能把所有重试都当成人物修改后的重放。
	s, c, m := fixture(t)
	msg := userMessage(c, "以后更幽默一点吧")
	if err := s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	w.Attempts, w.Result = 1, ErrBudget.Error()
	c, _ = s.Conversation(c.ID)
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		return jsonReply(Control{PreferenceUpdates: []PreferenceUpdate{{MemberID: m.ID, Profile: "喜欢自然幽默地交流", SourceID: msg.ID}}}), nil
	}), testConfig())
	if err := e.applyControl(context.Background(), c, w); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Member(m.ID)
	if err != nil || saved.Profile != "喜欢自然幽默地交流" {
		t.Fatalf("预算恢复错误跳过了用户偏好: %+v %v", saved, err)
	}
}

func testMemberSpoke(s *Store, room, member string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id=? AND author_id=?`, room, member).Scan(&n)
	return n > 0
}
