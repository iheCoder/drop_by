package dropby

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// 这里登记的是已经交付的消息 ID，不在前台提炼含义。一个来源只登记一次，
// 后台使用自己的游标合并处理；旁观同一聊天的成员仍各自形成理解。
func (s *Store) recordProfileMessages(member string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, message := range messages {
		if message.ID == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO profile_experiences(member_id,source_id)
 SELECT ?,id FROM messages WHERE id=?`, member, message.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// 工具回执也属于实际经历，包括失败。只记录引用，不复制正文，不增加一次模型调用。
func (s *Store) recordProfileTool(member, id string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO profile_experiences(member_id,source_id)
 SELECT ?,id FROM tool_runs WHERE id=? AND member_id=? AND status<>'running'`, member, id, member)
	return err
}

// 沿用现有个人活动间隔合并后台调用；间隔只控制调度，不决定人格何时“成熟”。
// 老成员从新交付开始处理，不在升级时批量重做历史人物设定。
func (s *Store) scheduleProfileReflections(interval time.Duration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT r.member_id FROM profile_reflections r WHERE r.next_at<=?
 AND EXISTS(SELECT 1 FROM profile_experiences x WHERE x.member_id=r.member_id AND x.seq>r.after_seq)
 AND EXISTS(SELECT 1 FROM participants p JOIN conversations c ON c.id=p.conversation_id WHERE p.member_id=r.member_id AND c.research_paused=0)
 AND NOT EXISTS(SELECT 1 FROM work w WHERE w.member_id=r.member_id AND w.kind='reflect'
 AND w.status IN ('pending','running','waiting_budget','paused'))`, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	var members []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		members = append(members, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, member := range members {
		w := Work{ID: newID("work"), MemberID: member, Kind: "reflect", Autonomous: true,
			Instruction: "整理自己的人物说明", Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
		if err := insertWork(tx, w); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE profile_reflections SET next_at=? WHERE member_id=?`, time.Now().Add(interval).UnixMilli(), member); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// 待提交结果只存在于这次 Work 中，尚不是生效的人格。保存它是为了前台到来时
// 可以让路，稍后提交，不需要重新支付已经完成的整理调用。
type profileReflection struct {
	Revision   int           `json:"profile_revision"`
	ThroughSeq int64         `json:"through_seq"`
	Update     profileUpdate `json:"update"`
}

func (s *Store) profileReflectionInput(m Member) ([]MemoryEvidence, int64, error) {
	rows, err := s.db.Query(`SELECT x.seq,x.source_id FROM profile_experiences x
 JOIN profile_reflections r ON r.member_id=x.member_id
 WHERE x.member_id=? AND x.seq>r.after_seq ORDER BY x.seq`, m.ID)
	if err != nil {
		return nil, 0, err
	}
	var ids []string
	var through int64
	for rows.Next() {
		var id string
		if err := rows.Scan(&through, &id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	evidence, err := s.memoryEvidence(m.ID, "", ids, nil)
	return evidence, through, err
}

func (e *Engine) reflectProfile(ctx context.Context, w Work) error {
	m, err := e.Store.Member(w.MemberID)
	if err != nil {
		return err
	}
	var result profileReflection
	if json.Unmarshal([]byte(w.Result), &result) != nil || result.ThroughSeq == 0 || result.Revision != m.ProfileRevision {
		// 每次整理有自己的输入。前台的工作窗口、任务指令、压缩便笺不参与此请求。
		evidence, through, err := e.Store.profileReflectionInput(m)
		if err != nil {
			return err
		}
		if through == 0 {
			return nil
		}
		result = profileReflection{Revision: m.ProfileRevision, ThroughSeq: through}
		ctx = withUsageCall(ctx, workUsage("personality", m.ID, Conversation{}, w))
		messages := []ChatMessage{
			{Role: "system", Content: profileEditingPrompt + "\n" + profileSelfPrompt},
			{Role: "user", Content: asJSON(map[string]any{"self": map[string]any{"id": m.ID, "name": m.Name, "profile": m.Profile}, "experiences": evidence})},
		}
		c, err := e.Store.personalConversation(m.ID)
		if err != nil {
			return err
		}
		defs := e.Tools.Definitions(m, c, w)
		for {
			reply, err := completeModel(ctx, e.Model, &messages, defs, true)
			if err != nil {
				return err
			}
			messages = append(messages, reply)
			if len(reply.ToolCalls) == 0 {
				if err := parseJSON(reply.Content, &result.Update); err != nil {
					return err
				}
				break
			}
			// 允许按需回查原文，完整结果仍用现有工具日志保存。后台没有发消息、
			// 创作、安排任务或修改其他成员的工具，不把整理变成又一次自主行动。
			for _, call := range reply.ToolCalls {
				value, err := e.Tools.Execute(ctx, c, m, w, call)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					value = asJSON(map[string]string{"error": err.Error()})
				}
				messages = append(messages, ChatMessage{Role: "tool", ToolCallID: call.ID, Content: value})
			}
		}
		if _, err := e.Store.memoryEvidence(m.ID, "", result.Update.SourceIDs, nil); err != nil {
			return err
		}
		if _, err := e.Store.db.Exec(`UPDATE work SET result=? WHERE id=? AND status IN ('pending','running')`, asJSON(result), w.ID); err != nil {
			return err
		}
	}

	// 模型运行期间从未持有成员锁。只在提交这个短事务时尝试取锁，前台仍在
	// 执行或等待时直接让路；下一次调度使用上面已保存的结果。
	gate := e.gate(m.ID)
	if gate.waiting.Load() > 0 {
		return errActionYield
	}
	select {
	case gate.slot <- struct{}{}:
		defer func() { <-gate.slot }()
	default:
		return errActionYield
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	tx, err := e.Store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRow(`SELECT data FROM members WHERE id=?`, m.ID).Scan(&raw); err != nil {
		return err
	}
	var current Member
	if err := json.Unmarshal([]byte(raw), &current); err != nil {
		return err
	}
	if current.ProfileRevision != result.Revision {
		return errActionYield
	}
	if result.Update.Profile != nil {
		if _, err := setMemberProfile(tx, &current, *result.Update.Profile, "reflection", result.Update.ChangeNote, result.Update.SourceIDs); err != nil {
			return err
		}
	}
	// 没有修改也消耗这批来源；新到的经历留给下一批。光标与人物保存原子提交。
	if _, err := tx.Exec(`UPDATE profile_reflections SET after_seq=max(after_seq,?) WHERE member_id=?`, result.ThroughSeq, m.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE work SET status='done',result=? WHERE id=?`, asJSON(result), w.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	e.Hub.Publish("")
	return nil
}

// 前台到来取消低优先级请求，但不等待其返回。若调用已经完成，待提交结果仍可
// 留在 Work 中；取消不会修改人物、聊天或前台上下文。
func (e *Engine) yieldProfileReflections() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, cancel := range e.running {
		if strings.HasPrefix(key, "reflection:") {
			cancel()
		}
	}
}

// 错误留在工作记录中；已生成的结果应保留，避免让路一次就再调用模型一次。
func (s *Store) reflectionResult(id string) (string, error) {
	var raw string
	if err := s.db.QueryRow(`SELECT result FROM work WHERE id=?`, id).Scan(&raw); err != nil {
		return "", err
	}
	var result profileReflection
	if json.Unmarshal([]byte(raw), &result) == nil && result.ThroughSeq > 0 {
		return raw, nil
	}
	return "", nil
}

// 普通任务和后台整理使用同一套真实来源访问规则，不增加内容核验或表达审核。
func reflectionReadTool(name string) bool {
	switch name {
	case "read_history", "read_memory", "search_memory", "read_tool_result", "read_artifact", "read_work", "read_conversation":
		return true
	}
	return false
}
