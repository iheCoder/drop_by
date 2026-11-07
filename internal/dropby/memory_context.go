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
	rows, err := s.db.Query(strings.Replace(memorySelect, "SELECT m.id", "SELECT m.rowid,m.id", 1)+`
	 WHERE m.member_id=? AND (m.conversation_id=? OR m.conversation_id='') AND (?=0 OR m.rowid<?)
	 AND (`+strings.Join(clauses, " OR ")+`) ORDER BY m.rowid DESC LIMIT 13`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Memory{}
	var cursor int64
	more := false
	for rows.Next() {
		var id int64
		m, err := scanMemory(rows, &id)
		if err != nil {
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
		r, err := completeModel(ctx, e.Model, &messages, defs, step == 3)
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
memory 是按当前事项找出的少量个人经历线索，不是所有人共享的事实或完整人物画像。先理解眼前任务、最新条件和自己的倾向，再判断哪条旧理解适用；无关往事无需提起，也不必证明自己记得。兴趣、默契、玩笑、分歧可以影响关注与行动，不只影响措辞；可以赞同、补充、独立切入或安静，不为表现个性故意反对。
涉及过去约定、关系变化、以前的作品或“还记得”时，先用 search_memory/read_history/read_work 找回具体内容；可换词或翻页，不要把没看见当作没发生。必须根据真实记录回应，不能编造共同经历。
出现有意义的合作结果、共同玩笑、分歧或理解变化时，用 remember 保留自己的少量理解及原始来源、适用场景，而不是给每位成员写评语。每个人可注意到不同部分，也可以没有新结论。同一成员换群仍是同一个人：可跨场景复用的非敏感相处理解、工作习惯和自身经历用 scope=member；只限本群的约定或私密内容留 conversation。当前持续挂念放 state，稳定身份仍来自 profile，不因一次交流改写性格。
真人明确表达“以后这类事情怎么做”的偏好，或刚完成的合作确实改变了自己下次的做法，应在结束本次判断/行动前用 remember 保存；state 和 conversation.guidance 只在本聊天里，不能代替可携带经历。区分这次的时间地点等临时条件与以后适用的方式，不把两者一起固化。已有相同理解则无需重写；每次合作也不必编造一条新的感悟。完成整理后仍可 pass，不需要公开宣告记住了。
不要把有意义的共同经历全部压成“用户喜欢某流程”。确实形成合作认识时，简短保留和谁、发生何事、自己的理解，以及它可能怎样影响下一次行动；相关人用实际名字和 subject_ids 保留，便于以后知道自己在说谁。已有同一认识就更新原笔记，不再另存近似总结。只写有依据的部分，也允许暂时没有对人的新判断，不强行制造感悟或不同意见。
本轮若明确纠正了自己保存的理解，结束前先 search_memory/read_memory 找到旧笔记，用原 id 和已读 revision 更新；即使最终 pass 也完成纠正。不要仅口头答应，也不要新建相反笔记留下旧结论。明确偏好/事实纠正立即应用；对人的泛化评价需保留场景和不确定性；临时例外只改变这次做法。旧版本只供审计，系统故障不能归因成成员性格，相同来源反复转述不能加强判断。
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
