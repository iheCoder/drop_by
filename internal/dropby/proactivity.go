package dropby

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 这些状态只描述调度和资源边界，不为性格、兴趣或情绪打分。
type autonomyKey struct{}

// 自然语言里的“明早九点”由模型根据当前时区转成绝对时刻，执行层只核对
// 时间格式与真实未来时间，不用字符串猜日期。现有相对预约仍然兼容。
func followupTime(at time.Time, delay int, absolute string) (time.Time, error) {
	if absolute == "" {
		if delay <= 0 {
			return time.Time{}, fmt.Errorf("预约延时必须为正数")
		}
		return at.Add(time.Duration(delay) * time.Second), nil
	}
	if delay != 0 {
		return time.Time{}, fmt.Errorf("定时时刻和相对延时只能填写一个")
	}
	due, err := time.Parse(time.RFC3339, absolute)
	if err != nil || !due.After(at) {
		return time.Time{}, fmt.Errorf("定时需要带时区的未来时刻，请重新核对时间")
	}
	return due, nil
}

func autonomousWork(w Work) bool {
	return w.Autonomous || w.Kind == "revisit" || (w.Kind != "chat" && w.SourceID == "")
}

func autonomyRoot(ctx context.Context) string {
	root, _ := ctx.Value(autonomyKey{}).(string)
	return root
}

func rootOf(w Work) string {
	if w.RootID != "" {
		return w.RootID
	}
	return w.ID
}

func (s *Store) initProactivity() error {
	// 增量迁移保留历史数据；SQLite 的 pragma 支持在旧数据库中检查已有列。
	for _, col := range []struct{ table, name, definition string }{
		{"work", "root_id", "TEXT NOT NULL DEFAULT ''"},
		{"work", "autonomous", "INTEGER NOT NULL DEFAULT 0"},
		{"conversations", "idle_rounds", "INTEGER NOT NULL DEFAULT 0"},
		{"usage", "autonomous_cost_micros", "INTEGER NOT NULL DEFAULT 0"},
		{"usage", "unsettled_micros", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var exists int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, col.table, col.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := s.db.Exec(`ALTER TABLE ` + col.table + ` ADD COLUMN ` + col.name + ` ` + col.definition); err != nil {
				return err
			}
		}
	}
	_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS work_root ON work(root_id);
UPDATE work SET autonomous=1 WHERE source_id='' AND kind!='chat';`)
	return err
}

// 闲时检查逐渐放缓。一次新真人交流会重置间隔；BOT 自己的消息不会刷新配额。
// 到期的具体后续另走工作队列，不受这一闲时退避影响，也不会在休眠后补发每次错过的机会。
func (s *Store) ScheduleExploration(interval time.Duration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT c.id,c.idle_rounds FROM conversations c WHERE c.next_explore<=? AND c.research_paused=0
AND NOT EXISTS(SELECT 1 FROM work w WHERE w.conversation_id=c.id AND (w.status IN ('running','waiting_budget') OR (w.status='pending' AND (w.kind IN ('chat','explore') OR w.due_at<=?))))`, time.Now().UnixMilli(), time.Now().UnixMilli())
	if err != nil {
		return err
	}
	type idleRoom struct {
		id     string
		rounds int
	}
	var rooms []idleRoom
	for rows.Next() {
		var r idleRoom
		if err := rows.Scan(&r.id, &r.rounds); err != nil {
			rows.Close()
			return err
		}
		rooms = append(rooms, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range rooms {
		w := Work{ID: newID("work"), ConversationID: r.id, Kind: "explore", Autonomous: true, Instruction: "这是一次闲时观察机会。结合自己的特质、当前关注、共同经历和实际进展决定下一步。", Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
		if err := insertWork(tx, w); err != nil {
			return err
		}
		delay := min(interval*time.Duration(1<<min(r.rounds+1, 5)), 6*time.Hour)
		if _, err := tx.Exec(`UPDATE conversations SET idle_rounds=idle_rounds+1,next_explore=? WHERE id=?`, time.Now().Add(delay).UnixMilli(), r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// 明确的后续只预约一次重新考虑的机会，不预约必须发送的消息。
func (s *Store) ScheduleFollowup(c Conversation, m Member, source Work, instruction, reason string, due time.Time) (any, error) {
	if c.ResearchPaused {
		return nil, fmt.Errorf("自主后续已暂停")
	}
	if strings.TrimSpace(instruction) == "" || strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("后续需要具体关注和实际来由")
	}
	w := Work{ID: newID("work"), ParentID: source.ID, RootID: rootOf(source), ConversationID: c.ID, MemberID: m.ID, Kind: "revisit", Autonomous: true, Instruction: clip(instruction, 5000), Reason: clip(reason, 1500), SourceID: source.SourceID, Status: "pending", DueAt: due.UnixMilli(), CreatedAt: now()}
	err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		// 同一成员已有完全相同的预约时更新时机，不堆积重复待办。
		var existing string
		err := tx.QueryRow(`SELECT id FROM work WHERE conversation_id=? AND member_id=? AND kind='revisit' AND instruction=? AND status IN ('pending','waiting_budget')`, c.ID, m.ID, w.Instruction).Scan(&existing)
		if err == nil {
			w.ID = existing
			_, err = tx.Exec(`UPDATE work SET due_at=?,reason=? WHERE id=?`, w.DueAt, w.Reason, w.ID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return insertWork(tx, w)
	})
	return map[string]any{"work_id": w.ID, "due_at": w.DueAt, "status": "pending"}, err
}

func (s *Store) CancelOwnWork(c Conversation, m Member, id string) (any, error) {
	var status string
	err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		// 先确认归属与类型。已完成/已停止的本人工作可以再次确认结束，
		// 但不能把同伴工作、其他聊天或未知 ID 当成一次成功的取消。
		if err := tx.QueryRow(`SELECT status FROM work WHERE id=? AND conversation_id=? AND member_id=? AND kind IN ('research','revisit')`, id, c.ID, m.ID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("只能取消自己在当前聊天中的研究或后续")
			}
			return err
		}
		switch status {
		case "done", "stopped", "superseded":
			return nil // 保留原有结果与实际终态，不把 done 改成 stopped。
		case "pending", "waiting_budget", "paused", "failed":
			_, err := tx.Exec(`UPDATE work SET status='stopped',result=CASE WHEN result='' THEN '成员结合最新情境取消了这项后续。' ELSE result END WHERE id=?`, id)
			status = "stopped"
			return err
		default:
			return fmt.Errorf("这项工作正在执行，尚未取消")
		}
	})
	return map[string]string{"work_id": id, "status": status}, err
}
