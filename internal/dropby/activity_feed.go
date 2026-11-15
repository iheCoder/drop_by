package dropby

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"drop_by/internal/observe"
)

const activityPageSize = 12

// Full work context is visible in its originating room or the owner's private
// view. Other rooms see only the messages they actually received.
const activityScope = `(w.conversation_id=? OR (w.conversation_id IS NULL AND w.member_id=?))`

func activityScopeMember(c Conversation) string {
	if c.Kind == "direct" && len(c.MemberIDs) == 1 {
		return c.MemberIDs[0]
	}
	return ""
}

const listedActivityTools = `'search','read_page','save_artifact','send_message','send_private','ask_question','schedule_exploration','schedule_followup','cancel_work','delegate_work','close_responsibility','apply_instruction','set_wakeup_rhythm'`

type activityKey struct {
	At         string
	WorkID     string
	Member     string
	LocalWork  bool
	MessageIDs string
}

func (k activityKey) cursor() string { return k.At + "|" + k.WorkID + "|" + k.Member }

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
	Trigger  string         `json:"trigger,omitempty"`
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
	ReplyTo  string    `json:"-"`
}

type activityDetail struct {
	FailureReason string `json:"failure_reason,omitempty"`
	activityEntry
	Source         *activityMessage        `json:"source,omitempty"`
	Messages       []activityMessage       `json:"messages,omitempty"`
	Tools          []ToolRun               `json:"tools,omitempty"`
	Errors         []observe.ActivityEvent `json:"errors,omitempty"`
	Work           *Work                   `json:"work,omitempty"`
	ReplyReference bool                    `json:"reply_reference,omitempty"`
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

// Start from real deliveries in this room and give each message exactly one
// owner card. A stored work ID wins over a legacy receipt or observation.
// Messages without a verified link are still actions in their own right.
func (s *Store) activityKeys(c Conversation, before activityKey, only string, limit int, deliveriesJSON string) ([]activityKey, error) {
	rows, err := s.db.Query(`
WITH scope AS MATERIALIZED (
 SELECT w.id,w.created_at,w.member_id,w.status
 FROM work w WHERE (w.conversation_id=:room OR (w.conversation_id IS NULL AND w.member_id=:personal))
 AND (:only='' OR w.id=:only)
), room_messages AS MATERIALIZED (
 SELECT id,author_id,created_at,work_id
 FROM messages WHERE conversation_id=:room AND author_id NOT IN ('user','system')
), receipts AS (
 SELECT m.id,min(t.work_id) AS work_id FROM tool_runs t
 JOIN room_messages m ON m.author_id=t.member_id AND m.work_id=''
 AND m.id=CASE WHEN json_valid(t.result) THEN json_extract(t.result,
  CASE t.name WHEN 'ask_question' THEN '$.id' ELSE '$.message_id' END) END
 JOIN work w ON w.id=t.work_id
 WHERE t.status='succeeded' AND t.name IN ('send_message','send_private','ask_question')
 GROUP BY m.id HAVING count(DISTINCT t.work_id)=1
), observed AS (
 SELECT m.id,min(w.id) AS work_id FROM json_each(:deliveries) e
 JOIN room_messages m ON m.id=json_extract(e.value,'$.message_id') AND m.author_id=json_extract(e.value,'$.member_id') AND m.work_id=''
 JOIN work w ON w.id=json_extract(e.value,'$.work_id')
 GROUP BY m.id HAVING count(DISTINCT w.id)=1
), linked AS (
 SELECT m.*,coalesce(w.id,r.work_id,o.work_id,m.id) AS card_id
 FROM room_messages m LEFT JOIN work w ON w.id=m.work_id AND m.work_id<>''
 LEFT JOIN receipts r ON r.id=m.id LEFT JOIN observed o ON o.id=m.id
), received AS (
 SELECT card_id,author_id,min(julianday(created_at)) AS position,json_group_array(id) AS messages
 FROM linked WHERE :only='' OR card_id=:only GROUP BY card_id,author_id
), owners AS (
 SELECT w.id,t.member_id FROM scope w JOIN tool_runs t ON t.work_id=w.id
 WHERE (t.conversation_id=:room OR t.conversation_id='') AND t.name IN (`+listedActivityTools+`,'remember')
 UNION SELECT w.id,p.member_id FROM scope w JOIN participation p ON p.work_id=w.id
 WHERE p.action IN ('speak','act','error')
 UNION SELECT w.id,w.member_id FROM scope w WHERE w.status='failed' AND w.member_id<>''
 UNION SELECT card_id,author_id FROM received
), keys AS (
 SELECT id,member_id FROM owners
 UNION ALL SELECT w.id,'' FROM scope w WHERE w.status='failed'
 AND NOT EXISTS(SELECT 1 FROM owners o WHERE o.id=w.id)
), ordered AS (
 SELECT strftime('%Y-%m-%dT%H:%M:%fZ',coalesce(r.position,julianday(w.created_at))) AS position,
        k.id,k.member_id,w.id IS NOT NULL AS local_work,coalesce(r.messages,'[]') AS messages
 FROM keys k LEFT JOIN scope w ON w.id=k.id LEFT JOIN received r ON r.card_id=k.id AND r.author_id=k.member_id
)
SELECT position,id,member_id,local_work,messages FROM ordered
WHERE :at='' OR position<:at OR (position=:at AND (id>:id OR (id=:id AND member_id>:member)))
ORDER BY position DESC,id,member_id LIMIT :limit`,
		sql.Named("room", c.ID), sql.Named("personal", activityScopeMember(c)), sql.Named("only", only),
		sql.Named("deliveries", deliveriesJSON), sql.Named("at", before.At), sql.Named("id", before.WorkID),
		sql.Named("member", before.Member), sql.Named("limit", limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []activityKey
	for rows.Next() {
		var k activityKey
		if err := rows.Scan(&k.At, &k.WorkID, &k.Member, &k.LocalWork, &k.MessageIDs); err != nil {
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
	RoomID      string   `json:"conversation_id"`
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
  'conversation_id',json_extract(arguments,'$.conversation_id'),
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
		// Explicitly targeted operations may have another room's arguments.
		var target struct {
			Room string `json:"conversation_id"`
		}
		json.Unmarshal([]byte(args), &target)
		if (target.Room != "" && target.Room != c.ID) || (r.Name == "send_private" && r.Status != "succeeded" && c.Kind != "direct") {
			continue
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
func (s *Store) activityMessages(room, member, idsJSON string) ([]activityMessage, error) {
	rows, err := s.db.Query(`
SELECT m.id,m.author_id,m.body,m.created_at,m.reply_to,coalesce(q.question,'')
FROM messages m LEFT JOIN message_questions q ON q.message_id=m.id
WHERE m.conversation_id=? AND (?='' OR m.author_id=?)
 AND m.id IN (SELECT value FROM json_each(?))
ORDER BY m.seq`, room, member, member, idsJSON)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []activityMessage
	for rows.Next() {
		var m activityMessage
		var question string
		if err := rows.Scan(&m.ID, &m.AuthorID, &m.Body, &m.At, &m.ReplyTo, &question); err != nil {
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
	d := activityDetail{activityEntry: activityEntry{WorkID: k.WorkID, MemberID: k.Member, At: k.At}}
	var err error
	d.Messages, err = s.activityMessages(c.ID, k.Member, k.MessageIDs)
	if err != nil {
		return d, err
	}

	// An incoming delivery from a different room exposes only the received
	// messages. An unlinked historical message follows the same path.
	if len(d.Messages) != 0 {
		d.At = d.Messages[0].At
	}
	if k.LocalWork {
		w, err := scanWork(s.db.QueryRow(`SELECT `+workColumns+` FROM work w WHERE w.id=? AND `+activityScope, k.WorkID, c.ID, activityScopeMember(c)))
		if err != nil {
			return d, err
		}
		d.Work = &w
		if len(d.Messages) == 0 {
			d.At = w.CreatedAt
		}
		switch {
		case w.AssignedBy != "":
			d.Trigger = "协作安排"
		case w.Kind == "research" || w.Kind == "revisit":
			d.Trigger = "后续安排"
		case w.SourceID != "":
			d.Trigger = "消息触发"
		default:
			d.Trigger = "自主触发"
		}
	}

	// A work source describes what prompted the work. An explicit reply can
	// still provide a quote when the original work or its source is unknown.
	sourceIDs := []string{}
	if d.Work != nil && d.Work.SourceID != "" {
		sourceIDs = append(sourceIDs, d.Work.SourceID)
	}
	for _, message := range d.Messages {
		if message.ReplyTo != "" && !slices.Contains(sourceIDs, message.ReplyTo) {
			sourceIDs = append(sourceIDs, message.ReplyTo)
		}
	}
	if len(sourceIDs) > 0 {
		sources, err := s.activityMessages(c.ID, "", asJSON(sourceIDs))
		if err != nil {
			return d, err
		}
		for _, id := range sourceIDs {
			if i := slices.IndexFunc(sources, func(m activityMessage) bool { return m.ID == id }); i >= 0 {
				d.SourceID = id
				if detail {
					d.Source = &sources[i]
					d.ReplyReference = d.Work == nil || id != d.Work.SourceID
				}
				break
			}
		}
	}
	if !k.LocalWork {
		err = s.activityCopy(c, &d, nil)
		return d, err
	}

	tools, err := s.actionTools(c, k, false)
	if err != nil {
		return d, err
	}
	w := d.Work
	for _, event := range events {
		// Older observations may have no member. Only use those on a work
		// that names its one owner, or on the unattributed group failure row.
		if event.MemberID == k.Member || (event.MemberID == "" && k.Member == w.MemberID) {
			d.Errors = append(d.Errors, event)
		}
	}
	if detail {
		// Compose the same facts as the list before adding complete receipts.
		rawTools, err := s.actionTools(c, k, true)
		if err != nil {
			return d, err
		}
		for _, tool := range rawTools {
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
	var operation, preview, note string
	var operations []activityFact
	var question *Question
	var haveNote, freshSearch, freshPage bool
	artifactIDs := map[string]bool{}
	var failedSteps []string
	for _, tool := range tools {
		label := actionTitles[tool.Name]
		if tool.Status == "failed" && label != "" && !slices.Contains(failedSteps, label) {
			failedSteps = append(failedSteps, label)
		}
		if tool.Status != "succeeded" {
			continue
		}
		failedSteps = slices.DeleteFunc(failedSteps, func(failed string) bool { return failed == label })
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
			id := receipt.ArtifactID
			if id == "" {
				id = title
			}
			if title != "" && !artifactIDs[id] {
				saves = append(saves, title)
				artifactIDs[id] = true
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
			label := labels[tool.Name]
			if operation == "" || tool.Name == "cancel_work" {
				operation = label
			}
			plan, status := args.Instruction, ""
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
						status = "现已结束"
					} else if job.Status == "stopped" {
						status = "已取消"
					} else if tool.Name == "cancel_work" {
						status = workStatusText(job)
					}
				} else if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			fact := activityFact{Label: label, Text: activityExcerptLine(displayActivityText(plan)), More: status}
			if !slices.Contains(operations, fact) {
				operations = append(operations, fact)
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
	for _, fact := range operations {
		// The title already covers this result when it has no further text.
		if fact.Text != "" || fact.More != "" || len(operations) > 1 || len(d.Messages) > 0 {
			d.Facts = append(d.Facts, fact)
		}
	}
	for _, message := range d.Messages {
		if message.Question != nil {
			if question == nil {
				question = message.Question
			}
		} else if preview == "" {
			preview = strings.SplitN(message.Body, "\n\n", 2)[0]
		}
	}
	if question != nil {
		more := fmt.Sprintf("%d 个选项", len(question.Options))
		if preview == "" && len(d.Messages) > 1 {
			more += " · " + total(len(d.Messages), "条消息")
		}
		add("提问", question.Text, more)
	}
	replied := len(d.Messages) != 0 && d.SourceID != ""
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
		} else {
			reason := d.FailureReason
			if reason == "" && d.Work != nil {
				reason = d.Work.Result
			}
			add("失败", reason, "")
		}
	case question != nil:
		d.Title = "提问"
	case operation != "" && !(replied && (len(queries) > 0 || len(pages) > 0)):
		d.Title = operation
	case len(saves) > 0 && replied:
		d.Title = "保存作品并回复"
	case len(saves) > 0 && len(d.Messages) != 0:
		d.Title = "保存作品并发消息"
	case len(saves) > 0:
		d.Title = "保存作品"
	case len(queries) > 0 && replied:
		d.Title = "搜索并回复"
	case len(pages) > 0 && replied:
		d.Title = "阅读并回复"
	case len(queries) > 0 && len(d.Messages) != 0:
		d.Title = "搜索并发消息"
	case len(pages) > 0 && len(d.Messages) != 0:
		d.Title = "阅读并发消息"
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
	// A delivered message stays visible even when this action also has search,
	// reading, or other outcomes on the same card.
	if preview != "" {
		label := "消息"
		if replied {
			label = "回复"
		}
		d.Facts = append(d.Facts, activityFact{Label: label, Text: activityExcerptLine(preview), More: total(len(d.Messages), "条"), MessageExcerpt: true})
	}
	if len(d.Messages) > 0 && len(failedSteps) > 0 {
		add("未完成", strings.Join(failedSteps, "、"), "")
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
		if k.LocalWork {
			ids = append(ids, k.WorkID)
		}
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

func (a *API) activityDeliveryJSON(c Conversation) string {
	// When observation is unavailable, actual messages still get a card and
	// persisted work IDs and delivery receipts continue to group them.
	deliveries, _ := a.engine.Observe.ActivityDeliveries(c.ID)
	return asJSON(deliveries)
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
		parts := strings.Split(raw, "|")
		if len(parts) != 3 || parts[1] == "" {
			respond(w, 400, map[string]string{"error": "行动分页位置无效"})
			return
		}
		if _, err := time.Parse(time.RFC3339Nano, parts[0]); err != nil {
			respond(w, 400, map[string]string{"error": "行动分页位置无效"})
			return
		}
		before = activityKey{At: parts[0], WorkID: parts[1], Member: parts[2]}
	}

	deliveries := a.activityDeliveryJSON(c)
	entries := []activityEntry{}
	next := ""
	for len(entries) <= activityPageSize {
		keys, err := s.activityKeys(c, before, "", activityPageSize+1, deliveries)
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
	keys, err := a.engine.Store.activityKeys(c, activityKey{}, r.PathValue("work"), -1, a.activityDeliveryJSON(c))
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
		keys, err := a.engine.Store.activityKeys(c, activityKey{}, work.ID, -1, a.activityDeliveryJSON(c))
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
