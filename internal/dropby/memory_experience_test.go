package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func saveTestExperience(t *testing.T, box *Toolbox, c Conversation, m Member, w Work, args memoryWrite) Memory {
	t.Helper()
	result, err := box.remember(context.Background(), c, m, w, args)
	if err != nil {
		t.Fatal(err)
	}
	return result.(map[string]any)["memory"].(Memory)
}

func TestPersonalExperienceRevisionAndEvidence(t *testing.T) {
	// 同一件事由两位成员分别形成理解。纠正甲的笔记，只改变甲的当前版本；
	// 乙仍保有自己的认识，原文与旧版本留作核对，不会被当作当前结论检索。
	s, c, people := groupWithMembers(t, 2)
	a, b := people[0], people[1]
	event := userMessage(c, "这个开场动画会自动播放，我更想直接看到内容。")
	if err := s.Publish(event, c.Revision); err != nil {
		t.Fatal(err)
	}
	w := addWork(t, s, c, a, "chat")
	w.SourceID = event.ID
	box := &Toolbox{Store: s, Hub: NewHub()}
	ma := saveTestExperience(t, box, c, a, w, memoryWrite{Body: "我原先理解用户完全不喜欢动画。待核对。", SourceIDs: []string{event.ID, event.ID}})
	mb := saveTestExperience(t, box, c, b, w, memoryWrite{Body: "这次开场打断了看内容；以后先尝试直接展示，再讨论可选动效。", SourceIDs: []string{event.ID}})
	if len(ma.SourceIDs) != 1 || ma.Revision != 1 || mb.MemberID != b.ID {
		t.Fatal("来源没有去重，或个人视角混在一起")
	}
	correction := userMessage(c, "我喜欢点击之后的小动画，只是不想打开页面就被迫等开场。")
	if err := s.Publish(correction, c.Revision); err != nil {
		t.Fatal(err)
	}
	w.SourceID = correction.ID
	updated := saveTestExperience(t, box, c, a, w, memoryWrite{ID: ma.ID, Revision: &ma.Revision, Body: "做页面时直接展示主要内容；用户愿意自己点击触发小动画。我之前把反感自动开场扩大成了反感所有动画，这个理解已修正。", SourceIDs: []string{correction.ID}})
	if updated.ID != ma.ID || updated.Revision != 2 {
		t.Fatal("纠正创建了另一条笔记，而没有替换原理解")
	}
	current, _ := s.memory(a.ID, c.ID, ma.ID)
	other, _ := s.memory(b.ID, c.ID, mb.ID)
	if current.Body != updated.Body || other.Body != mb.Body {
		t.Fatal("纠正未生效或改变了同伴的个人理解")
	}
	old, err := s.readMemory(a.ID, c.ID, ma.ID, &ma.Revision)
	if err != nil || old.(map[string]any)["historical"] != true || !strings.Contains(asJSON(old), ma.Body) {
		t.Fatalf("不能明确回查旧版本：%v %v", old, err)
	}
	latest, err := s.searchMemories(a.ID, c.ID, "动画", 0)
	if err != nil || strings.Contains(asJSON(latest), ma.Body) || !strings.Contains(asJSON(latest), updated.Body) {
		t.Fatalf("普通检索仍返回旧结论：%v %v", latest, err)
	}
	if _, err := box.remember(context.Background(), c, a, w, memoryWrite{ID: ma.ID, Revision: &ma.Revision, Body: "过期写入"}); err == nil {
		t.Fatal("旧版本覆盖了刚完成的纠正")
	}
	if _, err := s.readMemory(b.ID, c.ID, ma.ID, nil); err == nil {
		t.Fatal("同伴读到了另一个人的内部判断")
	}
	profile, _ := s.Member(a.ID)
	if profile.Profile != a.Profile {
		t.Fatal("一次经历擅自改写了稳定身份")
	}
}

func TestMemoryEvidenceScopeAndGrounding(t *testing.T) {
	// 本人可以跨场景回忆原文和旧笔记；原作者、来源类型保持真实，未知 ID 仍报错。
	s, c, people := groupWithMembers(t, 2)
	a, b := people[0], people[1]
	w := addWork(t, s, c, a, "research")
	peer := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: b.ID, Body: "我倾向先检查退出入口，配色可以接着玩。", CreatedAt: now()}
	s.Publish(peer, c.Revision)
	private, _ := s.CreateConversation("私聊", "direct", []string{a.ID}, time.Hour)
	secret := userMessage(private, "另一段私聊原文")
	s.Publish(secret, private.Revision)
	box := &Toolbox{Store: s, Hub: NewHub()}
	for _, source := range []string{"not_real"} {
		if _, err := box.remember(context.Background(), c, a, w, memoryWrite{Body: "不应保存", SourceIDs: []string{source}}); err == nil {
			t.Fatalf("接受了不可引用的来源 %s", source)
		}
	}
	if err := s.RecordTool(ToolRun{ID: "copied_note", WorkID: w.ID, ConversationID: c.ID, MemberID: a.ID, Name: "read_memory", Arguments: json.RawMessage(`{}`), Result: "旧观点的复制", Status: "succeeded", CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := box.remember(context.Background(), c, a, w, memoryWrite{Body: "第二次独立印证", SourceIDs: []string{"copied_note"}}); err != nil {
		t.Fatalf("应允许引用自己的旧笔记并保留来源种类: %v", err)
	}
	note := saveTestExperience(t, box, c, a, w, memoryWrite{Scope: "member", Body: "同伴提出先看退出入口，我愿意下次遇到类似交互时找他一起看。只是这类合作的初步认识。", SourceIDs: []string{peer.ID}})
	view, err := s.readMemory(a.ID, private.ID, note.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asJSON(view), peer.Body) || !strings.Contains(asJSON(view), note.Body) {
		t.Fatal("本人理解及参与过的其他聊天原文都应可读")
	}
}

func TestMemoryCorrectionRejectsOldVersion(t *testing.T) {
	// 两个会话的旧草稿可能先后返回。即使原会话 revision 未改变，另一个
	// 会话已经纠正的同一份个人经历也必须保住，不能以最后返回者为准。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "chat")
	box := &Toolbox{Store: s, Hub: NewHub()}
	note := saveTestExperience(t, box, c, m, w, memoryWrite{Scope: "member", Body: "初步认识"})
	other, _ := s.CreateConversation("另一个场景", "group", []string{m.ID}, time.Hour)
	w2 := addWork(t, s, other, m, "chat")
	saveTestExperience(t, box, other, m, w2, memoryWrite{ID: note.ID, Revision: &note.Revision, Body: "新证据已经修正"})
	if _, err := box.remember(context.Background(), c, m, w, memoryWrite{ID: note.ID, Revision: &note.Revision, Body: "延迟返回的旧草稿"}); err == nil {
		t.Fatal("旧版本覆盖了新证据")
	}
	latest, _ := s.memory(m.ID, c.ID, note.ID)
	if latest.Body != "新证据已经修正" || latest.ConversationID != "" {
		t.Fatal("新纠正或跨场景归属丢失")
	}
}

func TestRelevantMemoryDoesNotCrowdOutCurrentWork(t *testing.T) {
	// 数百条档案不自动进入首次现场；需要的经历仍可通过工具发现。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	trigger := userMessage(c, "21:10 的邀请卡要能一键关闭开场，按按钮才播放动画；请保留三种邀请方式。")
	s.Publish(trigger, c.Revision)
	w.SourceID, w.Instruction = trigger.ID, trigger.Body
	box := &Toolbox{Store: s, Hub: NewHub()}
	useful := saveTestExperience(t, box, c, m, w, memoryWrite{Body: "之前制作邀请卡时，同伴指出开场缺少关闭入口。下次先保证退出容易，再尝试可选动画。", SourceIDs: []string{trigger.ID}})
	for i := 0; i < 350; i++ {
		if _, err := s.db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?)`, fmt.Sprintf("noise_%04d", i), m.ID, c.ID, fmt.Sprintf("档案 %d：", i)+strings.Repeat("紫色星云中的旧电影片段。", 120), "", now()); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(s, nil, testConfig())
	input, err := e.contextFor(c, m, w)
	if err != nil {
		t.Fatal(err)
	}
	if input["memory"] != nil || input["self"].(map[string]any)["profile"] != nil || !strings.Contains(memberSystem(m, "").Content, m.Profile) || !strings.Contains(asJSON(input["latest_messages"]), "三种邀请方式") {
		t.Fatal("首次现场仍自动注入记忆，或丢失身份/当前消息")
	}
	// 旧经历仍可按需检索，无需先知道 ID；无需默认灌入全部匹配项。
	found, err := box.perform(context.Background(), c, m, w, toolCall("search_memory", map[string]string{"query": "关闭入口"}))
	if err != nil || !strings.Contains(asJSON(found), useful.ID) {
		t.Fatalf("主动检索找不到经历：%v", err)
	}
}

func TestCorrectionBeforeSilenceAndNoStaleToolReplay(t *testing.T) {
	// 成员更新理解后保持安静；下次现场不自动重放笔记，主动读取获得新版。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "chat")
	box := &Toolbox{Store: s, Hub: NewHub()}
	raw, err := box.Execute(context.Background(), c, m, w, toolCall("remember", memoryWrite{Body: "旧标签：用户永远拒绝动效"}))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			Memory Memory `json:"memory"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(raw), &response)
	old := response.Result.Memory
	msg := userMessage(c, "我喜欢由自己点击触发的小动画；自动开场才是问题。")
	s.Publish(msg, c.Revision)
	w.SourceID = msg.ID
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, input []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("remember", memoryWrite{ID: old.ID, Revision: &old.Revision, Body: "用户希望直接看到页面内容，但愿意点击触发小动画；以后制作页面时保留这个选择。", SourceIDs: []string{msg.ID}})}}, nil
		}
		if !strings.Contains(input[len(input)-1].Content, `"revision":2`) {
			t.Error("修正结果没有回到参与判断")
		}
		return jsonReply(Outcome{Outcome: "已经修正理解，此刻无需重复答复"}), nil
	}), testConfig())
	result, err := e.act(context.Background(), c, m, w, "")
	if err != nil || result.Message != "" {
		t.Fatalf("安静时纠正没有完成：%+v", result)
	}
	input, err := e.contextFor(c, m, w)
	updated, readErr := s.readMemory(m.ID, c.ID, old.ID, nil)
	if err != nil || readErr != nil || input["memory"] != nil || strings.Contains(asJSON(input), old.Body) || !strings.Contains(asJSON(updated), "点击触发小动画") {
		t.Fatalf("下次行动丢失新理解或重放了旧标签：%v", err)
	}
	messages, _ := s.Messages(c.ID, 0, 20)
	if len(messages) != 1 {
		t.Fatal("整理记忆强行产生了公开消息")
	}
}

func TestLegacyMemoryUpgradeAndDeletion(t *testing.T) {
	// 老数据库的六列表和旧 INSERT 保持兼容。首次改写保存原版，重开后
	// 当前版本仍可用；删除笔记清除其版本，原聊天记录不受影响。
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := Member{ID: "legacy_person", Name: "旧成员"}
	s.AddMember(m)
	c, _ := s.CreateConversation("旧聊天", "group", []string{m.ID}, time.Hour)
	msg := userMessage(c, "可核对的旧约定")
	s.Publish(msg, c.Revision)
	s.db.Exec(`INSERT INTO memories VALUES(?,?,?,?,?,?)`, "legacy", m.ID, c.ID, "旧理解", msg.ID, now())
	note, err := s.memory(m.ID, c.ID, "legacy")
	if err != nil || note.Revision != 0 {
		t.Fatalf("旧笔记读取失败：%v", err)
	}
	box := &Toolbox{Store: s, Hub: NewHub()}
	updated := saveTestExperience(t, box, c, m, Work{SourceID: msg.ID}, memoryWrite{ID: note.ID, Revision: &note.Revision, Body: "修正后的理解"})
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	view, err := s.readMemory(m.ID, c.ID, updated.ID, &note.Revision)
	if err != nil || !strings.Contains(asJSON(view), "旧理解") {
		t.Fatalf("升级丢失了原始版本：%v", err)
	}
	box.Store = s
	saveTestExperience(t, box, c, m, Work{}, memoryWrite{ID: updated.ID, Revision: &updated.Revision, Body: ""})
	var count int
	s.db.QueryRow(`SELECT count(*) FROM memory_versions WHERE memory_id=?`, note.ID).Scan(&count)
	if count != 0 {
		t.Fatal("删除后仍残留笔记版本")
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE id=?`, msg.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("删除笔记误删了原始聊天")
	}
	if _, err := s.readMemory(m.ID, c.ID, note.ID, &note.Revision); err == nil || err == sql.ErrNoRows {
		t.Fatal("删除后仍能读到旧版本，或泄露底层错误")
	}
}
