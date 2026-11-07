package dropby

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"
)

func testPortrait(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestPersonalProfileAndGroupEditThroughHTTP(t *testing.T) {
	// 真 HTTP handler 与数据库：头像保存后可读；非法图片失败且不改旧资料。
	s, c, members := groupWithMembers(t, 3)
	cfg := testConfig()
	handler := NewHandler(NewEngine(s, nil, cfg))
	portrait := testPortrait(t)
	response := preferenceRequest(handler, "PATCH", "/api/profile", UserProfile{Name: "小林", Avatar: portrait, AvatarIcon: "moon", Color: "blue"})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	saved, _ := s.UserProfile()
	if saved.Name != "小林" || saved.Avatar != portrait {
		t.Fatal("个人资料没有保存")
	}
	response = preferenceRequest(handler, "PATCH", "/api/profile", UserProfile{Name: "新名字", Avatar: "https://tracker.invalid/image.png"})
	if response.Code != 400 {
		t.Fatal("不应接受远程跟踪头像")
	}
	saved, _ = s.UserProfile()
	if saved.Name != "小林" {
		t.Fatal("失败修改不应覆盖旧资料")
	}

	// 群变更是一个原子更新：保留历史，终止被移除成员的工作；新修订后旧回复失效。
	oldMessage := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: members[1].ID, Body: "已经说过的话", CreatedAt: now()}
	if err := s.Publish(oldMessage, c.Revision); err != nil {
		t.Fatal(err)
	}
	job := addWork(t, s, c, members[1], "research")
	response = preferenceRequest(handler, "PATCH", "/api/conversations/"+c.ID, GroupEdit{Name: "新的群名", Avatar: portrait, Revision: c.Revision, MemberIDs: []string{members[0].ID, members[2].ID}})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	updated, _ := s.Conversation(c.ID)
	if updated.Name != "新的群名" || updated.Avatar != portrait || len(updated.MemberIDs) != 2 {
		t.Fatalf("群资料未完整更新: %+v", updated)
	}
	work, _ := s.ReadWork(c.ID, job.ID)
	if work.Status != "stopped" {
		t.Fatal("离群后仍会执行工作")
	}
	messages, _ := s.Messages(c.ID, 0, 10)
	if len(messages) != 1 || messages[0].ID != oldMessage.ID {
		t.Fatal("离群删除了历史")
	}
	oldMessage.ID = newID("msg")
	if err := s.Publish(oldMessage, c.Revision); !errors.Is(err, ErrStale) {
		t.Fatal("旧修订发布应被挡住")
	}
	if err := s.Publish(oldMessage, updated.Revision); !errors.Is(err, ErrStale) {
		t.Fatal("已离群成员不能借新修订继续发布")
	}
	toolbox := Toolbox{Store: s, Config: cfg, Hub: NewHub()}
	if _, err := toolbox.Execute(context.Background(), updated, members[1], job, toolCall("save_artifact", map[string]string{"title": "迟到", "content": "不应保存"})); !errors.Is(err, ErrStale) {
		t.Fatalf("离群旧工具仍可执行: %v", err)
	}
	response = preferenceRequest(handler, "PATCH", "/api/conversations/"+c.ID, GroupEdit{Name: "过时编辑", Revision: c.Revision, MemberIDs: c.MemberIDs})
	if response.Code != 409 {
		t.Fatalf("过时表单需要冲突响应: %d", response.Code)
	}
	// 被移除的朋友可以重新加入，私聊与角色原始资料未被删掉。
	response = preferenceRequest(handler, "PATCH", "/api/conversations/"+c.ID, GroupEdit{Name: updated.Name, Avatar: portrait, Revision: updated.Revision, MemberIDs: c.MemberIDs})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = preferenceRequest(handler, "POST", "/api/conversations", map[string]any{"kind": "group", "name": "有头像的新群", "member_ids": []string{members[0].ID}, "avatar": portrait})
	var created Conversation
	json.Unmarshal(response.Body.Bytes(), &created)
	if response.Code != 201 || created.Avatar != portrait {
		t.Fatalf("新群没有保存头像: %d", response.Code)
	}
}

func TestQuotaIsCurrentStateAndRestoredWorkBecomesRunnable(t *testing.T) {
	s, c, m := fixture(t)
	cfg := testConfig()
	cfg.WeeklyBudgetMicros = 1000
	day, err := s.Reserve(cfg, 800)
	if err != nil {
		t.Fatal(err)
	}
	job := addWork(t, s, c, m, "research")
	s.Claim(job.ID)
	job.BudgetRequired = 300
	if err := s.Finish(job, "waiting_budget", "额度暂时不足", nextBudgetWeek()); err != nil {
		t.Fatal(err)
	}
	usage, _ := s.Usage()
	if !s.budgetBlocked(cfg, usage) {
		t.Fatal("当前请求确实无法预留，应显示受限提示")
	}
	// 在途请求结算释放余额，不能继续等到下周，也不能让旧聊天提示冒充现状。
	if err := s.Settle(day, 800, 100, 20, 5); err != nil {
		t.Fatal(err)
	}
	usage, _ = s.Usage()
	if s.budgetBlocked(cfg, usage) {
		t.Fatal("额度恢复后仍显示受限")
	}
	if err := s.recheckBudgetWait(cfg); err != nil {
		t.Fatal(err)
	}
	work, _ := s.ReadWork(c.ID, job.ID)
	if work.Status != "pending" || work.DueAt > time.Now().Add(time.Second).UnixMilli() {
		t.Fatal("恢复后的工作没有重新进入队列")
	}
	s.SystemMessage(c.ID, "budget_old", "本周额度用完")
	messages, _ := s.Messages(c.ID, 0, 20)
	rooms, _ := s.Conversations()
	if len(messages) != 0 || strings.Contains(rooms[0].LastMessage, "额度") {
		t.Fatal("历史额度提示仍泄漏进聊天或列表")
	}
	var preserved int
	s.db.QueryRow(`SELECT count(*) FROM messages WHERE id='budget_old'`).Scan(&preserved)
	if preserved != 1 {
		t.Fatal("隐藏历史提示不应删除原始审计记录")
	}
}

func TestResumedContextKeepsSavedArtifactAndExcludesPortraitBytes(t *testing.T) {
	s, c, m := fixture(t)
	job := addWork(t, s, c, m, "research")
	saved := ToolRun{ID: "saved_before_pause", WorkID: job.ID, ConversationID: c.ID, MemberID: m.ID, Name: "save_artifact", Arguments: json.RawMessage(`{}`), Status: "succeeded", Result: `{"artifact_id":"saved_artifact","version":1}`, CreatedAt: now()}
	if err := s.RecordTool(saved); err != nil {
		t.Fatal(err)
	}
	// 后续有多个读取行动，已保存作品仍应保留在续接索引，不能再次写一份同名文稿。
	for i := 0; i < 8; i++ {
		r := saved
		r.ID = newID("tool")
		r.Name = "read_work"
		r.Result = "已读取"
		s.RecordTool(r)
	}
	c.Avatar = testPortrait(t)
	engine := NewEngine(s, nil, testConfig())
	input, err := engine.contextFor(c, m, job)
	if err != nil {
		t.Fatal(err)
	}
	raw := asJSON(input)
	if !strings.Contains(raw, "saved_before_pause") || strings.Contains(raw, "data:image/") {
		t.Fatal("续接遗漏已存成果或把图片塞进文字上下文")
	}
}
