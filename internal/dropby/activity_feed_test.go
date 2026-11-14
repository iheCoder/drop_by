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
	get(path+"?before="+page.Next, &page)
	if len(page.Entries) != 1 || page.Next != "" || page.Entries[0].WorkID != w.ID || page.Entries[0].MemberID != second.ID {
		t.Fatalf("第二页漏掉同一工作的另一成员: %+v", page)
	}
	var d activityDetail
	body := get(path+"/"+w.ID+"?member="+first.ID, &d)
	if d.Source == nil || d.Source.ID != source.ID || len(d.Messages) != 1 || len(d.Tools) != 1 || !strings.Contains(d.Tools[0].Result, "完整搜索结果") || strings.Contains(body, "这句话只出现在私聊") {
		t.Fatalf("详情未按实际发件成员与当前聊天归属过滤: %+v", d)
	}
	if r := preferenceRequest(h, http.MethodGet, path+"/"+personal.ID+"?member="+first.ID, nil); r.Code != 404 {
		t.Fatalf("不能从群打开全局自主活动: %d", r.Code)
	}
	get("/api/conversations/"+direct.ID+"/activity", &page)
	if !slices.ContainsFunc(page.Entries, func(e activityEntry) bool { return e.WorkID == personal.ID && e.Failed }) {
		t.Fatalf("私聊遗漏自主失败: %+v", page)
	}
	var conversation struct{ Plans []Work }
	get("/api/conversations/"+group.ID+"?activity=paged", &conversation)
	if len(conversation.Plans) != activityPageSize-1 {
		t.Fatalf("还在等待执行的安排应独立显示: %+v", conversation.Plans)
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
