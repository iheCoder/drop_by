package dropby

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 升级真实旧库时，未完的后台改写不能在启动恢复后继续运行。历史人物版本、
// 当下用户可见的人物和原始经历都保留；删除的只是旧功能的队列引用与游标。
func TestPersonalityUpgradeStopsReflectionAndKeepsSources(t *testing.T) {
	s, c, m := fixture(t)
	source := userMessage(c, "大家喜欢极简，我仍然喜欢丰富的细节。")
	if err := s.Publish(source, c.Revision); err != nil {
		t.Fatal(err)
	}
	var failedID string
	for _, status := range []string{"pending", "running", "waiting_budget", "paused", "failed", "done"} {
		w := addWork(t, s, c, m, "reflect")
		if status == "failed" {
			failedID = w.ID
		}
		if _, err := s.db.Exec(`UPDATE work SET status=?,result='旧版整理记录' WHERE id=?`, status, w.ID); err != nil {
			t.Fatal(err)
		}
	}
	recordWork := addWork(t, s, c, m, "research")
	if err := s.RecordTool(ToolRun{ID: "original_read", WorkID: recordWork.ID, MemberID: m.ID, ConversationID: c.ID, Name: "read_page", Arguments: json.RawMessage(`{}`), Status: "succeeded", Result: `{"content":"完整资料末尾"}`, CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE profile_experiences(member_id TEXT,source_id TEXT);
CREATE TABLE profile_reflections(member_id TEXT,after_seq INTEGER,next_at INTEGER);
INSERT INTO profile_experiences VALUES(?,?);
INSERT INTO profile_reflections VALUES(?,0,0);`, m.ID, source.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = setMemberProfile(tx, &m, "我重视鲜活的细节，也有自己的判断。", m.DomainProfiles, "reflection", "旧版产生的版本", []string{source.ID}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// 旧数据库的人物历史只有正文。移除新列后走真实启动迁移，再读原有快照。
	if _, err := s.db.Exec(`ALTER TABLE profile_versions DROP COLUMN domain_profiles`); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(s.lock.Name())
	s.Close()
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// 重复初始化也不会回滚既有人物、产生新版本或恢复已停止的旧工作。
	if err := reopened.initPersonality(); err != nil {
		t.Fatal(err)
	}
	current, err := reopened.Member(m.ID)
	if err != nil || current.Profile != m.Profile || current.ProfileRevision != m.ProfileRevision {
		t.Fatal("升级擅自重写了已有特质", err, current)
	}
	versions, err := reopened.profileHistory(m.ID)
	if err != nil || len(versions) != 2 || versions[0].Origin != "reflection" || len(versions[0].DomainProfiles) != 0 {
		t.Fatal("历史版本丢失", err, versions)
	}
	var stopped, done, tables int
	reopened.db.QueryRow(`SELECT count(*) FROM work WHERE kind='reflect' AND status='stopped' AND result='旧版整理记录'`).Scan(&stopped)
	reopened.db.QueryRow(`SELECT count(*) FROM work WHERE kind='reflect' AND status='done'`).Scan(&done)
	reopened.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('profile_experiences','profile_reflections')`).Scan(&tables)
	if stopped != 5 || done != 1 || tables != 0 {
		t.Fatal("旧后台未正确移除", stopped, done, tables)
	}
	// 旧失败项也不能通过通用“恢复工作”入口变成普通个人活动。
	response := preferenceRequest(NewHandler(NewEngine(reopened, nil, testConfig())), "POST", "/api/work/"+failedID+"/resume", nil)
	if response.Code != 409 {
		t.Fatal("已移除的旧后台仍能恢复", response.Code)
	}
	if err := reopened.ScheduleExploration(); err != nil {
		t.Fatal(err)
	}
	works, err := reopened.DueWorks()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range works {
		if w.Kind == "reflect" {
			t.Fatal("旧后台重新进入调度")
		}
	}
	var body, raw string
	if err := reopened.db.QueryRow(`SELECT body FROM messages WHERE id=?`, source.ID).Scan(&body); err != nil || body != source.Body {
		t.Fatal("原始消息丢失", err)
	}
	if err := reopened.db.QueryRow(`SELECT result FROM tool_runs WHERE id='original_read'`).Scan(&raw); err != nil || !strings.Contains(raw, "完整资料末尾") {
		t.Fatal("原始工具结果丢失", err)
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
		if reply, ok := replyIfWakeAdjust(ms); ok {
			return reply, nil
		}
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
	// 故障现场可能还包含旧人格生成、但尚未执行的发言。用户修改后恢复
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
	if _, err := setMemberProfile(tx, &m, "我现在喜欢热烈地交流。", m.DomainProfiles, "edit", "用户明确修改", nil); err != nil {
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
		if reply, ok := replyIfWakeAdjust(ms); ok {
			return reply, nil
		}
		if !strings.Contains(ms[0].Content, updated) || strings.Contains(ms[0].Content, m.Profile) {
			t.Error("恢复工具后仍使用旧人物", ms[0].Content)
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
	if err != nil || input["self"].(map[string]any)["profile_revision"] != m.ProfileRevision || !strings.Contains(memberSystem(saved, "继续交流").Content, m.Profile) {
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
