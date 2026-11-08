package dropby

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Toolbox struct {
	webCache webResultCache
	Store    *Store
	Config   Config
	Hub      *Hub
	Activity func(string, string, string)
}

func definition(name, desc string, properties map[string]any, required ...string) ToolDefinition {
	// JSON Schema 的 required 必须是数组；无必填参数的检索工具传 []，不能传 null。
	if required == nil {
		required = []string{}
	}
	return ToolDefinition{Type: "function", Function: ToolFunction{Name: name, Description: desc, Parameters: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}}
}
func textField(desc string) any { return map[string]string{"type": "string", "description": desc} }

func (t *Toolbox) Definitions(m Member, c Conversation, w Work) []ToolDefinition {
	defs := []ToolDefinition{
		definition("organize_context", "整理本人在当前场景的工作上下文。用续接便笺和选中的完整原文替换旧窗口，随后继续行动；不删除历史、不修改人物或任务状态。省略引用则只留便笺。", map[string]any{"note": textField("接下来仍有用的理解、分歧、更正或未完打算，自由撰写"), "keep_message_ids": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "需要保留原话的消息 ID，可来自本人参与的其他聊天"}, "keep_tool_run_ids": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "需要继续保留完整结果的可见工具记录 ID"}}, "note"),
		definition("send_message", "发送消息后可继续行动。可发到自己所在的聊天；个人活动省略目的地时发给真人私聊。", map[string]any{"conversation_id": textField("目的聊天 ID，普通聊天省略则发在当前聊天"), "message": textField("自己的消息"), "reply_to": textField("可选的目的聊天消息 ID"), "evidence": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "可选成果 tool_run ID"}, "notify": map[string]any{"type": "boolean", "description": "额外系统通知，默认 false"}}, "message"),
		definition("read_tool_result", "分段查看本人可访问的完整工具结果，供旧正文折叠后核对。", map[string]any{"id": textField("真实 tool_run ID"), "offset": map[string]any{"type": "integer", "minimum": 0}}, "id"),
		definition("read_memory", "读取个人记忆及来源；默认最新版本，revision 可指定历史版本。", map[string]any{"id": textField("已有记忆 ID"), "revision": map[string]any{"type": "integer", "minimum": 0, "description": "通常省略；回查历史版本时才填写"}}, "id"),
		definition("remember", "保存个人经历或想法。更新用原 id 和 revision；body 为空时删除。", map[string]any{
			"id": textField("更新已有笔记的 ID；新建省略"), "revision": map[string]any{"type": "integer", "minimum": 0, "description": "更新/删除时填写读到的 revision；发生冲突先重新读取"},
			"scope":       map[string]any{"type": "string", "enum": []string{"conversation", "member"}, "description": "新建默认 conversation，更新默认沿用；member 可携带自己的非敏感经历与相处理解，不搬运秘密或私聊原文"},
			"body":        textField("以自己的视角记下经历、想法或以后想怎么做"),
			"subject_ids": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "相关人的真实 ID，不限当前聊天成员"},
			"source_ids":  map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "相关消息或行动 ID；可引用失败和旧笔记，来源状态如实保留；省略时绑定本次触发"},
		}, "body"),
		definition("read_history", "检索当前聊天原话。可用多词换词搜索、before_seq 向前翻页；指定 id 后用 offset 分段读取完整原文。可用 conversation_id 选择本人参与的聊天。", map[string]any{"query": textField("字词以空格分隔，任一匹配；空串浏览记录"), "before_seq": map[string]any{"type": "integer", "minimum": 0}, "id": textField("完整读取某条消息时填写实际ID"), "offset": map[string]any{"type": "integer", "minimum": 0}}),
		definition("search_memory", "查找自己的全部经历。多个词以空格分隔，任一匹配；空串浏览，before 用返回游标翻页。旧经历不必常驻上下文，需要时 read_memory 读全文。", map[string]any{"query": textField("想找回的词；可换词检索，不是向量语义搜索"), "before": map[string]any{"type": "integer", "minimum": 0}}, "query"),
		definition("read_work", "查看当前聊天的实际工作状态与结果。提供 id 读完整工作；省略 id 列出未完工作，可按 member_id 筛选、offset 翻页。可用 conversation_id 选择本人参与的其他聊天。", map[string]any{"id": textField("实际工作ID"), "member_id": textField("可选，浏览某位成员的未完工作"), "offset": map[string]any{"type": "integer", "minimum": 0}}),
		// 本地成果创作是基本能力，旧成员不必重新配置工具才能生成网页。
		definition("save_artifact", "保存可查看的 Markdown 或 HTML 成果，如小卡片、文章、网页草稿。HTML 使用完整自包含文档和内联样式，不依赖 CDN 或外部资源。修改时提供原 id，新版本保留历史。", map[string]any{"id": textField("修改已有成果的 ID，新建时省略"), "title": textField("具体标题"), "format": map[string]any{"type": "string", "enum": []string{"markdown", "html"}, "description": "新建默认 markdown；修改省略时沿用原格式"}, "content": textField("完整内容；HTML 使用完整源代码")}, "title", "content"),
		definition("read_artifact", "发现当前聊天的成果，或读取指定成果及历史版本。不知道 ID 时用 query 按标题查找，空串浏览，可用 offset 翻页；知道 ID 后读取正文。", map[string]any{"id": textField("已有成果 ID，可省略"), "query": textField("要找的标题字词，省略则浏览"), "offset": map[string]any{"type": "integer", "minimum": 0}}),
		definition("cancel_work", "取消自己在当前聊天里已经安排、但现在不再需要的后续或研究；不能取消同伴的工作，自己的已结束工作会返回实际终态。先从已有工作中取真实 ID；用户改意或事情已解决时及时取消。", map[string]any{"id": textField("自己的待执行工作 ID")}, "id"),
	}
	if !c.ResearchPaused {
		defs = append(defs, definition("schedule_followup", "预约稍后检查某件事，例如邀请后是否有人回应、约定是否有进展。到期会读取最新消息和工作，再自行决定行动；服务需运行。", map[string]any{"instruction": textField("到时要核对的具体事情或相关消息，以及自己的打算"), "reason": textField("来自哪段交流、兴趣或真实进展"), "delay_seconds": map[string]any{"type": "integer", "minimum": 1}, "due_at": textField("可选绝对时间，RFC3339且含时区，例如2026-11-08T09:00:00+08:00；与delay_seconds只选一个。一次定时，不是周期任务，服务需运行。")}, "instruction"))
		defs = append(defs, definition("schedule_exploration", "安排后台研究或制作。", map[string]any{"instruction": textField("可用现有工具执行的具体方向，不是泛泛关注"), "reason": textField("来自什么兴趣、经历或新材料"), "delay_seconds": map[string]any{"type": "integer", "minimum": 0}}, "instruction"))
	}
	// 本轮已看到的本人临时职责允许重复确认结束；Store 仍核对本人和事项。
	// 真正委托权限每次查当前记录，结束职责后不能继续安排同伴。
	for _, responsibility := range c.Responsibilities {
		if responsibility.MemberID != m.ID || responsibility.ScopeID == "" {
			continue
		}
		defs = append(defs, definition("close_responsibility", "当前事项已经完成时，结束自己对应的临时组织职责。不取消研究、不修改长期约定；需先收齐实际结果，有未完工作时返回 waiting 及具体工作。", map[string]any{"id": textField("conversation.responsibilities 中自己的临时职责ID")}, "id"))
		break
	}
	if t.Store.CanDelegate(c, m, w) {
		defs = append(defs, definition("delegate_work", "依据当前真实约定，委托同群成员完成一项具体工作。coordinate=true 可请对方组织这个子事项，不扩大原授权。写明预期结果、边界及选择理由；完成或遇到问题后会获得复看机会。", map[string]any{"member_id": textField("本群另一成员ID"), "instruction": textField("具体任务、预期结果和边界"), "reason": textField("为什么需要这件事、为什么选择这位成员"), "coordinate": map[string]any{"type": "boolean", "description": "是否委托对方组织这项子工作；默认自己完成具体产出"}}, "member_id", "instruction"))
	}
	if !c.DiscussionPaused && c.ID != "" {
		faces := make([]string, 0, len(reactionPalette))
		for _, face := range reactionPalette {
			faces = append(faces, face.Emoji)
		}
		defs = append(defs, definition(reactionTool, "用表情回应当前聊天的一条消息，或撤回自己的回应。回应显示在原消息下，不发送新消息。可只回应表情，也可以继续交流。", map[string]any{
			"message_id": textField("当前聊天的实际消息 ID"),
			"emoji":      map[string]any{"type": "string", "enum": faces},
			"present":    map[string]any{"type": "boolean", "description": "true 添加自己的回应；false 撤回自己的回应"},
		}, "message_id", "emoji", "present"))
		defs = append(defs, definition("ask_question", "在聊天里发布一个可自由回答的选择题。用户也可以直接聊别的；调用成功即已发布。", map[string]any{"text": textField("一个简短自然的问题"), "options": map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "minItems": 2, "description": "可选答案"}}, "text", "options"))
		defs = append(defs, definition("send_private", "向真人发送私聊消息。", map[string]any{"message": textField("自然的私聊内容"), "notify": map[string]string{"type": "boolean"}}, "message", "notify"))
	}
	for _, name := range m.Tools {
		switch name {
		case "search":
			if !c.ResearchPaused && t.Config.SearchKey != "" {
				defs = append(defs, definition("search", "搜索公开网页，返回标题、链接和摘要。", map[string]any{"query": textField("1 到 128 个 Unicode 字符；支持 site:、-site:、lang:、after:、before: 等搜索语法"), "count": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "结果数量，省略默认 8"}, "need_content": map[string]any{"type": "boolean", "description": "是否获取正文，默认 false"}, "ori_question": map[string]any{"type": []string{"string", "null"}, "description": "原始问题，最多 128 字符"}, "localization": textField("本地化搜索使用的 IP 地址")}, "query"))
			}
		case "read_page":
			if !c.ResearchPaused && t.Config.SearchKey != "" {
				defs = append(defs, definition("read_page", "读取公开网页，返回正文或本次读取结果。", map[string]any{"url": textField("公开 HTTP(S) 地址")}, "url"))
			}
		}
	}
	defs = append(defs, definition("read_conversation", "查看自己所在聊天的当前现场。用于选择交流对象或接续关系；历史可继续用 read_history。", map[string]any{"conversation_id": textField("conversations 中的聊天 ID")}, "conversation_id"))
	for i := range defs {
		props := defs[i].Function.Parameters.(map[string]any)["properties"].(map[string]any)
		name := defs[i].Function.Name
		if name == "read_history" || name == "read_work" || name == "read_artifact" || name == "read_tool_result" || name == "read_memory" || name == "search_memory" || name == "remember" {
			props["conversation_id"] = textField("可选，自己参与的聊天 ID；省略使用当前场景")
		}

		switch name {
		case "search", "read_page":
			props["refresh"] = map[string]any{"type": "boolean", "description": "重新获取，跳过已有缓存；默认 false"}
		}
		switch name {
		case "read_tool_result", "read_history", "read_conversation", "search_memory", "read_work", "read_artifact":
			props["limit"] = map[string]any{"type": "integer", "minimum": 1, "description": "列表条数；省略使用默认值"}
		}
		switch name {
		case "read_tool_result", "read_history", "read_artifact":
			props["length"] = map[string]any{"type": "integer", "minimum": 1, "description": "正文读取字符数；省略默认 5000"}
		}
		if name == "read_tool_result" {
			defs[i].Function.Parameters.(map[string]any)["required"] = []string{}
			props["query"] = textField("省略 id 时检索当前聊天可见工具的参数和结果；提供 id 时从 offset 起定位此文本")
		}
		if name == "schedule_exploration" {
			props["due_at"] = textField("绝对预约时间，RFC3339，含时区；与 delay_seconds 二选一")
		}
		if name == "read_artifact" {
			props["version"] = map[string]any{"type": "integer", "minimum": 1, "description": "指定历史版本；省略读最新版"}
		}
	}
	return defs
}

func (t *Toolbox) Execute(ctx context.Context, c Conversation, m Member, w Work, call ToolCall) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := t.Store.checkMembership(c, m.ID); err != nil {
		return "", err
	}
	allowed := false
	for _, def := range t.Definitions(m, c, w) {
		if def.Function.Name == call.Function.Name {
			allowed = true
		}
	}
	if !allowed {
		return "", fmt.Errorf("当前成员没有该工具权限：%s", call.Function.Name)
	}
	if !json.Valid([]byte(call.Function.Arguments)) {
		return "", fmt.Errorf("工具参数不是有效的 JSON")
	}
	// 参数对象按键排序保存；空白和键序不改变请求的含义。
	decoder := json.NewDecoder(bytes.NewBufferString(call.Function.Arguments))
	decoder.UseNumber()
	var canonical any
	if err := decoder.Decode(&canonical); err != nil {
		return "", err
	}
	call.Function.Arguments = asJSON(canonical)

	// 同一条模型 tool_call 的重放复用已有结果；新的调用 ID 是成员的新行动，
	// 不按相似文本去重。执行中断且结果不明时禁止盲目重放写操作。
	runID := newID("tool")
	if call.ID != "" {
		runID = fmt.Sprintf("tool_%x", sha256.Sum256([]byte(w.ID+"\x00"+m.ID+"\x00"+call.ID)))
	}
	resuming := false
	var oldStatus, oldResult string
	readErr := t.Store.db.QueryRow("SELECT status,result FROM tool_runs WHERE id=?", runID).Scan(&oldStatus, &oldResult)
	if readErr == nil {
		if oldStatus == "running" {
			if !readOnlyTool(call.Function.Name) && call.Function.Name != "send_message" && call.Function.Name != reactionTool {
				return "", errUncertainTool
			}
			resuming = true
		} else {
			return asJSON(map[string]any{"tool_run_id": runID, "status": oldStatus, "result": json.RawMessage(oldResult)}), nil
		}
	}
	if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
		return "", readErr
	}
	run := ToolRun{ID: runID, WorkID: w.ID, ConversationID: c.ID, MemberID: m.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments), Status: "running", CreatedAt: now()}
	if !resuming {
		if err := t.Store.RecordTool(run); err != nil {
			return "", err
		}
	}
	if t.Activity != nil && call.Function.Name != reactionTool {
		labels := map[string]string{"organize_context": "正在整理思路", "search": "正在搜索网页", "read_page": "正在阅读网页", "save_artifact": "正在保存成果", "remember": "正在整理经历", "schedule_exploration": "正在安排后续探索", "schedule_followup": "正在记下之后想继续的事", "cancel_work": "正在取消已不需要的安排", "ask_question": "正在提出一个问题", "send_message": "正在发送消息", "read_conversation": "正在看聊天近况", "send_private": "正在准备私聊", "read_history": "正在翻阅聊天记录", "read_artifact": "正在查看成果", "read_tool_result": "正在核对已有材料", "read_memory": "正在回顾经历", "search_memory": "正在找回相关经历", "read_work": "正在查看实际进展", "delegate_work": "正在和同伴分工", "close_responsibility": "正在整理已完成的分工"}
		t.Activity(c.ID, m.ID, labels[call.Function.Name])
	}
	var result any
	var err error
	if call.Function.Name == reactionTool {
		result, err = t.reactToMessage(ctx, c, m, call, run.ID)
		if err == nil {
			return asJSON(map[string]any{"tool_run_id": run.ID, "status": "succeeded", "result": result}), nil
		}
	} else {
		result, err = t.perform(ctx, c, m, w, call)
	}
	status := "succeeded"
	if err != nil {
		status = "failed"
		result = map[string]any{"error": err.Error()}
		var transport *requestFailure
		if errors.As(err, &transport) {
			result = map[string]any{"error": err.Error(), "execution_error": true}
		}
		var pending *pendingResearchError
		if errors.As(err, &pending) {
			// 工具没有结束职责，但也没有发生执行故障。明确返回未完工作，
			// 模型可等待既有回报，不必把收尾反复尝试成新的后台任务。
			status = "waiting"
			result = map[string]any{"status": "waiting", "reason": err.Error(), "pending_work": pending.Work}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrStale) {
			status = "cancelled"
		}
	}
	// 完整工具返回值留在本地；旧上下文可以折叠，原始结果始终可回读。
	raw := asJSON(result)
	if _, saveErr := t.Store.db.Exec(`UPDATE tool_runs SET result=?,status=? WHERE id=?`, raw, status, run.ID); saveErr != nil {
		return "", saveErr
	}
	if call.Function.Name != reactionTool {
		t.Hub.Publish(c.ID)
	}
	// 即使失败也返回实际记录 ID，成员可以如实说明失败的具体来由。
	if errors.Is(err, ErrBudget) || errors.Is(err, ErrStale) || errors.Is(err, context.Canceled) {
		return "", err
	}
	return asJSON(map[string]any{"tool_run_id": run.ID, "status": status, "result": result}), nil
}

func (t *Toolbox) perform(ctx context.Context, c Conversation, m Member, w Work, call ToolCall) (any, error) {
	var args struct {
		ConversationID string   `json:"conversation_id"`
		ReplyTo        string   `json:"reply_to"`
		Evidence       []string `json:"evidence"`
		ID             string   `json:"id"`
		Scope          string   `json:"scope"`
		Body           string   `json:"body"`
		Query          string   `json:"query"`
		URL            string   `json:"url"`
		Title          string   `json:"title"`
		Content        string   `json:"content"`
		Format         string   `json:"format"`
		Instruction    string   `json:"instruction"`
		Reason         string   `json:"reason"`
		DelaySeconds   int      `json:"delay_seconds"`
		Message        string   `json:"message"`
		Notify         bool     `json:"notify"`
		Offset         int      `json:"offset"`
		Text           string   `json:"text"`
		Options        []string `json:"options"`
		Before         int64    `json:"before"`
		BeforeSeq      int64    `json:"before_seq"`
		MemberID       string   `json:"member_id"`
		DueAt          string   `json:"due_at"`
		Coordinate     bool     `json:"coordinate"`
		Limit          int      `json:"limit"`
		Length         int      `json:"length"`
		Version        int      `json:"version"`
		Refresh        bool     `json:"refresh"`
		Revision       *int     `json:"revision"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("工具参数格式不正确")
	}
	if args.Limit < 0 || args.Length < 0 {
		return nil, fmt.Errorf("读取数量不能为负数")
	}
	if args.Length == 0 {
		args.Length = 5000
	}
	if c.ID != "" {
		current, err := t.Store.Conversation(c.ID)
		if err != nil {
			return nil, err
		}
		if current.Revision != c.Revision {
			return nil, ErrStale
		}
	}
	var err error
	if args.ConversationID != "" && (call.Function.Name == "read_history" || call.Function.Name == "read_work" || call.Function.Name == "read_artifact" || call.Function.Name == "read_tool_result" || call.Function.Name == "read_memory" || call.Function.Name == "search_memory" || call.Function.Name == "remember") {
		c, err = t.Store.accessibleConversation(m.ID, args.ConversationID)
		if err != nil {
			return nil, err
		}
	}

	switch call.Function.Name {
	case "organize_context":
		return t.Store.organizeContext(c, m, call.Function.Arguments)
	case "send_message":
		return t.sendMessage(ctx, c, m, w, call, args.ConversationID, args.Message, args.ReplyTo, args.Evidence, args.Notify)
	case "read_conversation":
		room, err := t.Store.accessibleConversation(m.ID, args.ConversationID)
		if err != nil {
			return nil, err
		}
		room.Avatar = ""
		limit := args.Limit
		if limit == 0 {
			limit = 16
		}
		messages, err := t.Store.Messages(room.ID, 0, limit)
		if err != nil {
			return nil, err
		}
		memories, err := t.Store.relationshipMemories(m.ID, room.ID, append([]string{"user"}, room.MemberIDs...))
		return map[string]any{"conversation": room, "messages": messages, "relationships": memories}, err
	case "schedule_followup":
		due, err := followupTime(time.Now(), args.DelaySeconds, args.DueAt)
		if err != nil {
			return nil, err
		}
		return t.Store.ScheduleFollowup(c, m, w, args.Instruction, args.Reason, due)
	case "close_responsibility":
		return map[string]string{"responsibility_id": args.ID, "status": "closed"}, t.Store.CloseResponsibility(c, m, w, args.ID)
	case "cancel_work":
		return t.Store.CancelOwnWork(c, m, args.ID)
	case "ask_question":
		return t.Store.AskQuestion(c, m, Question{Text: args.Text, Options: args.Options}, args.Text, false)
	case "read_tool_result":
		if args.ID == "" {
			return t.toolIndex(c.ID, m.ID, args.Query, args.Offset, args.Limit)
		}
		var run ToolRun
		var originalArguments string
		if err := t.Store.db.QueryRow(`SELECT member_id,name,result,arguments,created_at FROM tool_runs WHERE id=? AND ((conversation_id='' AND member_id=?) OR conversation_id IN (SELECT conversation_id FROM participants WHERE member_id=?))`, args.ID, m.ID, m.ID).Scan(&run.MemberID, &run.Name, &run.Result, &originalArguments, &run.CreatedAt); err != nil {
			return nil, fmt.Errorf("当前聊天没有这条可读取的行动记录")
		}
		if !toolResultVisibleTo(run, m.ID) {
			return nil, fmt.Errorf("当前聊天没有这条可读取的行动记录")
		}
		if args.Query != "" {
			chars := []rune(run.Result)
			if args.Offset < 0 || args.Offset > len(chars) {
				return nil, fmt.Errorf("读取位置无效")
			}
			position := strings.Index(string(chars[args.Offset:]), args.Query)
			if position < 0 {
				return map[string]any{"tool_run_id": args.ID, "found": false}, nil
			}
			args.Offset += len([]rune(string(chars[args.Offset:])[:position]))
		}
		chars := []rune(run.Result)
		if args.Offset < 0 || args.Offset > len(chars) {
			return nil, fmt.Errorf("读取位置无效")
		}
		end := args.Offset + min(args.Length, len(chars)-args.Offset)
		return map[string]any{"tool_run_id": args.ID, "name": run.Name, "arguments": json.RawMessage(originalArguments), "created_at": run.CreatedAt, "content": string(chars[args.Offset:end]), "next_offset": end, "has_more": end < len(chars)}, nil
	case "read_memory":
		return t.Store.readMemory(m.ID, c.ID, args.ID, args.Revision)
	case "search", "read_page":
		// 原生参数完整传给供应商；refresh 只控制本地缓存。
		var body map[string]any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &body); err != nil {
			return nil, err
		}
		delete(body, "refresh")
		path := "fetch"
		if call.Function.Name == "search" {
			path = "search"
			if _, ok := body["count"]; !ok {
				body["count"] = 8
			}
			if _, ok := body["need_content"]; !ok {
				body["need_content"] = false
			}
		}
		return t.cachedWeb(ctx, c.ID, path, body, func() (map[string]any, error) {
			var out map[string]any
			if err := t.webRequest(withUsageCall(ctx, workUsage(call.Function.Name, m.ID, c, w)), path, body, &out); err != nil {
				return nil, err
			}
			if path == "fetch" {
				out["url"] = args.URL
			}
			return out, nil
		}, args.Refresh)

	case "remember":
		var memory memoryWrite
		if err := json.Unmarshal([]byte(call.Function.Arguments), &memory); err != nil {
			return nil, fmt.Errorf("经历参数格式不正确")
		}
		return t.remember(ctx, c, m, w, memory)
	case "read_history":
		return t.Store.searchHistory(c.ID, args.Query, args.ID, args.BeforeSeq, args.Offset, args.Limit, args.Length)
	case "search_memory":
		return t.Store.searchMemories(m.ID, c.ID, args.Query, args.Before, args.Limit)
	case "read_work":
		if args.ID != "" {
			w, err := t.Store.ReadWork(c.ID, args.ID)
			if err == nil && c.ID == "" && w.MemberID != m.ID {
				return nil, fmt.Errorf("这不是你的个人行动")
			}
			return roleWork(w), err
		}
		jobs, err := t.Store.OpenWork(c.ID)
		if err != nil {
			return nil, err
		}
		filtered := jobs[:0]
		for _, job := range jobs {
			if (c.ID != "" || job.MemberID == m.ID) && (args.MemberID == "" || args.MemberID == job.MemberID) {
				filtered = append(filtered, job)
			}
		}
		if args.Offset < 0 || args.Offset > len(filtered) {
			return nil, fmt.Errorf("读取位置无效")
		}
		limit := args.Limit
		if limit == 0 {
			limit = 20
		}
		end := args.Offset + min(limit, len(filtered)-args.Offset)
		page := filtered[args.Offset:end]
		for i := range page {
			page[i] = roleWork(page[i])
		}
		return map[string]any{"work": page, "next_offset": end, "has_more": end < len(filtered)}, nil
	case "delegate_work":
		assigned, err := t.Store.DelegateWork(c, m, w, args.MemberID, args.Instruction, args.Reason, args.Coordinate)
		return map[string]any{"work_id": assigned.ID, "work": assigned}, err
	case "save_artifact":
		return t.saveArtifact(ctx, c, m, w, Artifact{ID: args.ID, Title: args.Title, Content: args.Content, Format: args.Format})
	case "read_artifact":
		if args.ID == "" {
			return t.Store.artifactIndexForMember(c.ID, m.ID, args.Query, args.Offset, args.Limit)
		}
		list, err := t.Store.ArtifactVersions(args.ID)
		if err != nil {
			return nil, err
		}

		for _, artifact := range list {
			visible, err := t.Store.artifactVisible(c.ID, m.ID, artifact)
			if err != nil {
				return nil, err
			}
			if !visible {
				continue
			}
			if args.Version == 0 || artifact.Version == args.Version {
				chars := []rune(artifact.Content)
				if args.Offset < 0 || args.Offset > len(chars) {
					return nil, fmt.Errorf("读取位置无效")
				}
				end := args.Offset + min(args.Length, len(chars)-args.Offset)
				artifact.Content = string(chars[args.Offset:end])
				return map[string]any{"artifact": artifact, "next_offset": end, "has_more": end < len(chars)}, nil
			}
		}
		return nil, fmt.Errorf("没有这版成果")
	case "schedule_exploration":
		if args.DelaySeconds < 0 {
			return nil, fmt.Errorf("延时不能为负数")
		}
		due := time.Now().Add(time.Duration(args.DelaySeconds) * time.Second)
		if args.DueAt != "" {
			due, err = followupTime(time.Now(), args.DelaySeconds, args.DueAt)
			if err != nil {
				return nil, err
			}
		}
		return t.Schedule(ctx, c, m, w, args.Instruction, args.Reason, due)

	case "send_private":
		if strings.TrimSpace(args.Message) == "" {
			return nil, fmt.Errorf("私聊消息为空")
		}
		direct, err := t.Store.CreateConversation(m.Name, "direct", []string{m.ID}, t.Config.ExploreInterval)
		if err != nil {
			return nil, err
		}
		message := Message{ID: newID("msg"), ConversationID: direct.ID, AuthorID: m.ID, Body: args.Message, Notify: args.Notify, CreatedAt: now()}
		err = t.Store.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
			var paused bool
			if err := tx.QueryRow(`SELECT discussion_paused FROM conversations WHERE id=?`, direct.ID).Scan(&paused); err != nil {
				return err
			}
			if paused {
				return fmt.Errorf("私聊已暂停，不能绕过停止要求")
			}
			return insertMessage(tx, message)
		})
		if err == nil {
			t.Hub.Publish(direct.ID)
		}
		return map[string]string{"conversation_id": direct.ID, "message_id": message.ID, "message": message.Body}, err
	}
	return nil, fmt.Errorf("未知工具")
}

func (t *Toolbox) Schedule(ctx context.Context, c Conversation, m Member, source Work, instruction, reason string, due time.Time) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.ResearchPaused {
		return nil, fmt.Errorf("研究已停止")
	}
	if strings.TrimSpace(instruction) == "" {
		return nil, fmt.Errorf("后台行动需要具体内容")
	}
	w := Work{ID: newID("work"), ParentID: source.ID, ConversationID: c.ID, MemberID: m.ID, RootID: rootOf(source), Autonomous: source.Autonomous, Kind: "research", Instruction: instruction, Reason: reason, SourceID: source.SourceID, Status: "pending", DueAt: due.UnixMilli(), CreatedAt: now()}
	err := t.Store.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		return insertWork(tx, w)
	})
	return map[string]any{"work_id": w.ID, "due_at": w.DueAt, "status": "pending"}, err
}

func (t *Toolbox) webRequest(ctx context.Context, path string, body, out any) error {
	if t.Config.SearchKey == "" {
		return fmt.Errorf("尚未配置搜索服务，不能执行真实网页探索")
	}
	return retryRequest(ctx, func() error {
		purpose, model := "read_page", "DeepSeek Fetch"
		if path == "search" {
			purpose, model = "search", "DeepSeek Search"
		}
		call, err := t.Store.reserveCall(ctx, t.Config, t.Config.ToolCostMicros, "DeepSeek", model, purpose)
		if err != nil {
			return err
		}
		defer t.Store.finishUsageCall(call)
		var response map[string]any
		err = requestJSON(ctx, &http.Client{Timeout: 45 * time.Second}, t.Config.SearchURL+"/"+path, t.Config.SearchKey, []byte(asJSON(body)), &response)
		if err != nil {
			var failure *requestFailure
			// 收到供应商非成功状态可确认免费；断线或本地超时不能推断服务端结果。
			if errors.As(err, &failure) && failure.Status != 0 {
				if settleErr := t.Store.settleCall(call, 0, responseUsage{}, "released"); settleErr != nil {
					return settleErr
				}
			}
			return err
		}
		cost, outcome := t.Config.ToolCostMicros, ""
		if path == "search" {
			results, ok := response["results"].([]any)
			if !ok {
				return fmt.Errorf("搜索响应缺少有效 results 数组，计费结果未知")
			}
			for _, item := range results {
				entry, ok := item.(map[string]any)
				if !ok {
					return fmt.Errorf("搜索结果结构异常，计费结果未知")
				}
				if _, ok := entry["url"].(string); !ok {
					return fmt.Errorf("搜索结果缺少链接，计费结果未知")
				}
			}
			if len(results) == 0 {
				cost = 0
				outcome = "empty"
			}
		} else {
			content, ok := response["content"].(string)
			if !ok {
				return fmt.Errorf("网页响应缺少正文，计费结果未知")
			}
			if strings.TrimSpace(content) == "" {
				if err := t.Store.settleCall(call, 0, responseUsage{}, "empty"); err != nil {
					return err
				}
				return fmt.Errorf("网页没有返回可用正文")
			}
		}
		if err := t.Store.settleCall(call, cost, responseUsage{}, outcome); err != nil {
			return err
		}
		data, _ := json.Marshal(response)
		return json.Unmarshal(data, out)
	})

}

func decode(s string, out any) { _ = json.Unmarshal([]byte(s), out) }

// 这些现有工具只读取数据；结果未落盘的读取可以重做。写入/发送不走这条路径。
func readOnlyTool(name string) bool {
	switch name {
	case "organize_context", "search", "read_page", "read_conversation", "read_history", "read_memory", "search_memory", "read_work", "read_artifact", "read_tool_result":
		return true
	}
	return false
}
