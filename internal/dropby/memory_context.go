package dropby

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// context_start_seq 只决定这位成员本次自动带入的原话从哪里开始，不删除历史，
// 也不决定性格和话题分类。切题是否成立由成员结合语境判断；身份、持续关注、
// 用户更正和未完成工作不受这个窗口影响。
func (s *Store) initContextWindow() error {
	var exists int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('member_observations') WHERE name='context_start_seq'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		_, err := s.db.Exec(`ALTER TABLE member_observations ADD COLUMN context_start_seq INTEGER NOT NULL DEFAULT 0`)
		return err
	}
	return nil
}

func (s *Store) contextStart(room, member string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(`SELECT context_start_seq FROM member_observations WHERE conversation_id=? AND member_id=?`, room, member).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return seq, err
}

func (s *Store) rememberContextStart(c Conversation, member string, seen int64, id *string) error {
	if id == nil {
		return nil
	}
	return s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		var seq int64
		if *id != "" {
			// 只接受实际读到的本会话原话。跨私聊引用、未来消息不能改变记忆窗口。
			if err := tx.QueryRow(`SELECT seq FROM messages WHERE conversation_id=? AND id=? AND seq<=?`, c.ID, *id, seen).Scan(&seq); err != nil {
				return fmt.Errorf("上下文起点必须是已读到的当前聊天消息")
			}
		}
		_, err := tx.Exec(`INSERT INTO member_observations(conversation_id,member_id,context_start_seq) VALUES(?,?,?)
		 ON CONFLICT(conversation_id,member_id) DO UPDATE SET context_start_seq=excluded.context_start_seq`, c.ID, member, seq)
		return err
	})
}

// 列表是找回原文的入口，不替模型决定哪一段语义最相关。支持中文原词、多词 OR
// 和稳定游标；没有匹配时模型可以换词、浏览更早记录，再按 ID 完整读取。
func likeTerms(query string) ([]string, []any) {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		terms = []string{""}
	}
	if len(terms) > 8 {
		terms = terms[:8]
	}
	clauses, values := []string{}, []any{}
	for _, term := range terms {
		term = strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(term)
		clauses = append(clauses, `body LIKE ? ESCAPE '\'`)
		values = append(values, "%"+term+"%")
	}
	return clauses, values
}

func (s *Store) searchMemories(member, room, query string, before int64) (any, error) {
	clauses, values := likeTerms(query)
	args := []any{member, room, before, before}
	args = append(args, values...)
	rows, err := s.db.Query(`SELECT rowid,id,body,source_id,conversation_id,updated_at FROM memories
	 WHERE member_id=? AND (conversation_id=? OR conversation_id='') AND (?=0 OR rowid<?)
	 AND (`+strings.Join(clauses, " OR ")+`) ORDER BY rowid DESC LIMIT 13`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Memory{}
	var cursor int64
	more := false
	for rows.Next() {
		var id int64
		m := Memory{MemberID: member}
		if err := rows.Scan(&id, &m.ID, &m.Body, &m.SourceID, &m.ConversationID, &m.UpdatedAt); err != nil {
			return nil, err
		}
		if len(list) == 12 {
			more = true
			break
		}
		m.Body = clip(m.Body, 400)
		list = append(list, m)
		cursor = id
	}
	return map[string]any{"memories": list, "before": cursor, "has_more": more}, rows.Err()
}

func (s *Store) searchHistory(room, query, id string, before int64, offset int) (any, error) {
	if id != "" {
		var m Message
		m.ConversationID = room
		if err := s.db.QueryRow(`SELECT seq,id,author_id,body,reply_to,created_at FROM messages WHERE conversation_id=? AND id=?`, room, id).
			Scan(&m.Seq, &m.ID, &m.AuthorID, &m.Body, &m.ReplyTo, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("当前聊天没有这条消息")
		}
		chars := []rune(m.Body)
		if offset < 0 || offset > len(chars) {
			return nil, fmt.Errorf("读取位置无效")
		}
		end := min(offset+5000, len(chars))
		m.Body = string(chars[offset:end])
		return map[string]any{"message": m, "next_offset": end, "has_more": end < len(chars)}, nil
	}
	clauses, values := likeTerms(query)
	args := []any{room, before, before}
	args = append(args, values...)
	args = append(args, query) // 保持旧版本 query=消息ID 的兼容行为。
	rows, err := s.db.Query(`SELECT seq,id,author_id,body,reply_to,created_at FROM messages WHERE conversation_id=?
	 AND (?=0 OR seq<?) AND (`+strings.Join(clauses, " OR ")+` OR id=?) ORDER BY seq DESC LIMIT 13`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Message{}
	var cursor int64
	more := false
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Seq, &m.ID, &m.AuthorID, &m.Body, &m.ReplyTo, &m.CreatedAt); err != nil {
			return nil, err
		}
		if len(list) == 12 {
			more = true
			break
		}
		m.Body = contextExcerpt(m.Body, 700)
		list = append(list, m)
		cursor = m.Seq
	}
	return map[string]any{"messages": list, "before_seq": cursor, "has_more": more}, rows.Err()
}

// 长消息尾部经常是用户的最后更正。窗口摘录保留头尾，原文用分页工具读取。
func contextExcerpt(body string, limit int) string {
	r := []rune(body)
	if len(r) <= limit {
		return body
	}
	head := limit * 2 / 3
	return string(r[:head]) + "\n[中段省略；可按消息 ID 读取原文]\n" + string(r[len(r)-(limit-head):])
}

func (s *Store) artifactIndex(room, query string, offset int) (any, error) {
	if offset < 0 {
		return nil, fmt.Errorf("读取位置无效")
	}
	// 这里只取元数据，较早的安静创作也能找到，不能要求模型先猜出成果 ID。
	rows, err := s.db.Query(`SELECT a.id,a.member_id,a.title,a.version,a.format,a.created_at FROM artifacts a
	 WHERE a.conversation_id=? AND a.version=(SELECT max(version) FROM artifacts b WHERE b.id=a.id)
	 AND instr(lower(a.title),lower(?))>0 ORDER BY a.created_at DESC,a.id LIMIT 13 OFFSET ?`, room, query, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Artifact{}
	more := false
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.MemberID, &a.Title, &a.Version, &a.Format, &a.CreatedAt); err != nil {
			return nil, err
		}
		if len(list) == 12 {
			more = true
			break
		}
		list = append(list, a)
	}
	return map[string]any{"artifacts": list, "next_offset": offset + len(list), "has_more": more}, rows.Err()
}

func recallTool(name string) bool {
	switch name {
	case "search_memory", "read_memory", "read_history", "read_artifact", "read_tool_result", "read_work", "remember":
		return true
	}
	return false
}

// 决定是否发言之前也能找回经历；不能因为线索没进近期窗口就永远选择沉默。
// 此阶段只开放本地回忆与笔记，不允许先发消息、委托工作或联网搜索。
func (e *Engine) recallDecision(ctx context.Context, c Conversation, m Member, w Work, system string, input any, out any) error {
	definitions := []ToolDefinition{}
	for _, def := range e.Tools.Definitions(m, c, w) {
		if recallTool(def.Function.Name) {
			definitions = append(definitions, def)
		}
	}
	messages := []ChatMessage{{Role: "system", Content: system}, {Role: "user", Content: asJSON(input)}}
	for step := 0; step < 4; step++ {
		defs := definitions
		if step == 3 {
			defs = nil
			messages = append(messages, ChatMessage{Role: "user", Content: "先用已找回的材料完成本次参与判断。找不到的经历不要假装记得；需要更多行动可以稍后继续。只返回判断 JSON。"})
		}
		compactToolHistory(messages)
		r, err := e.Model.Complete(ctx, messages, defs, step == 3)
		if err != nil {
			return err
		}
		if len(r.ToolCalls) == 0 {
			return parseJSON(r.Content, out)
		}
		if step == 3 || len(r.ToolCalls) > 4 {
			return ErrRunLimit
		}
		messages = append(messages, r)
		for _, call := range r.ToolCalls {
			if !recallTool(call.Function.Name) {
				return fmt.Errorf("参与判断阶段不能执行 %s", call.Function.Name)
			}
			result, err := e.Tools.Execute(ctx, c, m, w, call)
			if err != nil {
				return err
			}
			messages = append(messages, ChatMessage{Role: "tool", ToolCallID: call.ID, Content: toolExcerpt(result, 6000)})
		}
	}
	return ErrRunLimit
}

const memoryContextPrompt = `
身份 self.profile、当前仍有效的方向 conversation.guidance、自己的 current_state 和未完 work 是持续上下文；近期聊天和 memory/artifacts 索引只是一小部分入口，不代表全部经历。
涉及过去约定、关系变化、以前的作品或“还记得”时，先用 search_memory/read_history/read_work 找回具体内容；可换词或翻页，不要把没看见当作没发生。必须根据真实记录回应，不能编造共同经历。
remember 保存值得以后找回的经历、约定及其变化，已过时就更新已有笔记；日常闲聊不逐句写。即使本次决定 pass，也可先保存重要经历。当前持续挂念放 state，长期身份仍来自用户编辑的 profile，不因一次交流改写性格。
若话题已明显转变，旧闲聊对当前判断无帮助，可返回 context_start_id=本聊天中新话题起点的消息ID，使之后自动上下文从这里开始。先把仍有意义的约定或经历留进 state/remember；不要丢掉未解决的更正或承诺。无需切换就省略；空串恢复默认近期窗口。历史始终可检索，选择起点不删除任何原文。`

func observationLabel(c Conversation, w Work) string {
	switch w.Kind {
	case "explore":
		return "正在回顾自己的关注"
	case "revisit":
		return "正在重新考虑先前的打算"
	}
	if c.Kind == "direct" {
		return "正在读你们的新消息"
	}
	return "正在读群里的新消息"
}
