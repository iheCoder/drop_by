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

// 后台测试使用真实消息、来源登记、持久工作和提交路径；模型替身只让并发时序可控。
func reflectionFixture(t *testing.T, s *Store, c Conversation, m Member) (Work, Message) {
	t.Helper()
	msg := userMessage(c, "刚才的试验里，大家能自己接着做，比做得整齐更让我意外。")
	if err := s.Publish(msg, c.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.recordProfileMessages(m.ID, []Message{msg}); err != nil {
		t.Fatal(err)
	}
	if err := s.scheduleProfileReflections(time.Hour); err != nil {
		t.Fatal(err)
	}
	works, err := s.DueWorks()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range works {
		if w.Kind == "reflect" && w.MemberID == m.ID {
			return w, msg
		}
	}
	t.Fatal("实际新经历没有安排后台整理")
	return Work{}, Message{}
}

func TestProfileReflectionWaitsOnlyForCommitAndKeepsProposal(t *testing.T) {
	// 模型尚未完成时，前台仍能执行。整理完成但前台占用成员时，只延后提交，
	// 重试复用已保存的结果，既不打断任务也不再付一次整理调用。
	s, c, m := fixture(t)
	w, source := reflectionFixture(t, s, c, m)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	newProfile := "我喜欢通过小实验理解问题，也更在意别人能否自然接上自己的行动。"
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, defs []ToolDefinition, _ bool) (ChatMessage, error) {
		if !strings.Contains(ms[0].Content, profileSelfPrompt) {
			return jsonReply(Outcome{Outcome: "前台正常完成"}), nil
		}
		calls.Add(1)
		for _, def := range defs {
			if !reflectionReadTool(def.Function.Name) {
				t.Error("后台出现行动工具", def.Function.Name)
			}
		}
		close(started)
		<-release
		return jsonReply(profileUpdate{Profile: &newProfile, SourceIDs: []string{source.ID}, ChangeNote: "更关注行动如何被接续"}), nil
	}), testConfig())
	finished := make(chan error, 1)
	go func() { finished <- e.reflectProfile(context.Background(), w) }()
	<-started
	// 后台生成期间调用真实前台行动，短期限只用于检出被锁住的测试故障。
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	front := addWork(t, s, c, m, "followup")
	if _, err := e.act(ctx, c, m, front, "接着聊"); err != nil {
		t.Fatal("后台阻塞了前台", err)
	}
	unlock, err := e.lockMember(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-finished; !errors.Is(err, errActionYield) {
		unlock()
		t.Fatal("忙碌时没有延后提交", err)
	}
	current, _ := s.Member(m.ID)
	if current.Profile != m.Profile {
		t.Fatal("任务中途更换了特质")
	}
	unlock()
	w.Result, err = s.reflectionResult(w.ID)
	if err != nil || w.Result == "" {
		t.Fatal("整理结果没有保存", err)
	}
	if err := e.reflectProfile(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	current, _ = s.Member(m.ID)
	if current.Profile != newProfile || current.ProfileRevision != 1 || calls.Load() != 1 {
		t.Fatal("提交丢失或重复调用", current, calls.Load())
	}
	room, _ := s.Conversation(c.ID)
	if room.Revision != c.Revision {
		t.Fatal("后台修改取消了正在进行的房间任务")
	}
	changes, _ := s.profileHistory(m.ID)
	if len(changes) != 2 || changes[0].Origin != "reflection" || changes[0].SourceIDs[0] != source.ID {
		t.Fatal("变化没有保留真实来源", changes)
	}
}

func TestProfileDispatcherYieldsToIncomingChat(t *testing.T) {
	// 真实调度器启动整理后到达新聊天：取消后台 HTTP 上下文，但不等待它
	// 清理完毕；前台完成且人物不变，后台工作仍可在下一次空闲时恢复。
	s, c, m := fixture(t)
	w, _ := reflectionFixture(t, s, c, m)
	started, canceled := make(chan struct{}), make(chan struct{})
	e := NewEngine(s, modelFunc(func(ctx context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.Contains(ms[0].Content, profileSelfPrompt) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ChatMessage{}, ctx.Err()
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	e.dispatch(ctx)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("后台没有启动")
	}
	if err := e.Submit(userMessage(c, "先聊眼前这件事")); err != nil {
		t.Fatal(err)
	}
	e.dispatch(ctx)
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("前台没有取消后台生成")
	}
	e.wg.Wait()
	current, _ := s.Member(m.ID)
	if current.Profile != m.Profile {
		t.Fatal("取消的整理仍然修改了人格")
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM work WHERE id=?`, w.ID).Scan(&status); err != nil || status != "pending" {
		t.Fatal("后台没有保留续接机会", status, err)
	}
	var completed int
	if err := s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='chat' AND status='done'`).Scan(&completed); err != nil || completed != 1 {
		t.Fatal("前台没有完成", completed, err)
	}
}

func TestProfileReflectionRebasesAfterHumanEdit(t *testing.T) {
	// 先完成但暂未提交的后台结果不能覆盖用户后来的编辑，且不得把旧结果
	// 合并回新人物。下一次整理重新读取当前说明和这批尚未消费的经历。
	s, c, m := fixture(t)
	w, _ := reflectionFixture(t, s, c, m)
	evidence, through, err := s.profileReflectionInput(m)
	if err != nil {
		t.Fatal(err)
	}
	stale := "这是后台基于旧说明形成的结果"
	w.Result = asJSON(profileReflection{Revision: 0, ThroughSeq: through, Update: profileUpdate{Profile: &stale}})
	e := NewEngine(s, nil, testConfig())
	edited := "我说话直接，喜欢热闹，也愿意自己试试。"
	response := preferenceRequest(NewHandler(e), "PATCH", "/api/members/"+m.ID, map[string]any{"profile": edited, "profile_revision": 0})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	calls := 0
	e.Model = modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if !strings.Contains(ms[1].Content, edited) || strings.Contains(ms[1].Content, stale) || !strings.Contains(ms[1].Content, evidence[0].ID) {
			t.Error("没有基于新设定重新理解")
		}
		return jsonReply(profileUpdate{ChangeNote: "本次经历没有改变我"}), nil
	})
	if err := e.reflectProfile(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Member(m.ID)
	if calls != 1 || current.Profile != edited || current.ProfileRevision != 1 {
		t.Fatal("迟到结果覆盖用户编辑")
	}
	response = preferenceRequest(NewHandler(e), "PATCH", "/api/members/"+m.ID, map[string]any{"profile": stale, "profile_revision": 0})
	if response.Code != 409 {
		t.Fatal("旧编辑页面覆盖了新版本", response.Code)
	}
}

func TestProfileExperiencesAreDeliveredSourcesAndConsumedOnce(t *testing.T) {
	// 同一条消息多次回读不变成多条独立经历；从未交付的群消息不会凭空
	// 进入此成员的自我整理。失败工具仍保留失败这一真实状态。
	s, c, m := fixture(t)
	w, source := reflectionFixture(t, s, c, m)
	unseen := userMessage(c, "这条是还没看见的消息")
	if err := s.Publish(unseen, c.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.recordProfileMessages(m.ID, []Message{source, source}); err != nil {
		t.Fatal(err)
	}
	run := ToolRun{ID: "failed_experiment", WorkID: w.ID, ConversationID: c.ID, MemberID: m.ID, Name: "read_page", Arguments: json.RawMessage(`{}`), Result: "网络失败，没有取得正文", Status: "failed", CreatedAt: now()}
	if err := s.RecordTool(run); err != nil {
		t.Fatal(err)
	}
	if err := s.recordProfileTool(m.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	evidence, _, err := s.profileReflectionInput(m)
	if err != nil || len(evidence) != 2 || evidence[1].Kind != "tool:read_page:failed" {
		t.Fatal("来源重复或状态失真", evidence, err)
	}
	for _, item := range evidence {
		if item.ID == unseen.ID {
			t.Fatal("把未读消息当作已经历")
		}
	}
	e := NewEngine(s, modelFunc(func(_ context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		return jsonReply(profileUpdate{}), nil
	}), testConfig())
	if err := e.reflectProfile(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	// 没变化也推进游标，避免每个时钟重复整理同一材料。
	if _, err := s.db.Exec(`UPDATE profile_reflections SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	if err := s.scheduleProfileReflections(time.Hour); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM work WHERE kind='reflect'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("无新经历仍重复安排", count, err)
	}
	current, _ := s.Member(m.ID)
	if current.Profile != m.Profile {
		t.Fatal("无新认识仍重写人物")
	}
}

func TestCurrentProfileAppearsOnceAndSurvivesOrganization(t *testing.T) {
	// 明确修改与整理在同批发生时，下一次模型调用必须只看到新说明；
	// 旧说明不能从行动开始时的场景缓存恢复出来。
	s, c, m := fixture(t)
	source := userMessage(c, "请把你本身改得外向一点，其他保留。")
	if err := s.AddUserMessage(source, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := addWork(t, s, c, m, "followup")
	w.SourceID = source.ID
	newProfile := "我喜欢寻找原始材料，也愿意改变判断，平时乐于主动与人交流。"
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		raw := asJSON(ms)
		if calls == 1 {
			if strings.Count(raw, m.Profile) != 1 {
				t.Error("人物说明没有保持单一来源", raw)
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
				toolCall("apply_instruction", map[string]any{"source_id": source.ID, "preference_updates": []PreferenceUpdate{{MemberID: m.ID, Profile: newProfile, SourceID: source.ID}}}),
				toolCall("organize_context", map[string]any{"note": "用户刚刚调整了我的特质，继续聊。"}),
			}}, nil
		}
		if strings.Contains(raw, m.Profile) || !strings.Contains(raw, newProfile) {
			t.Error("整理恢复了旧人物", raw)
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	if _, err := e.act(context.Background(), c, m, w, "继续交流"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("意外额外调用", calls)
	}
}

func TestProfileChangeReconsidersUnexecutedCheckpoint(t *testing.T) {
	// 故障现场可能还包含旧人格生成、但尚未执行的发言。后台更新后恢复
	// 必须让模型重新决定；已经执行的回执仍沿用现有幂等恢复机制。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	cp := actionCheckpoint{ID: newID("msg"), WorkID: w.ID, MemberID: m.ID, Revision: c.Revision,
		Messages: []ChatMessage{{Role: "system", Content: "old"}, {Role: "assistant", ToolCalls: []ToolCall{toolCall("send_message", map[string]any{"message": "旧人物未经执行的发言"})}}}}
	if err := s.saveAction(cp); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setMemberProfile(tx, &m, "我现在喜欢热烈地交流。", "reflection", "关注发生了变化", nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if !strings.Contains(asJSON(ms), m.Profile) {
			t.Error("没有刷新人物")
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	if _, err := e.act(context.Background(), c, m, w, "继续"); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(c.ID, 0, -1)
	if len(messages) != 0 {
		t.Fatal("恢复执行了旧人物的草稿", messages)
	}
}

func TestResumedProfileEditReplacesPendingScene(t *testing.T) {
	// 模型已经生成修改指令、尚未来得及执行就退出；恢复工具批次后，原先
	// 预备补交的旧场景不能再覆盖刚刚更新的说明。
	s, c, m := fixture(t)
	source := userMessage(c, "请让你本身更外向一点。")
	if err := s.AddUserMessage(source, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	updated := "我喜欢寻找原始材料，也乐于主动与人交流。"
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		var scene struct {
			Self Member `json:"self"`
		}
		if err := json.Unmarshal([]byte(ms[1].Content), &scene); err != nil || scene.Self.Profile != updated {
			t.Error("恢复工具后仍使用旧场景", err, scene.Self.Profile)
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	input, err := e.contextFor(c, m, w)
	if err != nil {
		t.Fatal(err)
	}
	cp := actionCheckpoint{ID: newID("msg"), WorkID: w.ID, MemberID: m.ID, Revision: c.Revision, Seen: observedThrough(input), Messages: []ChatMessage{
		{Role: "system", Content: "旧运行规则"}, {Role: "user", Content: asJSON(input)},
		{Role: "assistant", ToolCalls: []ToolCall{toolCall("apply_instruction", map[string]any{"source_id": source.ID, "preference_updates": []PreferenceUpdate{{MemberID: m.ID, Profile: updated, SourceID: source.ID}}})}},
	}}
	if err := s.saveAction(cp); err != nil {
		t.Fatal(err)
	}
	if _, err := e.act(context.Background(), c, m, w, "接续交流"); err != nil {
		t.Fatal(err)
	}
}

func TestProfilePersistsAcrossRestartAndNewRoom(t *testing.T) {
	// 使用实际关闭/重开数据库验证生成结果与版本，迁移重跑不重新抽取性格。
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := Member{ID: "restart_person", Name: "小石", Profile: "我喜欢慢慢琢磨，偶尔说个冷笑话。"}
	if err := s.AddMember(m); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.CreateConversation("新群", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Member(m.ID)
	if err != nil || saved.Profile != m.Profile {
		t.Fatal("重启丢失人物", err)
	}
	e := NewEngine(s, nil, testConfig())
	input, err := e.contextFor(c, saved, Work{})
	if err != nil || input["self"].(map[string]any)["profile"] != m.Profile {
		t.Fatal("换群后人物不连续", err)
	}
	history, _ := s.profileHistory(m.ID)
	if len(history) != 1 {
		t.Fatal("重启产生了多份人物", history)
	}
}

// 使用明确的空更新验证问答没有改动时仍保留工具与人物，不把初始化方向重放。
func TestProfileAnswerKeepsCurrentIdentityAndTools(t *testing.T) {
	m := Member{Name: "测试", Profile: "我喜欢有趣的东西，也爱安静地观察。", Direction: "已经被修改的旧方向", Tools: []string{"save_artifact"}, Answers: []string{"比如 Grok bot，我也说不清原因"}, Questions: []Question{{Text: "你希望我怎样有趣？"}}}
	model := modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.Contains(ms[1].Content, m.Direction) {
			t.Error("旧方向重新覆盖当前资料")
		}
		return jsonReply(profileUpdate{}), nil
	})
	updated, err := CompleteMember(context.Background(), model, m)
	if err != nil || updated.Profile != m.Profile || asJSON(updated.Tools) != asJSON(m.Tools) {
		t.Fatal("空更新改写了身份", err)
	}
}
