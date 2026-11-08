package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// 指定给一个人的预约或研究由本人执行，消息再唤醒聊天。研究结束与群里同伴
// 的回应不共用终态：同伴回复失败不能把已完成研究重做，研究很慢也不占用全群。
func directedWork(w Work) bool {
	return w.MemberID != "" && (w.Kind == "research" || w.Kind == "revisit" || w.Kind == "share")
}

// 每位成员独立行动。两人的交流可以继续，不必等第三人读完资料或从故障中恢复。
func (e *Engine) converse(ctx context.Context, c Conversation, w Work) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	events, unsubscribe := e.Hub.Subscribe()
	defer unsubscribe()
	type result struct {
		member string
		seen   int64
		err    error
	}
	completed := make(chan result, len(c.MemberIDs))
	busy, failed, started := map[string]bool{}, map[string]int64{}, map[string]bool{}
	seen := map[string]int64{}
	for _, id := range c.MemberIDs {
		last, _, err := e.Store.observation(c.ID, id)
		if err != nil {
			return err
		}
		seen[id] = last
	}
	start := func(id string) {
		busy[id] = true
		actionWork := w
		if started[id] || (w.MemberID != "" && id != w.MemberID) {
			actionWork.Kind = "followup"
		}
		started[id] = true
		workers.Add(1)
		go func() {
			defer workers.Done()
			m, err := e.Store.Member(id)
			var last int64
			if err == nil {
				last, err = e.memberAction(ctx, c, m, actionWork)
			}
			completed <- result{id, last, err}
		}()
	}
	initial := c.MemberIDs
	pending, err := e.Store.pendingActions(w.ID)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		initial = pending
	}
	// 续接时只有待恢复者先执行；同伴后来发来的新消息仍会带来新的交流机会。
	var baseline int64
	if err = e.Store.db.QueryRow(`SELECT coalesce(max(seq),0) FROM messages WHERE conversation_id=?`, c.ID).Scan(&baseline); err != nil {
		return err
	}
	for _, id := range c.MemberIDs {
		if !contains(initial, id) {
			seen[id] = baseline
		}
	}
	if w.Kind == "followup" && w.SourceID != "" {
		var author string
		var sourceSeq int64
		if err := e.Store.db.QueryRow(`SELECT author_id,seq FROM messages WHERE id=?`, w.SourceID).Scan(&author, &sourceSeq); err != nil && err != sql.ErrNoRows {
			return err
		}
		filtered := []string{}
		for _, id := range initial {
			if id == author {
				// 发出消息本身不唤醒自己；同一批通知中别人后来的回应仍应被看见。
				seen[id] = max(seen[id], sourceSeq)
				var later int
				if err := e.Store.db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id=? AND author_id<>? AND seq>?`, c.ID, id, seen[id]).Scan(&later); err != nil {
					return err
				}
				if later == 0 {
					continue
				}
			}
			filtered = append(filtered, id)
		}
		initial = filtered
	}

	for _, id := range initial {
		if !contains(c.MemberIDs, id) {
			continue
		}
		var handled, unseen int
		if err = e.Store.db.QueryRow(`SELECT count(*) FROM participation WHERE work_id=? AND member_id=? AND action<>'error'`, w.ID, id).Scan(&handled); err != nil {
			return err
		}
		if err = e.Store.db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id=? AND author_id<>? AND seq>?`, c.ID, id, seen[id]).Scan(&unseen); err != nil {
			return err
		}
		// 已在运行中的交流读过这批消息，持久通知只需确认消费，不再重复叫一次模型。
		freshOpportunity := handled == 0 && !(w.Kind == "followup" && w.SourceID != "")
		if w.Kind == "chat" && unseen == 0 {
			freshOpportunity = false
		}
		if contains(pending, id) || freshOpportunity || unseen > 0 {
			start(id)
		}
	}
	failures := map[string]error{}
	for len(busy) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r := <-completed:
			delete(busy, r.member)
			seen[r.member] = max(seen[r.member], r.seen)
			if r.err != nil {
				// 只记下失败时已到达的位置；之后的新消息仍可触发本人。
				var latest int64
				if err := e.Store.db.QueryRow(`SELECT coalesce(max(seq),0) FROM messages WHERE conversation_id=? AND author_id<>?`, c.ID, r.member).Scan(&latest); err != nil {
					return err
				}
				if cp, err := e.Store.loadAction(w.ID, r.member); err == nil {
					latest = cp.Seen
				}
				failed[r.member] = latest
				failures[r.member] = r.err
			}
			if r.err == nil {
				delete(failures, r.member)
			}
		case room := <-events:
			if room != c.ID {
				continue
			}
		}
		// 新收到别人的话会唤醒已结束的行动；自己的消息不自动唤醒自己。
		// 本人可以在同一行动中通过 send_message 继续说，不必靠自发事件续命。
		for _, id := range c.MemberIDs {
			if busy[id] {
				continue
			}
			var latest int64
			if err := e.Store.db.QueryRow(`SELECT coalesce(max(seq),0) FROM messages WHERE conversation_id=? AND author_id<>?`, c.ID, id).Scan(&latest); err != nil {
				return err
			}
			if latest > max(seen[id], failed[id]) {
				start(id)
			}
		}
	}
	var failure error
	for _, err := range failures {
		failure = errors.Join(failure, err)
	}
	return failure
}

func (e *Engine) memberAction(ctx context.Context, c Conversation, m Member, w Work) (int64, error) {
	unlock, err := e.lockMember(ctx, m.ID)
	if err != nil {
		return 0, err
	}
	defer unlock()
	// 发布和回执已提交、调度终态还没写入时可能发生重启。没有未完现场就不用重演个人活动。
	if c.ID == "" && w.Kind != "research" {
		var completed int
		if err = e.Store.db.QueryRow(`SELECT count(*) FROM participation WHERE work_id=? AND member_id=? AND action<>'error' AND NOT EXISTS(SELECT 1 FROM action_checkpoints WHERE work_id=? AND member_id=?)`, w.ID, m.ID, w.ID, m.ID).Scan(&completed); err != nil {
			return 0, err
		}
		if completed > 0 {
			return 0, nil
		}
	}
	instruction := "结合当前现场，以自己的身份行动或交流。"
	if c.Kind == "personal" {
		instruction = "这是你的个人活动机会。可以从自己的兴趣、经历、对人的了解或新接触的东西出发，使用工具、创作或找人交流；当前聊天不是唯一的方向。"
	} else if w.Kind == "research" || w.Kind == "revisit" || w.Kind == "share" {
		instruction = "结合当前情况继续自己的这项行动。"
	}
	out, err := e.actLocked(ctx, c, m, w, instruction)
	if errors.Is(err, errActionStopped) {
		return out.seen, nil
	}
	if err != nil {
		if !errors.Is(err, errActionYield) {
			_ = e.recordMemberAction(w, m.ID, "error", err.Error())
		}
		return out.seen, err
	}
	target := c
	var msg *Message
	if strings.TrimSpace(out.Message) != "" {
		if out.ConversationID != "" {
			target, err = e.Store.accessibleConversation(m.ID, out.ConversationID)
		}
		if err == nil && target.ID == "" {
			target, err = e.Store.CreateConversation(m.Name, "direct", []string{m.ID}, e.Config.ExploreInterval)
		}
		if err != nil {
			return out.seen, err
		}
		cp, loadErr := e.Store.loadAction(w.ID, m.ID)
		if loadErr != nil {
			return out.seen, loadErr
		}
		msg = &Message{ID: cp.ID, ConversationID: target.ID, AuthorID: m.ID, Body: out.Message, ReplyTo: out.ReplyTo, Evidence: out.Evidence, Notify: out.Notify, CreatedAt: now()}
	}
	// 消息、已完成回执、断点删除在同一事务中：恢复不会重复发言或重新执行已完成行动。
	err = e.Store.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if msg != nil {
			var revision int64
			if err := tx.QueryRow(`SELECT revision FROM conversations WHERE id=?`, target.ID).Scan(&revision); err != nil {
				return err
			}
			if revision != target.Revision {
				return ErrStale
			}
			if err := publishMessage(tx, *msg); err != nil {
				return err
			}
			// 兼容以最终正文完成的行动：自己的已发布发言也是实际经历。
			// 与发布一并记录引用，不要求再来一条消息才进入后台可读材料。
			if _, err := tx.Exec(`INSERT OR IGNORE INTO profile_experiences(member_id,source_id) VALUES(?,?)`, m.ID, msg.ID); err != nil {
				return err
			}
			if target.ID != c.ID || directedWork(w) {
				if err := wakeConversation(tx, *msg, w); err != nil {
					return err
				}
			}
		}
		action := "quiet"
		if msg != nil {
			action = "speak"
		}
		var usedTools int
		if err := tx.QueryRow(`SELECT count(*) FROM tool_runs WHERE work_id=? AND member_id=?`, w.ID, m.ID).Scan(&usedTools); err != nil {
			return err
		}
		if usedTools > 0 {
			action = "act"
		}
		if _, err := tx.Exec(`INSERT INTO participation VALUES(?,?,?,?,?,?)`, w.ID, m.ID, action, clip(out.Outcome, 500), true, now()); err != nil {
			return err
		}
		// 窗口与行动完成原子落盘；静默结束也保留连续理解，崩溃后不会重放已发消息。
		var raw string
		var seen int64
		if err := tx.QueryRow(`SELECT messages,seen FROM action_checkpoints WHERE work_id=? AND member_id=?`, w.ID, m.ID).Scan(&raw, &seen); err != nil {
			return err
		}
		var history []ChatMessage
		if err := json.Unmarshal([]byte(raw), &history); err != nil {
			return err
		}
		// 到此最终结果已消费；只从续接窗口移除运行结果，断点在事务提交前仍完整。
		if _, err := tx.Exec(`INSERT INTO working_contexts(conversation_id,member_id,seen,messages) VALUES(?,?,?,?) ON CONFLICT(conversation_id,member_id) DO UPDATE SET seen=excluded.seen,messages=excluded.messages`, c.ID, m.ID, seen, asJSON(completedHistory(history))); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM action_checkpoints WHERE work_id=? AND member_id=?`, w.ID, m.ID); err != nil {
			return err
		}
		if w.Kind == "research" {
			status := "done"
			if out.Unfinished {
				status = "pending"
			}
			if _, err := tx.Exec(`UPDATE work SET status=?,result=?,due_at=? WHERE id=? AND status='running'`, status, out.Outcome, time.Now().UnixMilli(), w.ID); err != nil {
				return err
			}
			return queueCoordinationReview(tx, w, status, out.Outcome)
		}
		if c.ID == "" {
			_, err := tx.Exec(`UPDATE work SET status='done',result=? WHERE id=? AND status='running'`, out.Outcome, w.ID)
			return err
		}
		return nil
	})
	if err != nil {
		return out.seen, err
	}
	if msg != nil {
		e.Hub.Publish(target.ID)
	}
	if out.Unfinished && w.Kind != "research" && w.Kind != "share" {
		_, err = e.Tools.Schedule(ctx, c, m, w, out.Outcome, "", time.Now())
	}
	return out.seen, err
}

func (e *Engine) recordMemberAction(w Work, member, action, reason string) error {
	if w.ID == "" {
		return nil
	}
	_, err := e.Store.db.Exec(`INSERT INTO participation VALUES(?,?,?,?,?,?)`, w.ID, member, action, clip(reason, 500), true, now())
	return err
}

// 对方房间即使正在处理其他工作，也留下一个持久唤醒。只合并尚未执行的通知，
// 不把“正在运行”当作已经读到新消息；否则工作恰好结束时会漏掉新发言。
func wakeConversation(tx *sql.Tx, msg Message, parent Work) error {
	var existing string
	err := tx.QueryRow(`SELECT id FROM work WHERE conversation_id=? AND kind='followup' AND status='pending' LIMIT 1`, msg.ConversationID).Scan(&existing)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	return insertWork(tx, Work{ID: newID("work"), ConversationID: msg.ConversationID, Kind: "followup", SourceID: msg.ID, RootID: rootOf(parent), Autonomous: parent.Autonomous, Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()})
}
