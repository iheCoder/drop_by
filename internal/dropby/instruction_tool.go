package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// 指令由正在参与的成员理解。这个工具只负责落实明确的共享变化，普通聊天无需调用。
// 复用既有 Control 写入路径，避免另一份协调状态或另一轮模型审批。
func instructionDefinition() ToolDefinition {
	array := func(props map[string]any) any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": props, "additionalProperties": false}}
	}
	ids := map[string]any{"type": "array", "items": map[string]string{"type": "string"}}
	return definition("apply_instruction", "落实真人明确提出的暂停、继续、工作调整、人物设定或组织职责。普通聊天无需调用。source_id 引用实际真人消息；只填写需要改变的部分。职责可长期或限于一件事，撤销职责不等于取消工作。", map[string]any{
		"conversation_id": textField("目标聊天，省略为当前聊天"), "source_id": textField("明确指示的真人消息 ID"),
		"discussion": map[string]any{"type": "string", "enum": []string{"keep", "pause", "resume"}}, "research": map[string]any{"type": "string", "enum": []string{"keep", "pause", "resume"}},
		"related_work_id": textField("已有事项 ID，交接或继续该事项时填写"),
		"stop_ids":        ids, "resume_ids": ids, "complete_ids": ids,
		"updates":                array(map[string]any{"id": textField("实际工作 ID"), "instruction": textField("调整后的方向")}),
		"responsibility_changes": array(map[string]any{"id": textField("修改或撤销已有职责时填写"), "member_id": textField("承担职责的成员"), "scope_id": textField("空串为长期；this 为本次事项；也可用已有工作 ID"), "body": textField("职责内容"), "revoke": map[string]string{"type": "boolean"}}),
		"preference_updates": array(map[string]any{
			"member_id":         textField("成员 ID"),
			"profile":           textField("用户明确修改特质或回答角色设置问题时，直接给出调整后的完整人物说明；保留其他特点。含义不清的举例保持原有抽象程度。仅与用户相处的长期约定用 remember，本次要求留在当前交流"),
			"source_id":         textField("同一真人消息 ID"),
			"expected_revision": map[string]any{"type": "integer", "description": "目标成员当前人物说明的版本；修改自己时取 self.profile_revision，未取得时省略"},
			"change_note":       textField("简述这次修改了什么"),
		}),
	}, "source_id")
}

func (t *Toolbox) applyInstruction(ctx context.Context, c Conversation, m Member, w Work, call ToolCall) (any, error) {
	var args struct {
		Control
		ConversationID string `json:"conversation_id"`
		SourceID       string `json:"source_id"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return nil, err
	}
	if args.ConversationID != "" {
		var err error
		c, err = t.Store.accessibleConversation(m.ID, args.ConversationID)
		if err != nil {
			return nil, err
		}
	}
	// 来源是原消息，不把成员对网页或同伴的理解变成人物编辑授权。
	var source Work
	err := t.Store.db.QueryRow(`SELECT w.id,m.body FROM messages m JOIN work w ON w.source_id=m.id AND w.kind='chat' WHERE m.id=? AND m.conversation_id=? AND m.author_id='user' ORDER BY w.created_at LIMIT 1`, args.SourceID, c.ID).Scan(&source.ID, &source.Instruction)
	if err != nil {
		return nil, fmt.Errorf("找不到对应的真人指示：%w", err)
	}
	source.ConversationID, source.SourceID = c.ID, args.SourceID
	args.Control.receiptID = "tool_" + sentMessageID(w, m, call)[4:]
	if w.ConversationID == "" {
		args.Control.personalWorkID = w.ID
	}
	args.Control.Guidance = c.Guidance // 普通理解不再汇总成公共 guidance。
	e := &Engine{Store: t.Store}
	if err = e.applyControl(ctx, c, source, args.Control); err != nil {
		return nil, err
	}
	t.Hub.Publish(c.ID)
	return map[string]string{"status": "applied", "conversation_id": c.ID}, nil
}

// 问题卡与普通消息共享调用身份和持久唤醒。成功回执丢失后重试不会多发一个问题。
func (t *Toolbox) sendQuestion(ctx context.Context, c Conversation, m Member, w Work, call ToolCall, body string, options []string) (any, error) {
	if c.ID == "" {
		var err error
		c, err = t.Store.CreateConversation(m.Name, "direct", []string{m.ID}, t.Config.ExploreInterval)
		if err != nil {
			return nil, err
		}
	}
	if c.DiscussionPaused {
		return nil, fmt.Errorf("目标聊天已暂停发言")
	}
	if err := validateQuestion(Question{Text: body, Options: options}); err != nil {
		return nil, err
	}
	msg := Message{ID: sentMessageID(w, m, call), ConversationID: c.ID, AuthorID: m.ID, Body: body, Question: &Question{Text: body, Options: options}, CreatedAt: now()}
	err := t.Store.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRow(`SELECT count(*) FROM messages WHERE id=?`, msg.ID).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return nil
		}
		if err := publishMessage(tx, msg); err != nil {
			return err
		}
		return wakeConversation(tx, msg, w)
	})
	if err == nil {
		t.Hub.Publish(c.ID)
	}
	return msg, err
}
