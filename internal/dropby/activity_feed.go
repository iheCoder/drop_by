package dropby

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"drop_by/internal/observe"
)

const activityPageSize = 12

// Personal work belongs in a friend's private view. A group only sees its own
// work, and even a group's action detail only quotes messages from that group.
const activityScope = `(w.conversation_id=? OR (w.conversation_id IS NULL AND w.member_id=?))`

func activityScopeMember(c Conversation) string {
	if c.Kind == "direct" && len(c.MemberIDs) == 1 {
		return c.MemberIDs[0]
	}
	return ""
}

const listedActivityTools = `'search','read_page','save_artifact','send_message','send_private','ask_question','schedule_exploration','schedule_followup','cancel_work','delegate_work','close_responsibility','apply_instruction','set_wakeup_rhythm'`

type activityKey struct {
	Row    int64
	WorkID string
	Member string
}

func (k activityKey) cursor() string { return strconv.FormatInt(k.Row, 10) + ":" + k.Member }

type activityFact struct {
	Label          string `json:"label"`
	Text           string `json:"text"`
	More           string `json:"more,omitempty"`
	At             string `json:"at,omitempty"`
	MessageExcerpt bool   `json:"message_excerpt,omitempty"`
}

type activityEntry struct {
	WorkID   string         `json:"work_id"`
	MemberID string         `json:"member_id"`
	At       string         `json:"at"`
	Title    string         `json:"title"`
	Trigger  string         `json:"trigger"`
	Facts    []activityFact `json:"facts"`
	Failed   bool           `json:"failed,omitempty"`
	Status   string         `json:"status,omitempty"`
	SourceID string         `json:"source_id,omitempty"`
}

type activityMessage struct {
	ID       string    `json:"id"`
	AuthorID string    `json:"author_id"`
	Body     string    `json:"body"`
	At       string    `json:"at"`
	Question *Question `json:"question,omitempty"`
}

type activityDetail struct {
	FailureReason string `json:"failure_reason,omitempty"`
	activityEntry
	Source   *activityMessage        `json:"source,omitempty"`
	Messages []activityMessage       `json:"messages,omitempty"`
	Tools    []ToolRun               `json:"tools,omitempty"`
	Errors   []observe.ActivityEvent `json:"errors,omitempty"`
	Work     Work                    `json:"work"`
}

func (s *Store) activityPlans(c Conversation) ([]Work, error) {
	rows, err := s.db.Query(`SELECT `+workColumns+` FROM work w WHERE `+activityScope+`
 AND w.kind IN ('research','revisit') AND substr(w.id,1,13)<>'coordination_'
 AND w.status IN ('pending','running','waiting_budget','paused')
 ORDER BY w.due_at,w.rowid`, c.ID, activityScopeMember(c))
	if err != nil {
		return nil, err
	}
	var plans = []Work{}
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		plans = append(plans, w)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range plans {
		d, err := s.workSummary(plans[i])
		if err != nil {
			return nil, err
		}
		plans[i].Display = &d
	}
	return plans, nil
}

// A work can involve several members. Keep each member's actions together without
// merging their records, and paginate by the original work's immutable order.
func (s *Store) activityKeys(c Conversation, before activityKey, only string, limit int) ([]activityKey, error) {
	rows, err := s.db.Query(`
WITH scope AS MATERIALIZED (
 SELECT w.id, w.rowid AS position, w.member_id,w.status
 FROM work w WHERE `+activityScope+` AND (?='' OR w.id=?) AND (?=0 OR w.rowid<=?)
), owners AS (
 SELECT w.id AS work_id,t.member_id FROM scope w JOIN tool_runs t ON t.work_id=w.id
 WHERE (t.conversation_id=? OR t.conversation_id='') AND t.name IN (`+listedActivityTools+`,'remember')
 UNION SELECT w.id,m.author_id FROM scope w JOIN messages m ON m.work_id=w.id AND m.work_id<>''
 WHERE m.conversation_id=? AND m.author_id NOT IN ('user','system')
 UNION SELECT w.id,p.member_id FROM scope w JOIN participation p ON p.work_id=w.id
 WHERE p.action IN ('speak','act','error')
 UNION SELECT w.id,w.member_id FROM scope w WHERE w.status='failed' AND w.member_id<>''
), keys AS (
 SELECT w.position,w.id,o.member_id FROM scope w JOIN owners o ON o.work_id=w.id
 UNION ALL SELECT w.position,w.id,'' FROM scope w WHERE w.status='failed'
 AND NOT EXISTS(SELECT 1 FROM owners o WHERE o.work_id=w.id)
)
SELECT position,id,member_id FROM keys
WHERE ?=0 OR position<? OR (position=? AND member_id>?)
ORDER BY position DESC,member_id LIMIT ?`,
		c.ID, activityScopeMember(c), only, only, before.Row, before.Row, c.ID, c.ID,
		before.Row, before.Row, before.Row, before.Member, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []activityKey
	for rows.Next() {
		var k activityKey
		if err := rows.Scan(&k.Row, &k.WorkID, &k.Member); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

type activityArgs struct {
	ID          string   `json:"id"`
	Query       string   `json:"query"`
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Instruction string   `json:"instruction"`
	Text        string   `json:"text"`
	Options     []string `json:"options"`
	Member      string   `json:"member_id"`
}

type activityReceipt struct {
	ID         string `json:"id"`
	MessageID  string `json:"message_id"`
	ArtifactID string `json:"artifact_id"`
	Title      string `json:"title"`
	WorkID     string `json:"work_id"`
	Work       struct {
		ID string `json:"id"`
	} `json:"work"`
	Status string                     `json:"status"`
	Reused bool                       `json:"reused"`
	Memory *struct{ ID, Body string } `json:"memory"`
}

func (s *Store) actionTools(c Conversation, k activityKey, detail bool) ([]ToolRun, error) {
	// The list only needs small receipt fields. Complete original tool records
	// are read on demand, including any additional steps within this action.
	query := `
SELECT id,work_id,conversation_id,member_id,name,arguments,result,status,created_at
FROM tool_runs WHERE work_id=? AND member_id=? AND (conversation_id=? OR conversation_id='')
ORDER BY rowid`
	if !detail {
		query = `
SELECT id,work_id,conversation_id,member_id,name,
 CASE WHEN json_valid(arguments) THEN json_object(
  'id',json_extract(arguments,'$.id'),'query',json_extract(arguments,'$.query'),
  'url',json_extract(arguments,'$.url'),'title',json_extract(arguments,'$.title'),
  'instruction',json_extract(arguments,'$.instruction'),'text',json_extract(arguments,'$.text'),
  'options',json_extract(arguments,'$.options'),'member_id',json_extract(arguments,'$.member_id')) ELSE '{}' END,
 CASE WHEN json_valid(result) THEN json_object(
  'id',json_extract(result,'$.id'),'message_id',json_extract(result,'$.message_id'),
  'artifact_id',json_extract(result,'$.artifact_id'),'title',json_extract(result,'$.title'),
  'work_id',coalesce(json_extract(result,'$.work_id'),json_extract(result,'$.work.id')),
  'status',json_extract(result,'$.status'),'reused',CASE WHEN json_extract(result,'$.reused') THEN json('true') ELSE json('false') END,
  'memory',CASE WHEN json_extract(result,'$.memory.id') IS NOT NULL THEN json_object('id',json_extract(result,'$.memory.id'),'body',substr(json_extract(result,'$.memory.body'),1,240)) END) ELSE '{}' END,
 status,created_at
FROM tool_runs WHERE work_id=? AND member_id=? AND (conversation_id=? OR conversation_id='')
 AND name IN (` + listedActivityTools + `,'remember')
ORDER BY rowid`
	}
	rows, err := s.db.Query(query, k.WorkID, k.Member, c.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []ToolRun
	for rows.Next() {
		var r ToolRun
		var args string
		if err = rows.Scan(&r.ID, &r.WorkID, &r.ConversationID, &r.MemberID, &r.Name, &args, &r.Result, &r.Status, &r.CreatedAt); err != nil {
			return nil, err
		}
		if detail && !json.Valid([]byte(args)) {
			args = asJSON(args)
		}
		r.Arguments = json.RawMessage(args)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// A delivery receipt alone is insufficient: verify that the message exists in
// this room, and prefer its actual text over the requested text.
func (s *Store) activityMessages(room, workID, member string, ids []string) ([]activityMessage, error) {
	rows, err := s.db.Query(`
SELECT m.id,m.author_id,m.body,m.created_at,coalesce(q.question,'')
FROM messages m LEFT JOIN message_questions q ON q.message_id=m.id
WHERE m.conversation_id=? AND (?='' OR m.author_id=?)
 AND ((?<>'' AND m.work_id=?) OR m.id IN (SELECT value FROM json_each(?)))
ORDER BY m.seq`, room, member, member, workID, workID, asJSON(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []activityMessage
	for rows.Next() {
		var m activityMessage
		var question string
		if err := rows.Scan(&m.ID, &m.AuthorID, &m.Body, &m.At, &question); err != nil {
			return nil, err
		}
		if question != "" {
			decode(question, &m.Question)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (a *API) loadActivity(c Conversation, k activityKey, events []observe.ActivityEvent, detail bool) (activityDetail, error) {
	s := a.engine.Store
	w, err := scanWork(s.db.QueryRow(`SELECT `+workColumns+` FROM work w WHERE w.id=? AND `+activityScope, k.WorkID, c.ID, activityScopeMember(c)))
	d := activityDetail{activityEntry: activityEntry{WorkID: k.WorkID, MemberID: k.Member, At: w.CreatedAt, SourceID: w.SourceID}, Work: w}
	if err != nil {
		return d, err
	}
	d.Trigger = "自主触发"
	switch {
	case w.AssignedBy != "":
		d.Trigger = "协作安排"
	case w.Kind == "research" || w.Kind == "revisit":
		d.Trigger = "后续安排"
	case w.SourceID != "":
		d.Trigger = "消息触发"
	}

	tools, err := s.actionTools(c, k, detail)
	if err != nil {
		return d, err
	}
	var messageIDs []string
	for _, event := range events {
		// Older observations may have no member. Only use those on a work
		// that names its one owner, or on the unattributed group failure row.
		if event.MemberID != k.Member && !(event.MemberID == "" && k.Member == w.MemberID) {
			continue
		}
		if event.Step == "input_tokens" {
			d.Errors = append(d.Errors, event)
		}
		if event.RoomID == c.ID && event.MessageID != "" {
			messageIDs = append(messageIDs, event.MessageID)
		}
	}
	for _, tool := range tools {
		if tool.Status != "succeeded" {
			continue
		}
		var r activityReceipt
		decode(tool.Result, &r)
		switch tool.Name {
		case "send_message", "send_private":
			messageIDs = append(messageIDs, r.MessageID)
		case "ask_question":
			messageIDs = append(messageIDs, r.ID)
		}
	}
	d.Messages, err = s.activityMessages(c.ID, w.ID, k.Member, messageIDs)
	if err != nil {
		return d, err
	}
	if detail && w.SourceID != "" {
		source, err := s.activityMessages(c.ID, "", "", []string{w.SourceID})
		if err != nil {
			return d, err
		}
		if len(source) != 0 {
			d.Source = &source[0]
		}
	}
	if detail {
		d.Tools = make([]ToolRun, 0, len(tools))
		for _, tool := range tools {
			// Do not bring another conversation's private message into this panel.
			switch tool.Name {
			case "send_private", "send_message", "ask_question":
				var r activityReceipt
				decode(tool.Result, &r)
				id := r.MessageID
				if tool.Name == "ask_question" {
					id = r.ID
				}
				if (tool.Status == "succeeded" || tool.Name == "send_private") && !slices.ContainsFunc(d.Messages, func(m activityMessage) bool { return m.ID == id }) {
					continue
				}
			}
			d.Tools = append(d.Tools, tool)
		}
	}
	var latestError string
	// Only a recorded failure can label the card as failed. An empty outcome or
	// a voluntary decision to stay quiet has no bearing on that state.
	var decision string
	err = s.db.QueryRow(`SELECT action,reason FROM participation WHERE work_id=? AND member_id=? ORDER BY rowid DESC LIMIT 1`, w.ID, k.Member).Scan(&decision, &latestError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	d.Failed = decision == "error" || (w.Status == "failed" && (k.Member == w.MemberID || k.Member == ""))
	if decision == "error" && latestError != "" {
		d.FailureReason = latestError
	}
	err = s.activityCopy(c, &d, tools)
	return d, err
}

func displayActivityText(body string) string {
	if internalActivityText.MatchString(body) || json.Valid([]byte(body)) {
		return ""
	}
	return body
}

func activityExcerptLine(body string) string {
	runes := []rune(strings.Join(strings.Fields(body), " "))
	if len(runes) > 230 {
		return string(runes[:230]) + "…"
	}
	return string(runes)
}

func (s *Store) activityCopy(c Conversation, d *activityDetail, tools []ToolRun) error {
	var queries, pages, saves []string
	var plan, operation, preview, note string
	var typeTitle string
	var haveNote, freshSearch, freshPage bool
	artifactIDs := map[string]bool{}
	for _, tool := range tools {
		if tool.Status != "succeeded" {
			continue
		}
		var args activityArgs
		var receipt activityReceipt
		json.Unmarshal(tool.Arguments, &args)
		decode(tool.Result, &receipt)
		switch tool.Name {
		case "search":
			query := strings.TrimSpace(args.Query)
			if query != "" && !slices.Contains(queries, query) {
				queries = append(queries, query)
			}
			freshSearch = freshSearch || !receipt.Reused
		case "read_page":
			page, err := url.Parse(args.URL)
			if err == nil && (page.Scheme == "https" || page.Scheme == "http") && page.Host != "" && !slices.Contains(pages, page.String()) {
				pages = append(pages, page.String())
			}
			freshPage = freshPage || !receipt.Reused
		case "save_artifact":
			title := receipt.Title
			if title == "" {
				title = args.Title
			}
			if title != "" && !artifactIDs[receipt.ArtifactID] {
				saves = append(saves, title)
				artifactIDs[receipt.ArtifactID] = true
			}
		case "remember":
			if receipt.Memory != nil {
				haveNote = true
				if note == "" {
					note = strings.SplitN(receipt.Memory.Body, "\n", 2)[0]
				}
			}
		case "schedule_exploration", "schedule_followup", "cancel_work", "delegate_work", "close_responsibility", "apply_instruction", "set_wakeup_rhythm":
			labels := map[string]string{
				"schedule_exploration": "安排后续", "schedule_followup": "安排后续", "cancel_work": "取消后续",
				"delegate_work": "安排协作", "close_responsibility": "结束分工", "apply_instruction": "更新约定", "set_wakeup_rhythm": "调整自主活动",
			}
			if operation != "" && tool.Name != "cancel_work" {
				continue
			}
			operation = labels[tool.Name]
			plan = args.Instruction
			target := receipt.WorkID
			if target == "" {
				target = receipt.Work.ID
			}
			if tool.Name == "cancel_work" && target == "" {
				target = args.ID
			}
			if target != "" {
				job, err := scanWork(s.db.QueryRow(`SELECT `+workColumns+` FROM work w WHERE w.id=? AND `+activityScope, target, c.ID, activityScopeMember(c)))
				if err == nil {
					plan = job.Instruction
					if job.Status == "done" {
						d.Status = "现已结束"
					} else if job.Status == "stopped" {
						d.Status = "已取消"
					} else if tool.Name == "cancel_work" {
						d.Status = workStatusText(job)
					}
				} else if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
		default:
			// Delivery is checked against the actual messages below.
		}
	}
	add := func(label, value, more string) {
		if value != "" {
			d.Facts = append(d.Facts, activityFact{Label: label, Text: activityExcerptLine(value), More: more})
		}
	}
	total := func(n int, unit string) string {
		if n <= 1 {
			return ""
		}
		return fmt.Sprintf("共 %d %s", n, unit)
	}
	if len(queries) > 0 {
		label := "搜索"
		if !freshSearch {
			label = "复用搜索"
		}
		add(label, queries[0], total(len(queries), "组"))
	}
	if len(pages) > 0 {
		label := "阅读"
		if !freshPage {
			label = "查看缓存"
		}
		var hosts []string
		for _, page := range pages {
			u, _ := url.Parse(page)
			host := strings.TrimPrefix(u.Host, "www.")
			if !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
		first, _ := url.Parse(pages[0])
		location := strings.Join(hosts[:min(2, len(hosts))], "、")
		if len(pages) == 1 {
			// Keep page identifiers such as ?id=123; drop the scheme for display.
			location = hosts[0] + first.RequestURI()
		}
		add(label, location, total(len(pages), "个网页"))
	}
	if len(saves) > 0 {
		add("保存", saves[0], total(len(saves), "件"))
	}
	if operation != "" {
		if plan != "" {
			add(operation, displayActivityText(plan), "")
		}
		typeTitle = operation
	}
	for _, message := range d.Messages {
		if message.Question != nil {
			question := message.Question.Text
			if i := strings.IndexAny(question, "？?"); i >= 0 {
				prefix := []rune(question[:i])
				for j := len(prefix) - 1; j >= 0; j-- {
					if strings.ContainsRune("。！!\n", prefix[j]) {
						prefix = prefix[j+1:]
						break
					}
				}
				question = strings.TrimSpace(string(prefix)) + string([]rune(question[i:])[0])
			}
			add("提问", question, fmt.Sprintf("%d 个选项", len(message.Question.Options)))
			typeTitle = "提问"
			break
		}
		if preview == "" {
			preview = strings.SplitN(message.Body, "\n\n", 2)[0]
		}
	}
	replied := len(d.Messages) != 0 && d.Work.SourceID != ""
	switch {
	case d.Failed:
		d.Title = "行动失败"
		if len(d.Errors) > 0 {
			last := d.Errors[len(d.Errors)-1]
			d.Facts = append([]activityFact{{Label: "失败", Text: "统计输入用量时请求失败"}}, d.Facts...)
			// Keep the older activity's position; the failure may be days later.
			if at, err := time.Parse(time.RFC3339Nano, last.At); err == nil {
				d.Facts[0].At = at.Format(time.RFC3339Nano)
			}
		} else if d.FailureReason != "" || d.Work.Result != "" {
			reason := d.FailureReason
			if reason == "" {
				reason = d.Work.Result
			}
			add("失败", reason, "")
		}
	case typeTitle != "":
		d.Title = typeTitle
	case len(saves) > 0 && len(d.Messages) != 0 && !replied:
		d.Title = "保存作品并发消息"
	case len(saves) > 0:
		d.Title = "保存作品"
	case len(queries) > 0 && replied:
		d.Title = "搜索并回复"
	case len(pages) > 0 && replied:
		d.Title = "阅读并回复"
	case len(queries) > 0 && len(pages) > 0:
		d.Title = "搜索与阅读"
	case len(pages) > 0:
		d.Title = "阅读网页"
	case len(queries) > 0:
		d.Title = "搜索网页"
	case replied:
		d.Title = "回复消息"
	case len(d.Messages) != 0:
		d.Title = "发送消息"
	case haveNote:
		d.Title = "保存笔记"
		add("笔记", displayActivityText(note), "")
	}
	if preview != "" && len(d.Facts) == 0 {
		label := "消息"
		if replied {
			label = "回复"
		}
		d.Facts = append(d.Facts, activityFact{Label: label, Text: activityExcerptLine(preview), More: total(len(d.Messages), "条"), MessageExcerpt: true})
	}
	if len(d.Errors) > 0 && !d.Failed && replied {
		d.Status = "曾中断 · 已回复"
	} else if replied && (len(queries) > 0 || len(pages) > 0) && d.Status == "" {
		d.Status = "已回复"
	} else if haveNote && d.Status == "" && d.Title != "保存笔记" {
		d.Status = "保存了笔记"
	}
	if d.Title == "" {
		// Waiting, stopped or failed tool steps can be inspected without implying
		// that the model read a page, sent a message or changed a plan.
		for _, tool := range tools {
			if strings.Contains(","+listedActivityTools+",", ",'"+tool.Name+"',") && tool.Status != "succeeded" {
				d.Title = actionTitles[tool.Name]
				if d.Title == "" {
					d.Title = "行动记录"
				}
				if tool.Status == "failed" {
					d.Status = "未完成"
					d.Failed = true
				} else if tool.Status == "running" {
					d.Status = "进行中"
				} else {
					d.Status = "未完成"
				}
				var args activityArgs
				json.Unmarshal(tool.Arguments, &args)
				switch tool.Name {
				case "read_page":
					if u, err := url.Parse(args.URL); err == nil && u.Host != "" {
						add("网页", strings.TrimPrefix(u.Host, "www.")+u.RequestURI(), "")
					}
				case "search":
					add("搜索", args.Query, "")
				default:
					add("内容", displayActivityText(args.Instruction), "")
				}
				break
			}
		}
	}
	return nil
}

func (a *API) activityEvents(keys []activityKey, detail bool) map[string][]observe.ActivityEvent {
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.WorkID)
	}
	events, err := a.engine.Observe.ActivityEvents(ids, detail)
	if err != nil {
		// Observation is optional. A temporarily unavailable log must not hide
		// messages, work statuses or tool receipts from the main database.
		return nil
	}
	grouped := make(map[string][]observe.ActivityEvent)
	for _, event := range events {
		grouped[event.WorkID] = append(grouped[event.WorkID], event)
	}
	return grouped
}

func (a *API) activityPage(w http.ResponseWriter, r *http.Request) {
	s := a.engine.Store
	c, err := s.Conversation(r.PathValue("id"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "聊天不存在"})
		return
	}
	var before activityKey
	if raw := r.URL.Query().Get("before"); raw != "" {
		pos, member, ok := strings.Cut(raw, ":")
		before.Row, err = strconv.ParseInt(pos, 10, 64)
		if !ok || err != nil || before.Row <= 0 {
			respond(w, 400, map[string]string{"error": "行动分页位置无效"})
			return
		}
		before.Member = member
	}

	entries := []activityEntry{}
	next := ""
	for len(entries) <= activityPageSize {
		keys, err := s.activityKeys(c, before, "", activityPageSize+1)
		if err != nil {
			a.fail(w, err)
			return
		}
		events := a.activityEvents(keys, false)
		for _, key := range keys {
			d, err := a.loadActivity(c, key, events[key.WorkID], false)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				a.fail(w, err)
				return
			}
			if d.Title == "" {
				continue
			}
			if len(entries) == activityPageSize {
				respond(w, 200, map[string]any{"entries": entries, "next": next})
				return
			}
			entries = append(entries, d.activityEntry)
			next = key.cursor()
		}
		if len(keys) < activityPageSize+1 {
			break
		}
		before = keys[len(keys)-1]
	}
	respond(w, 200, map[string]any{"entries": entries, "next": ""})
}

func (a *API) activityRecord(w http.ResponseWriter, r *http.Request) {
	c, err := a.engine.Store.Conversation(r.PathValue("id"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "聊天不存在"})
		return
	}
	keys, err := a.engine.Store.activityKeys(c, activityKey{}, r.PathValue("work"), -1)
	if err != nil {
		a.fail(w, err)
		return
	}
	member := r.URL.Query().Get("member")
	if i := slices.IndexFunc(keys, func(k activityKey) bool { return k.Member == member }); i >= 0 {
		k := keys[i]
		d, err := a.loadActivity(c, k, a.activityEvents([]activityKey{k}, true)[k.WorkID], true)
		if errors.Is(err, sql.ErrNoRows) {
			respond(w, 404, map[string]string{"error": "行动记录不存在"})
			return
		}
		if err != nil {
			a.fail(w, err)
			return
		}
		if d.Title != "" {
			respond(w, 200, d)
			return
		}
	}
	respond(w, 404, map[string]string{"error": "行动记录不存在"})
}

// A chat notice is tied to the current incoming message. It opens the same
// history record as the feed, and disappears when a real reply is available.
func (a *API) replyFailureNotice(c Conversation, messages []Message, works []Work) (*activityEntry, error) {
	var latest Message
	for _, message := range messages {
		if message.AuthorID != "system" {
			latest = message
		}
	}
	if latest.AuthorID != "user" {
		return nil, nil
	}
	for _, work := range works {
		if work.SourceID != latest.ID || work.Status != "failed" || (work.Kind != "chat" && work.Kind != "followup") {
			continue
		}
		keys, err := a.engine.Store.activityKeys(c, activityKey{}, work.ID, -1)
		if err != nil {
			return nil, err
		}
		var notice *activityEntry
		events := a.activityEvents(keys, false)[work.ID]
		for _, key := range keys {
			d, err := a.loadActivity(c, key, events, false)
			if err != nil {
				return nil, err
			}
			if len(d.Messages) != 0 {
				return nil, nil
			}
			if d.Failed && notice == nil {
				notice = &d.activityEntry
			}
		}
		return notice, nil
	}
	return nil, nil
}
