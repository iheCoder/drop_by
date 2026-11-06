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

// 每位成员每个根任务最多安排六次实际后续；分享已完成结果不占这个名额。
const maxChainedActions = 6

func autonomousWork(w Work) bool {
	return w.Autonomous || w.Kind == "revisit" || (w.Kind != "chat" && w.SourceID == "")
}

var errAutonomyLimit = errors.New("这次自主交流已达到运行边界，保留进展并安静结束")
var errAutonomyBudget = fmt.Errorf("今日自主探索额度已用完，真人聊天仍可继续：%w", ErrBudget)

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
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS autonomous_publications(root_id TEXT PRIMARY KEY, reserved INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS work_root ON work(root_id);
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
		w := Work{ID: newID("work"), ConversationID: r.id, Kind: "explore", Autonomous: true, Instruction: "这是一次稀疏的闲时自主机会。结合自己的兴趣、当前挂念、共同经历和真实进展，可以尝试一个具体小方向、自然分享，也可以安静结束。不要求延续最后一个话题；已经聊充分的内容不用重开，不催回复。想稍后再考虑一件具体事情时可安排 followup，不必每次都安排。", Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
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
	w := Work{ID: newID("work"), RootID: rootOf(source), ConversationID: c.ID, MemberID: m.ID, Kind: "revisit", Autonomous: true, Instruction: clip(instruction, 5000), Reason: clip(reason, 1500), SourceID: source.SourceID, Status: "pending", DueAt: due.UnixMilli(), CreatedAt: now()}
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
	err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		r, err := tx.Exec(`UPDATE work SET status='stopped',result='成员结合最新情境取消了这项后续。' WHERE id=? AND conversation_id=? AND member_id=? AND kind IN ('research','revisit') AND status IN ('pending','waiting_budget','paused')`, id, c.ID, m.ID)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("只能取消自己的、当前聊天中尚未执行的后续")
		}
		return nil
	})
	return map[string]string{"work_id": id, "status": "stopped"}, err
}

// 在公开消息和提问/私聊工具的副作用之前预留名额。崩溃或发布失败时保守保留，
// 不会因为重试、切换到私聊或创建新子工作而获得一套新的发言上限。
func (s *Store) claimAutonomousMessage(ctx context.Context, limit int) error {
	root := autonomyRoot(ctx)
	if root == "" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO autonomous_publications(root_id) VALUES(?)`, root); err != nil {
		return err
	}
	r, err := tx.Exec(`UPDATE autonomous_publications SET reserved=reserved+1 WHERE root_id=? AND reserved<?`, root, max(1, limit))
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errAutonomyLimit
	}
	return tx.Commit()
}

func (s *Store) autonomousMessagesLeft(ctx context.Context, limit int) (bool, error) {
	root := autonomyRoot(ctx)
	if root == "" {
		return true, nil
	}
	var used int
	err := s.db.QueryRow(`SELECT reserved FROM autonomous_publications WHERE root_id=?`, root).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return used < max(1, limit), err
}
