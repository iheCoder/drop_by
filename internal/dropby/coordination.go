package dropby

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) initCoordination() error {
	// 这两个字段分别记录群里的职责、具体工作的委托关系。旧群和旧工作
	// 仍然没有协调者，不通过迁移擅自改变任何成员的身份或工作安排。
	for _, column := range []struct{ table, name string }{
		{"conversations", "coordinator_id"}, {"work", "assigned_by"},
	} {
		var exists int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, column.table, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := s.db.Exec(`ALTER TABLE ` + column.table + ` ADD COLUMN ` + column.name + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetCoordinator 只由真人设置。任命是某个群的职责，不是所有会话共用的超级 BOT；
// 空串表示恢复平等交流。修订号使已经在执行的旧职责判断不能继续产生副作用。
func (s *Store) SetCoordinator(c Conversation, memberID string) (Conversation, error) {
	err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		var kind, existing string
		if err := tx.QueryRow(`SELECT kind,coordinator_id FROM conversations WHERE id=?`, c.ID).Scan(&kind, &existing); err != nil {
			return err
		}
		if kind != "group" {
			return fmt.Errorf("只有群聊可以设置协调者")
		}
		if memberID != "" {
			var present int
			if err := tx.QueryRow(`SELECT count(*) FROM participants WHERE conversation_id=? AND member_id=?`, c.ID, memberID).Scan(&present); err != nil {
				return err
			}
			if present == 0 {
				return fmt.Errorf("协调者必须是当前群里的成员")
			}
		}
		if existing == memberID {
			return nil
		}
		if _, err := tx.Exec(`UPDATE conversations SET coordinator_id=?,revision=revision+1,updated_at=? WHERE id=?`, memberID, now(), c.ID); err != nil {
			return err
		}
		// 已经到达但尚未处理的回报也归新职责人；取消角色则撤下这类协调
		// 机会。实际接手的研究不受影响，仍由原成员完成或依据用户要求停止。
		_, err := tx.Exec(`UPDATE work SET member_id=?,status=CASE WHEN ?='' THEN 'stopped' WHEN status='running' THEN 'pending' ELSE status END
WHERE conversation_id=? AND substr(id,1,13)='coordination_' AND status IN ('pending','running','waiting_budget','paused')`, memberID, memberID, c.ID)
		return err
	})
	if err != nil {
		return c, err
	}
	return s.Conversation(c.ID)
}

// DelegateWork 安排一项可检查结果的真实行动，不生成同伴的台词，也不直接转交
// 任何人的私有记忆。成员自己执行 research，仍可根据新情境说明不适合或不必继续。
func (s *Store) DelegateWork(c Conversation, coordinator Member, source Work, memberID, instruction, reason string, organize ...bool) (Work, error) {
	w := Work{ID: newID("work"), ConversationID: c.ID, MemberID: memberID,
		Kind: "research", AssignedBy: coordinator.ID, Instruction: strings.TrimSpace(instruction),
		Reason: strings.TrimSpace(reason), Status: "pending", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
	if w.Instruction == "" || w.Reason == "" {
		return w, fmt.Errorf("委托需要具体任务、预期结果，以及选择这位同伴的理由")
	}
	err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		var kind, current string
		var paused bool
		if err := tx.QueryRow(`SELECT kind,coordinator_id,research_paused FROM conversations WHERE id=?`, c.ID).Scan(&kind, &current, &paused); err != nil {
			return err
		}
		responsible, err := responsibleMembers(tx, c.ID, source)
		if err != nil {
			return err
		}
		if kind != "group" || !contains(responsible, coordinator.ID) {
			return fmt.Errorf("当前事项没有授予这位成员组织职责")
		}
		if paused {
			return fmt.Errorf("研究已暂停，不能通过委托继续安排工作")
		}
		if memberID == coordinator.ID {
			return fmt.Errorf("自己的行动请使用 schedule_exploration，不必委托给自己")
		}
		var present int
		if err := tx.QueryRow(`SELECT count(*) FROM participants WHERE conversation_id=? AND member_id=?`, c.ID, memberID).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			return fmt.Errorf("只能委托当前群里的其他成员")
		}

		// 从真实触发工作继承执行根和计费来源。把真人消息放进引用不等于
		// 可以把自主后续变为前台调用，更不能用转派不断取得一份新的链额度。
		parent, err := scanWork(tx.QueryRow(`SELECT `+workColumns+` FROM work WHERE id=? AND conversation_id=?`, source.ID, c.ID))
		if err != nil {
			return fmt.Errorf("委托必须来自当前聊天中实际存在的工作")
		}
		w.RootID, w.SourceID, w.Autonomous, w.ParentID = rootOf(parent), parent.SourceID, autonomousWork(parent), parent.ID
		// 新委托即使文字相同也有自己的目的；工具入口负责同次调用的恢复。
		if err := insertWork(tx, w); err != nil {
			return err
		}
		if len(organize) > 0 && organize[0] {
			// 这是原委托内的子事项组织权；不是把对方升为整个群的长期负责人。
			_, err := tx.Exec(`INSERT INTO responsibilities(id,conversation_id,member_id,scope_id,body,source_id) VALUES(?,?,?,?,?,?)`, newID("responsibility"), c.ID, memberID, w.ID, "受 "+coordinator.Name+" 委托组织这项工作："+w.Instruction, w.SourceID)
			return err
		}
		return nil
	})
	return w, err
}

// 结果驱动下一次协调判断，不用定时轮询催促同伴。通知与工作保存共用事务，
// 每项工作只有一个回报机会，服务重启和重复完成不会产生多份通知。
func queueCoordinationReview(tx *sql.Tx, w Work, status, result string) error {
	if w.AssignedBy == "" || w.Kind != "research" || (status != "done" && status != "failed" && status != "paused") {
		return nil
	}
	var coordinator string
	var paused bool
	if err := tx.QueryRow(`SELECT coordinator_id,research_paused FROM conversations WHERE id=? AND kind='group'`, w.ConversationID).Scan(&coordinator, &paused); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	scope := w
	if w.ParentID != "" {
		scope.ID = w.ParentID
	}
	responsible, err := responsibleMembers(tx, w.ConversationID, scope)
	if err != nil {
		return err
	}
	if len(responsible) > 0 {
		coordinator = responsible[0]
		if contains(responsible, w.AssignedBy) {
			coordinator = w.AssignedBy
		}
	} else {
		coordinator = ""
	}
	if coordinator == "" || paused {
		return nil
	}
	id := "coordination_" + w.ID
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM work WHERE id=?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	if status == "failed" || status == "paused" {
		result = "尚未完成，已有成果保留。"
	}
	review := Work{ID: id, ParentID: w.ParentID, RootID: rootOf(w), Autonomous: true, ConversationID: w.ConversationID,
		MemberID: coordinator, Kind: "revisit", SourceID: w.SourceID, Status: "pending",
		Instruction: "一项委托有了实际结果。先用 read_work 核对工作 " + w.ID + "。状态：" + status + "；实际记录：" + clip(result, 2500),
		Reason:      "同伴的委托结果触发了这次复看，不是定时催促。", DueAt: time.Now().UnixMilli(), CreatedAt: now()}
	err = insertWork(tx, review)
	return err
}

// 工作属于当前会话的共享执行记录，读取不能越过群聊/私聊边界。
func (s *Store) ReadWork(room, id string) (Work, error) {
	w, err := scanWork(s.db.QueryRow(`SELECT `+workColumns+` FROM work WHERE coalesce(conversation_id,'')=? AND id=?`, room, id))
	if errors.Is(err, sql.ErrNoRows) {
		return w, fmt.Errorf("当前聊天没有这项工作")
	}
	return w, err
}

// 活跃工作不能因为创建得早就被最近 50 条已完成记录挤掉。调用者按需给出
// 简短索引，详情仍用 ReadWork 读取，而不是把每项任务全文塞进所有上下文。
func (s *Store) OpenWork(room string) ([]Work, error) {
	rows, err := s.db.Query(`SELECT `+workColumns+` FROM work WHERE coalesce(conversation_id,'')=? AND status IN ('pending','running','waiting_budget','paused') AND kind IN ('research','revisit') ORDER BY due_at,created_at`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Work{}
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, w)
	}
	return list, rows.Err()
}
