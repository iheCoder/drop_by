package dropby

import (
	"context"
	"fmt"
	"math"

	"slices"
	"sort"
	"strings"
)

// SQL 查原文与分享，query 按大意找，id 按版本精读。每种方式沿用相同的数据归属。
func recallDefinition() ToolDefinition {
	return definition("recall", "回顾自己可见的作品、聊天原话和个人记忆。写 sql 直接查，或用 query 按大意找候选，按 id 读原件。长结果默认展示原文预览，可用 full 要求完整返回。分享作品可把对应版本的 save_run_id 放进 send_message.evidence。", map[string]any{
		"sql":                   textField("只读 SQLite 查询，条件、排序和分页直接写在 SQL 中。records(kind: artifact/message/memory, id, version, revision, seq, title, body, author_id, mine, conversation_id, created_at, local_time, save_run_id)，mine=1 表示本人，local_time 是服务本地时间。shares(message_id, conversation_id, conversation_name, artifact_id, version, save_run_id) 是聊天实际引用的作品版本。"),
		"query":                 textField("用整句大意做本地向量搜索；省略查询可按时间浏览。sql、query、id 三选一"),
		"full":                  map[string]any{"type": "boolean", "description": "完整交付本次结果。按 id 可通读整条记录；语义搜索可读取本页候选的正文。默认长结果仅展示原文预览，完整结果可按 tool_run_id 回读。"},
		"types":                 map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"artifact", "message", "memory"}}},
		"conversation_id":       textField("只看这个聊天里的记录与分享；省略时跨本人可见的场景"),
		"member_id":             textField("只看这位作者的记录（真人消息可用 user）"),
		"id":                    textField("打开结果中的记录 ID"),
		"version":               map[string]any{"type": "integer", "minimum": 1, "description": "作品版本；按 ID 省略时读最新可见版"},
		"revision":              map[string]any{"type": "integer", "minimum": 0, "description": "读取记忆的指定修订版"},
		"include_source_bodies": map[string]any{"type": "boolean", "description": "读取记忆时展开来源的完整正文"},
		"offset":                map[string]any{"type": "integer", "minimum": 0, "description": "搜索结果偏移，或打开作品、消息的正文字符位置"},
		"before_seq":            map[string]any{"type": "integer", "minimum": 1, "description": "仅查序号更早的聊天消息，排除其他类型；用结果的 seq 连续向前翻页"},
		"limit":                 map[string]any{"type": "integer", "minimum": 1, "description": "每页候选数，默认 12"},
		"length":                map[string]any{"type": "integer", "minimum": 1, "description": "打开作品或消息时明确要读的字符数"},
	})
}

type recallRequest struct {
	SQL                   string
	Full                  bool
	Query                 string
	Types                 []string
	Room                  string `json:"conversation_id"`
	Author                string `json:"member_id"`
	ID                    string
	Version               int
	Revision              *int
	SourceBodies          bool  `json:"include_source_bodies"`
	BeforeSeq             int64 `json:"before_seq"`
	Offset, Limit, Length int
}

type recallRecord struct {
	Seq      int64  `json:"seq,omitempty"`
	Kind     string `json:"type"`
	ID       string `json:"id"`
	Version  int    `json:"version,omitempty"`
	Revision *int   `json:"revision,omitempty"`
	Title    string `json:"title,omitempty"`
	Author   string `json:"author_id"`
	Room     string `json:"conversation_id,omitempty"`
	Date     string `json:"created_at"`
	Evidence string `json:"save_run_id,omitempty"`
	Match    string `json:"match,omitempty"`
	body     string
	Body     string `json:"body,omitempty"`
}

// 分享权限绑定真实消息引用的那一次保存。不能因为作者分享过旧稿，就把更新稿也编入同伴的候选。
const recallShareCTE = `WITH rooms AS (SELECT conversation_id FROM participants WHERE member_id=?1 AND (?2='' OR conversation_id=?2)),
 saves AS MATERIALIZED (SELECT t.id,t.member_id,json_extract(t.result,'$.artifact_id') aid,json_extract(t.result,'$.version') ver
 FROM tool_runs t WHERE t.name='save_artifact' AND t.status='succeeded' AND json_valid(t.result)),
 visible_shares AS (SELECT r.aid,r.ver,r.member_id,r.id save_run_id,m.id message_id,m.conversation_id FROM messages m JOIN rooms p ON p.conversation_id=m.conversation_id
 JOIN json_each(m.evidence) e JOIN saves r ON r.id=e.value AND r.member_id=m.author_id) `

const recallArtifactRows = recallShareCTE + ` SELECT a.id,a.version,a.member_id,coalesce(a.conversation_id,''),a.created_at,a.title,a.content,0,
 coalesce((SELECT s.id FROM saves s WHERE s.aid=a.id AND s.ver=a.version AND s.member_id=a.member_id ORDER BY s.id LIMIT 1),'')
 FROM artifacts a WHERE ((?2='' AND a.member_id=?1) OR a.conversation_id IN rooms OR
 EXISTS(SELECT 1 FROM visible_shares s WHERE s.aid=a.id AND s.ver=a.version AND s.member_id=a.member_id))`

var recallSources = []struct{ kind, sql string }{
	{"artifact", recallArtifactRows},
	{"message", `SELECT m.id,0,m.author_id,m.conversation_id,m.created_at,'',m.body,m.seq,'' FROM messages m
 WHERE m.conversation_id IN (SELECT conversation_id FROM participants WHERE member_id=?1 AND (?2='' OR conversation_id=?2))`},
	{"memory", `SELECT m.id,coalesce(d.revision,0),m.member_id,m.conversation_id,m.updated_at,'',m.body,0,''
 FROM memories m LEFT JOIN memory_details d ON d.memory_id=m.id WHERE m.member_id=?1 AND (?2='' OR m.conversation_id=?2)`},
}

func (s *Store) recallRows(ctx context.Context, member string, q recallRequest) ([]recallRecord, error) {
	// 先按成员、场景、作者和类型选原始数据，再进入语义检索。缓存从不决定谁能读什么。

	var records []recallRecord
	for _, source := range recallSources {
		if (q.BeforeSeq > 0 && source.kind != "message") || (q.Version > 0 && source.kind != "artifact") {
			continue
		}
		if len(q.Types) > 0 && !slices.Contains(q.Types, source.kind) {
			continue
		}
		// 在数据库内定位 ID，已有记录即使没有类型前缀也可以精读。
		// 浏览只需短摘录，打开 ID 后由各记录的原文读取路径完成分页和来源展开。
		// 大意和原词查找都需要完整原文；显式条件交给 SQLite，按 ID 可使用其索引。
		bodyColumn := "substr(body,1,200)"
		if q.ID == "" && (q.Query != "" || q.Full) {
			bodyColumn = "body"
		}
		query := "WITH candidates(id,number,author,room,stamp,title,body,seq,receipt) AS (" + source.sql +
			") SELECT id,number,author,room,stamp,title," + bodyColumn + ",seq,receipt FROM candidates WHERE 1=1"
		args := []any{member, q.Room}
		if q.ID != "" {
			query += " AND id=?"
			args = append(args, q.ID)
		}
		if q.Author != "" {
			query += " AND author=?"
			args = append(args, q.Author)
		}
		if q.Version > 0 {
			query += " AND number=?"
			args = append(args, q.Version)
		}
		if q.BeforeSeq > 0 {
			query += " AND seq<?"
			args = append(args, q.BeforeSeq)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)

		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r := recallRecord{Kind: source.kind}
			var version int
			if err := rows.Scan(&r.ID, &version, &r.Author, &r.Room, &r.Date, &r.Title, &r.body, &r.Seq, &r.Evidence); err != nil {
				rows.Close()
				return nil, err
			}
			if r.Kind == "memory" {
				r.Revision = &version
			} else if r.Kind == "artifact" {
				r.Version = version
			}
			records = append(records, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	// 空查询按时间浏览。同时间保留稳定顺序，按 ID 打开总选最新可见版本。
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if q.Query == "" && (q.BeforeSeq > 0 || (len(q.Types) == 1 && q.Types[0] == "message")) {
			return a.Seq > b.Seq
		}
		if q.ID != "" && a.ID == b.ID {
			return a.Version > b.Version
		}
		if a.Date != b.Date {
			return a.Date > b.Date
		}
		if a.Seq != b.Seq {
			return a.Seq > b.Seq
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Version > b.Version
	})
	return records, nil
}

func (s *Store) recall(ctx context.Context, member string, q recallRequest) (any, error) {
	if q.BeforeSeq < 0 || q.Offset < 0 || q.Limit < 0 || q.Length < 0 || q.Version < 0 || (q.Revision != nil && *q.Revision < 0) {
		return nil, fmt.Errorf("读取位置或版本无效")
	}
	for _, kind := range q.Types {
		if !slices.Contains([]string{"artifact", "message", "memory"}, kind) {
			return nil, fmt.Errorf("未知记录类型 %q", kind)
		}
	}
	if q.Room != "" {
		if _, err := s.accessibleConversation(member, q.Room); err != nil {
			return nil, err
		}
	}
	q.SQL = strings.TrimSpace(q.SQL)
	q.Query = strings.TrimSpace(q.Query)
	if q.SQL != "" {
		hasScope := q.Room != "" || q.Author != "" || len(q.Types) != 0 || q.BeforeSeq != 0
		hasSlice := q.Limit != 0 || q.Offset != 0 || q.Length != 0 || q.Version != 0 || q.Revision != nil
		if q.Query != "" || q.ID != "" || hasScope || hasSlice || q.SourceBodies {
			return nil, fmt.Errorf("SQL 的筛选、截取和分页直接写入 sql；可加 full 指定完整交付")
		}
		return s.queryRecallSQL(ctx, member, q.SQL)
	}
	if q.Full && q.Length != 0 {
		return nil, fmt.Errorf("full 读取全文，length 读取指定范围，请选一种")
	}
	if q.ID != "" && q.Query != "" {
		return nil, fmt.Errorf("按 id 打开或按 query 查找，请选择一种")
	}
	if q.ID == "" && (q.Revision != nil || q.SourceBodies) {
		return nil, fmt.Errorf("读取记忆版本及来源需要 id")
	}
	records, err := s.recallRows(ctx, member, q)
	if err != nil {
		return nil, err
	}
	if q.ID != "" {
		if len(records) == 0 {
			return nil, fmt.Errorf("找不到可见的记录或指定版本")
		}
		r := records[0]
		if r.Kind != "memory" && (q.Revision != nil || q.SourceBodies) {
			return nil, fmt.Errorf("只有记忆支持修订号及来源展开")
		}
		switch r.Kind {
		case "memory":
			return s.readMemory(member, r.Room, r.ID, q.Revision, q.SourceBodies)
		case "message":
			return s.readMessage(r.Room, r.ID, q.Offset, readDefault([]int{q.Length}, 0, math.MaxInt))
		case "artifact":
			var a Artifact
			err := s.db.QueryRowContext(ctx, `SELECT id,coalesce(conversation_id,''),member_id,title,content,version,source_id,created_at,format FROM artifacts WHERE id=? AND version=?`, r.ID, r.Version).Scan(&a.ID, &a.ConversationID, &a.MemberID, &a.Title, &a.Content, &a.Version, &a.SourceID, &a.CreatedAt, &a.Format)
			if err != nil {
				return nil, err
			}
			chars := []rune(a.Content)
			if q.Offset > len(chars) {
				return nil, fmt.Errorf("读取位置无效")
			}
			end := q.Offset + min(readDefault([]int{q.Length}, 0, len(chars)), len(chars)-q.Offset)
			a.Content = string(chars[q.Offset:end])
			return map[string]any{"artifact": a, "save_run_id": r.Evidence, "next_offset": end, "has_more": end < len(chars)}, nil
		}
	}
	if q.Query != "" && len(records) > 0 {
		hits, err := s.recallModel.rank(ctx, q.Query, records)
		if err != nil {
			return nil, fmt.Errorf("本地语义查找失败，可用 sql 查原文：%w", err)
		}
		// 只按本地模型给出的相似度排序，命中段落原样交付。
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		ranked := make([]recallRecord, 0, len(hits))
		for _, h := range hits {
			r := records[h.Index]
			r.Match = h.Match
			ranked = append(ranked, r)
		}
		records = ranked
	}
	// 一篇多次修改的作品占一个位置，保留最匹配的那一版；显式 version 可找回其他版。
	seen := map[string]bool{}
	page := []recallRecord{}
	limit := readDefault([]int{q.Limit}, 0, 12)
	more := false
	for _, r := range records {
		key := r.Kind + "/" + r.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(seen) <= q.Offset {
			continue
		}
		if len(page) == limit {
			more = true
			break
		}
		if r.Match == "" {
			chars := []rune(r.body)
			r.Match = string(chars[:min(len(chars), 200)])
		}
		if q.Full {
			r.Body = r.body
		}
		page = append(page, r)
	}
	return map[string]any{"records": page, "next_offset": q.Offset + len(page), "has_more": more}, nil
}
