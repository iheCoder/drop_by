package dropby

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"drop_by/internal/observe"
)

func TestMemberActivityPagesKeepOwnersAndOriginalMessages(t *testing.T) {
	s, group, first := fixture(t)
	second := Member{ID: "member_zzz", Name: "第二位朋友", CreatedAt: now()}
	if err := s.AddMember(second); err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateConversation("一起查资料", "group", []string{first.ID, second.ID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := s.CreateConversation(first.Name, "direct", []string{first.ID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// One source produced two replies; page boundaries must not merge the
	// members or let a later room's reply leak into the group record.
	source := userMessage(group, "这种车是怎么设计的？")
	w := Work{ID: "work_group", ConversationID: group.ID, Kind: "chat", SourceID: source.ID, Status: "done", CreatedAt: now()}
	err = s.Guard(group.ID, group.Revision, func(tx *sql.Tx) error {
		if err := insertMessage(tx, source); err != nil {
			return err
		}
		if err := insertWork(tx, w); err != nil {
			return err
		}
		for _, member := range []Member{first, second} {
			m := Message{ID: newID("msg"), workID: w.ID, ConversationID: group.ID, AuthorID: member.ID, Body: "这位朋友的回复", CreatedAt: now()}
			if err := insertMessage(tx, m); err != nil {
				return err
			}
		}
		return insertMessage(tx, Message{ID: "msg_private", workID: w.ID, ConversationID: direct.ID, AuthorID: first.ID, Body: "这句话只出现在私聊", CreatedAt: now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTool(ToolRun{ID: "tool_search", WorkID: w.ID, ConversationID: group.ID, MemberID: first.ID, Name: "search", Arguments: json.RawMessage(`{"query":"蓝色自行车设计","count":7}`), Result: `{"content":"完整搜索结果"}`, Status: "succeeded", CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	// 同一次回复中既安排了一件事，又取消了另一件。两条结果都应在列表可见。
	for _, tool := range []ToolRun{
		{ID: "tool_schedule", Name: "schedule_followup", Arguments: json.RawMessage(`{"instruction":"明天再看车架"}`)},
		{ID: "tool_cancel", Name: "cancel_work", Arguments: json.RawMessage(`{"instruction":"不再查轮胎"}`)},
	} {
		tool.WorkID, tool.ConversationID, tool.MemberID = w.ID, group.ID, first.ID
		tool.Status, tool.Result, tool.CreatedAt = "succeeded", `{}`, now()
		if err := s.RecordTool(tool); err != nil {
			t.Fatal(err)
		}
	}

	// Eleven newer works put the two members of the same work on different
	// pages. Include a private autonomous failure and an open plan separately.
	for i := range activityPageSize - 1 {
		job := addWork(t, s, group, first, "research")
		if err := s.RecordTool(ToolRun{ID: fmt.Sprintf("tool_read_%d", i), WorkID: job.ID, ConversationID: group.ID, MemberID: first.ID, Name: "read_page", Arguments: json.RawMessage(`{"url":"https://example.com/story?id=5"}`), Result: `{}`, Status: "succeeded", CreatedAt: now()}); err != nil {
			t.Fatal(err)
		}
	}
	personal := Work{ID: "work_personal_failed", MemberID: first.ID, Kind: "explore", Status: "failed", Result: "统计输入用量失败", CreatedAt: now()}
	if err := s.Guard("", 0, func(tx *sql.Tx) error { return insertWork(tx, personal) }); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(NewEngine(s, nil, testConfig()))
	get := func(path string, dst any) string {
		t.Helper()
		r := preferenceRequest(h, http.MethodGet, path, nil)
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), dst) != nil {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
		}
		return r.Body.String()
	}
	var page struct {
		Entries []activityEntry
		Next    string
	}
	path := "/api/conversations/" + group.ID + "/activity"
	if body := get(path, &page); strings.Contains(body, personal.ID) || strings.Contains(body, "完整搜索结果") {
		t.Fatalf("群列表不应带上私聊事件或工具正文: %s", body)
	}
	if len(page.Entries) != activityPageSize || page.Next == "" {
		t.Fatalf("第一页: %+v", page)
	}
	entry := page.Entries[len(page.Entries)-1]
	if entry.WorkID != w.ID || entry.MemberID != first.ID || entry.Title != "搜索并回复" || entry.Facts[0].Text != "蓝色自行车设计" {
		t.Fatalf("搜索和已发回复应属于同一个成员: %+v", entry)
	}
	if !slices.ContainsFunc(entry.Facts, func(f activityFact) bool {
		return f.Label == "回复" && f.MessageExcerpt && f.Text == "这位朋友的回复"
	}) {
		t.Fatalf("搜索内容不应盖住这位成员的回复: %+v", entry)
	}
	for label, want := range map[string]string{"安排后续": "明天再看车架", "取消后续": "不再查轮胎"} {
		if !slices.ContainsFunc(entry.Facts, func(f activityFact) bool { return f.Label == label && f.Text == want }) {
			t.Fatalf("复合行动漏掉 %s: %+v", label, entry)
		}
	}
	get(path+"?before="+page.Next, &page)
	if len(page.Entries) != 1 || page.Next != "" || page.Entries[0].WorkID != w.ID || page.Entries[0].MemberID != second.ID {
		t.Fatalf("第二页漏掉同一工作的另一成员: %+v", page)
	}
	var d activityDetail
	body := get(path+"/"+w.ID+"?member="+first.ID, &d)
	if d.Source == nil || d.Source.ID != source.ID || len(d.Messages) != 1 || len(d.Tools) != 3 || !strings.Contains(d.Tools[0].Result, "完整搜索结果") || strings.Contains(body, "这句话只出现在私聊") {
		t.Fatalf("详情未按实际发件成员与当前聊天归属过滤: %+v", d)
	}
	if r := preferenceRequest(h, http.MethodGet, path+"/"+personal.ID+"?member="+first.ID, nil); r.Code != 404 {
		t.Fatalf("不能从群打开全局自主活动: %d", r.Code)
	}
	get("/api/conversations/"+direct.ID+"/activity", &page)
	if !slices.ContainsFunc(page.Entries, func(e activityEntry) bool { return e.WorkID == personal.ID && e.Failed }) {
		t.Fatalf("私聊遗漏自主失败: %+v", page)
	}
	body = get("/api/conversations/"+direct.ID+"/activity/"+w.ID+"?member="+first.ID, &d)
	if !strings.Contains(body, "这句话只出现在私聊") || strings.Contains(body, source.Body) || strings.Contains(body, "完整搜索结果") {
		t.Fatalf("私聊只能看到实际收到的消息: %s", body)
	}
	var conversation struct{ Plans []Work }
	get("/api/conversations/"+group.ID+"?activity=paged", &conversation)
	if len(conversation.Plans) != activityPageSize-1 {
		t.Fatalf("还在等待执行的安排应独立显示: %+v", conversation.Plans)
	}
}

func TestActivityIncludesLegacyDeliveriesAndUnlinkedMessages(t *testing.T) {
	s, group, m := fixture(t)
	// 两条旧群消息属于同一个自主行动，一条只有工具回执，另一条只有观测记录。
	// 另有一条更早的老回复只有聊天记录，仍应有独立卡片及引用原文。
	work := Work{ID: "work_autonomous", MemberID: m.ID, Kind: "explore", Status: "done", Instruction: "私人的搜索主题", CreatedAt: "2026-11-10T01:00:00Z"}
	source := userMessage(group, "旧消息原文")
	if err := s.Guard(group.ID, group.Revision, func(tx *sql.Tx) error {
		if err := insertWork(tx, work); err != nil {
			return err
		}
		if err := insertMessage(tx, source); err != nil {
			return err
		}
		for _, message := range []Message{
			{ID: "msg_legacy", Body: "一条旧回复", ReplyTo: source.ID, CreatedAt: "2026-11-10T00:30:00Z"},
			{ID: "msg_receipt", Body: "第一条群消息", CreatedAt: "2026-11-10T02:00:00Z"},
			{ID: "msg_observed", Body: "第二条群消息", CreatedAt: "2026-11-10T02:01:00Z"},
		} {
			message.ConversationID, message.AuthorID = group.ID, m.ID
			if err := insertMessage(tx, message); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTool(ToolRun{ID: "tool_legacy_delivery", WorkID: work.ID, MemberID: m.ID, Name: "send_message", Status: "succeeded", Arguments: json.RawMessage(`{}`), Result: `{"message_id":"msg_receipt"}`, CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	log, err := observe.OpenLog(filepath.Join(t.TempDir(), "observe.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	go log.Serve()
	t.Cleanup(log.Close)
	run := log.BeginRun(observe.Run{WorkID: work.ID, MemberID: m.ID})
	for _, id := range []string{"msg_receipt", "msg_observed"} {
		log.WriteStep(observe.Step{ID: "step_" + id, RunID: run, Kind: "message", Name: "send_message", Status: "sent", BusinessID: id, TargetID: group.ID})
	}
	log.Sync()
	engine := NewEngine(s, nil, testConfig())
	engine.Observe = log
	h := NewHandler(engine)
	base := "/api/conversations/" + group.ID + "/activity"
	r := preferenceRequest(h, http.MethodGet, base, nil)
	var page struct{ Entries []activityEntry }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Entries) != 2 {
		t.Fatalf("同一行动的两条消息只应合成一张卡片: %d %s", r.Code, r.Body)
	}
	first, second := page.Entries[0], page.Entries[1]
	if first.WorkID != work.ID || first.MemberID != m.ID || first.Title != "发送消息" || first.At != "2026-11-10T02:00:00Z" || len(first.Facts) != 1 || first.Facts[0].More != "共 2 条" || second.WorkID != "msg_legacy" || second.Title != "回复消息" || second.Trigger != "" {
		t.Fatalf("旧消息的分类、来源或发送时间有误: %+v", page)
	}
	r = preferenceRequest(h, http.MethodGet, base+"/"+work.ID+"?member="+m.ID, nil)
	var detail activityDetail
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &detail) != nil || detail.Work != nil || len(detail.Tools) != 0 || len(detail.Messages) != 2 || detail.Messages[0].ID != "msg_receipt" || detail.Messages[1].ID != "msg_observed" {
		t.Fatalf("群详情应保留两条群原文及顺序，并保护源行动内容: %d %s", r.Code, r.Body)
	}
	r = preferenceRequest(h, http.MethodGet, base+"/msg_legacy?member="+m.ID, nil)
	detail = activityDetail{}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &detail) != nil || !detail.ReplyReference || detail.Source == nil || detail.Source.ID != source.ID {
		t.Fatalf("旧回复只能用实际引用展示原文: %d %s", r.Code, r.Body)
	}
}

func TestFriendActivityShowsMessageSentAfterSearching(t *testing.T) {
	s, _, m := fixture(t)
	c, err := s.CreateConversation(m.Name, "direct", []string{m.ID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 自主活动没有触发消息。只有查到本聊天实际收到的消息，才显示「发消息」。
	work := Work{ID: "work_autonomous_search", MemberID: m.ID, Kind: "explore", Status: "done", CreatedAt: now()}
	const body = "刚读到一篇有意思的文章。"
	if err := s.Guard("", 0, func(tx *sql.Tx) error {
		if err := insertWork(tx, work); err != nil {
			return err
		}
		return insertMessage(tx, Message{ID: "msg_autonomous", workID: work.ID, ConversationID: c.ID, AuthorID: m.ID, Body: body, CreatedAt: now()})
	}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []ToolRun{
		{ID: "tool_search", Name: "search", Arguments: json.RawMessage(`{"query":"文章"}`)},
		{ID: "tool_read", Name: "read_page", Arguments: json.RawMessage(`{"url":"https://example.com/story"}`)},
	} {
		tool.WorkID, tool.MemberID, tool.Status, tool.Result, tool.CreatedAt = work.ID, m.ID, "succeeded", `{}`, now()
		if err := s.RecordTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(NewEngine(s, nil, testConfig()))
	r := preferenceRequest(h, http.MethodGet, "/api/conversations/"+c.ID+"/activity", nil)
	var page struct{ Entries []activityEntry }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &page) != nil || len(page.Entries) != 1 {
		t.Fatalf("自主活动列表: %d %s", r.Code, r.Body.String())
	}
	entry := page.Entries[0]
	if entry.Title != "搜索并发消息" || !slices.ContainsFunc(entry.Facts, func(f activityFact) bool {
		return f.Label == "消息" && f.MessageExcerpt && f.Text == body
	}) {
		t.Fatalf("列表应同时展示检索和实际发出的消息: %+v", entry)
	}
}

func TestReplyFailureDetailUsesOriginalWorkAndProviderOutput(t *testing.T) {
	s, _, m := fixture(t)
	c, err := s.CreateConversation(m.Name, "direct", []string{m.ID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	source := userMessage(c, "这条消息请回复一下")
	work := Work{ID: "work_reply_failure", ConversationID: c.ID, Kind: "chat", SourceID: source.ID, Status: "failed", Result: "输入容量无法核对", CreatedAt: now()}
	// The provider failed before a tool could execute. Only this source
	// message gets a notice, and the details can still show the raw output.
	if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if err := insertMessage(tx, source); err != nil {
			return err
		}
		return insertWork(tx, work)
	}); err != nil {
		t.Fatal(err)
	}
	log, err := observe.OpenLog(filepath.Join(t.TempDir(), "observe.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	go log.Serve()
	t.Cleanup(log.Close)
	run := log.BeginRun(observe.Run{WorkID: work.ID, ConversationID: c.ID})
	log.WriteStep(observe.Step{ID: "step_count", RunID: run, Kind: "request", Name: "input_tokens", Status: "failed", Output: `{"error":{"code":400}}`, Input: "无需展示的完整模型上下文"})
	log.Sync()

	engine := NewEngine(s, nil, testConfig())
	engine.Observe = log
	h := NewHandler(engine)
	base := "/api/conversations/" + c.ID
	result := preferenceRequest(h, "GET", base+"?activity=paged", nil)
	var chat struct {
		ReplyNotice *activityEntry `json:"reply_notice"`
	}
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &chat) != nil || chat.ReplyNotice == nil || chat.ReplyNotice.SourceID != source.ID || chat.ReplyNotice.WorkID != work.ID {
		t.Fatalf("提示必须指向导致失败的原消息: %d %s", result.Code, result.Body)
	}
	raw := preferenceRequest(h, "GET", base+"/activity/"+work.ID, nil)
	var d activityDetail
	if raw.Code != 200 || json.Unmarshal(raw.Body.Bytes(), &d) != nil || d.Source == nil || len(d.Errors) != 1 || d.Errors[0].Output != `{"error":{"code":400}}` || strings.Contains(raw.Body.String(), "无需展示的完整模型上下文") {
		t.Fatalf("无法核对原始错误或泄出了请求正文: %d %s", raw.Code, raw.Body)
	}
	if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		return insertMessage(tx, Message{ID: "msg_recovered", workID: work.ID, ConversationID: c.ID, AuthorID: m.ID, Body: "现在回你", CreatedAt: now()})
	}); err != nil {
		t.Fatal(err)
	}
	result = preferenceRequest(h, "GET", base+"?activity=paged", nil)
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &chat) != nil || chat.ReplyNotice != nil {
		t.Fatalf("已有实际回复后仍提示回复失败: %d %s", result.Code, result.Body)
	}
}
