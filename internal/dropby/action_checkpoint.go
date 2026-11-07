package dropby

import (
	"encoding/json"
)

// 只保存正在执行的成员行动，不新增角色生命周期。它解决实际故障：工具已保存、
// 下一次模型请求失败时，恢复不能重新生成并执行那批工具。
// 公开消息提交与删除现场在同一事务内完成，崩溃后不会再次发布同一行动。
type actionCheckpoint struct {
	ID       string
	WorkID   string
	MemberID string
	Revision int64
	Seen     int64
	Messages []ChatMessage
}

func (s *Store) loadAction(work, member string) (actionCheckpoint, error) {
	var cp actionCheckpoint
	var raw string
	err := s.db.QueryRow("SELECT id,work_id,member_id,revision,seen,messages FROM action_checkpoints WHERE work_id=? AND member_id=?", work, member).Scan(&cp.ID, &cp.WorkID, &cp.MemberID, &cp.Revision, &cp.Seen, &raw)
	if err != nil {
		return cp, err
	}
	err = json.Unmarshal([]byte(raw), &cp.Messages)
	return cp, err
}
func (s *Store) saveAction(cp actionCheckpoint) error {
	if cp.WorkID == "" {
		return nil
	} // 无持久工作的一次性调用没有可恢复的调度身份。
	_, err := s.db.Exec("INSERT INTO action_checkpoints(id,work_id,member_id,revision,seen,messages) VALUES(?,?,?,?,?,?) ON CONFLICT(work_id,member_id) DO UPDATE SET revision=excluded.revision,seen=excluded.seen,messages=excluded.messages", cp.ID, cp.WorkID, cp.MemberID, cp.Revision, cp.Seen, asJSON(cp.Messages))
	return err
}
func (s *Store) finishAction(work, member string) error {
	_, err := s.db.Exec("DELETE FROM action_checkpoints WHERE work_id=? AND member_id=?", work, member)
	return err
}

// 多位成员可以分别保留未完现场；恢复时逐个接上，已发布者没有现场可重放。
func (s *Store) pendingActions(work string) ([]string, error) {
	rows, err := s.db.Query("SELECT member_id FROM action_checkpoints WHERE work_id=? ORDER BY rowid", work)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []string
	for rows.Next() {
		var member string
		if err := rows.Scan(&member); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}
