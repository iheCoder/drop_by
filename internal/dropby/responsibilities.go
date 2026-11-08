package dropby

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Responsibility 是群里实际发生的约定，不是人格类型。空 ScopeID 表示长期群职责，
// 非空则指向一件真实工作；Body 用自然语言保留范围，不把组织方式变成固定流程。
type Responsibility struct {
	ID       string `json:"id"`
	MemberID string `json:"member_id"`
	ScopeID  string `json:"scope_id"`
	Body     string `json:"body"`
	SourceID string `json:"source_id"`
}

type ResponsibilityChange struct {
	ID       string `json:"id"`
	MemberID string `json:"member_id"`
	ScopeID  string `json:"scope_id"`
	Body     string `json:"body"`
	Revoke   bool   `json:"revoke"`
}

func (s *Store) initResponsibilities() error {
	for _, name := range []string{"parent_id", "first_speakers"} {
		var exists int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('work') WHERE name=?`, name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := s.db.Exec(`ALTER TABLE work ADD COLUMN ` + name + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	// 旧版本没有保存中间委托父链，但保存了同群触发根。迁移只恢复这个确知
	// 的祖先关系，使取消旧事项也能覆盖已存在的后续，不捏造更细的交接顺序。
	if _, err := s.db.Exec(`UPDATE work SET parent_id=root_id WHERE parent_id='' AND root_id<>'' AND root_id<>id AND EXISTS(SELECT 1 FROM work parent WHERE parent.id=work.root_id AND parent.conversation_id=work.conversation_id)`); err != nil {
		return err
	}
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS responsibilities (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id), member_id TEXT NOT NULL REFERENCES members(id),
 scope_id TEXT NOT NULL DEFAULT '', body TEXT NOT NULL, source_id TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX IF NOT EXISTS responsibility_room ON responsibilities(conversation_id,scope_id,revoked);`)
	return err
}

func (s *Store) Responsibilities(room string) ([]Responsibility, error) {
	rows, err := s.db.Query(`SELECT id,member_id,scope_id,body,source_id FROM responsibilities WHERE conversation_id=? AND revoked=0 ORDER BY rowid`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Responsibility{}
	for rows.Next() {
		var r Responsibility
		if err = rows.Scan(&r.ID, &r.MemberID, &r.ScopeID, &r.Body, &r.SourceID); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// 接受明确真人约定的解释结果。ID、群成员、源消息与事项范围仍由代码核实，
// 其他 BOT 的发言不能调用这个入口给自己任命。撤销职责不取消已经做出的成果。
func applyResponsibilities(tx *sql.Tx, c Conversation, source Work, changes []ResponsibilityChange) error {
	if len(changes) == 0 {
		return nil
	}
	if c.Kind != "group" {
		return fmt.Errorf("群内职责只能在群聊中约定")
	}
	var human int
	if err := tx.QueryRow(`SELECT count(*) FROM messages WHERE id=? AND conversation_id=? AND author_id='user'`, source.SourceID, c.ID).Scan(&human); err != nil {
		return err
	}
	if human != 1 {
		return fmt.Errorf("职责约定缺少当前真人消息来源")
	}
	var latest string
	if err := tx.QueryRow(`SELECT id FROM messages WHERE conversation_id=? AND author_id='user' ORDER BY seq DESC LIMIT 1`, c.ID).Scan(&latest); err != nil {
		return err
	}
	if latest != source.SourceID {
		return ErrStale
	}

	for _, change := range changes {
		if change.ScopeID == "this" {
			change.ScopeID = source.ID
		}
		if change.ID != "" {
			var existing Responsibility
			if change.ID == "legacy_"+c.ID && c.CoordinatorID != "" {
				existing = Responsibility{MemberID: c.CoordinatorID, Body: "需要一起做事时帮助分工与跟进。"}
				if _, err := tx.Exec(`UPDATE conversations SET coordinator_id='' WHERE id=?`, c.ID); err != nil {
					return err
				}
				if change.Revoke {
					continue
				}
				change.ID = ""
			} else if err := tx.QueryRow(`SELECT id,member_id,scope_id,body,source_id FROM responsibilities WHERE id=? AND conversation_id=? AND revoked=0`, change.ID, c.ID).Scan(&existing.ID, &existing.MemberID, &existing.ScopeID, &existing.Body, &existing.SourceID); err != nil {
				return fmt.Errorf("要调整的职责不在当前群中")
			}
			change.ScopeID = existing.ScopeID
			if change.MemberID == "" {
				change.MemberID = existing.MemberID
			}
			if change.Body == "" {
				change.Body = existing.Body
			}
		}
		if change.Revoke {
			if change.ID == "" {
				return fmt.Errorf("撤销职责需要实际职责 ID")
			}
			if _, err := tx.Exec(`UPDATE responsibilities SET revoked=1 WHERE id=?`, change.ID); err != nil {
				return err
			}
			// 旧版设置同样是用户的真实约定；撤销后不能被兼容字段再次授予。
			if change.ScopeID == "" {
				if _, err := tx.Exec(`UPDATE conversations SET coordinator_id='' WHERE id=? AND coordinator_id=?`, c.ID, change.MemberID); err != nil {
					return err
				}
			}
			continue
		}
		if !contains(c.MemberIDs, change.MemberID) || strings.TrimSpace(change.Body) == "" || len([]rune(change.Body)) > 1600 {
			return fmt.Errorf("职责需要当前群成员与具体范围")
		}
		if change.ScopeID != "" {
			var present int
			if err := tx.QueryRow(`SELECT count(*) FROM work WHERE id=? AND conversation_id=?`, change.ScopeID, c.ID).Scan(&present); err != nil {
				return err
			}
			if present != 1 {
				return fmt.Errorf("临时职责必须关联当前群中的实际事项")
			}
			// 一件事更换负责人，保留原约定作为历史；不覆盖另一件事或长期职责。
			if _, err := tx.Exec(`UPDATE responsibilities SET revoked=1 WHERE conversation_id=? AND scope_id=?`, c.ID, change.ScopeID); err != nil {
				return err
			}
		}
		id := change.ID
		if id == "" {
			// 同一真人消息在预算或上下文恢复后可能重新解释。复用它已经形成的
			// 同范围约定，不因一次失败重试把同一职责记录好几份。
			err := tx.QueryRow(`SELECT id FROM responsibilities WHERE conversation_id=? AND member_id=? AND scope_id=? AND source_id=? ORDER BY rowid LIMIT 1`, c.ID, change.MemberID, change.ScopeID, source.SourceID).Scan(&id)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if id == "" {
			id = newID("responsibility")
		}
		if _, err := tx.Exec(`INSERT INTO responsibilities(id,conversation_id,member_id,scope_id,body,source_id) VALUES(?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET member_id=excluded.member_id,body=excluded.body,source_id=excluded.source_id,revoked=0`, id, c.ID, change.MemberID, change.ScopeID, strings.TrimSpace(change.Body), source.SourceID); err != nil {
			return err
		}
	}
	return refreshCoordinationReviews(tx, c.ID)
}

// 最近一层事项职责优先于长期约定。父链描述真实委托/后续关系，和 RootID 的
// 预算累计边界分开，新的真人消息可以关联旧事项而不篡改旧工作的计费来源。
func responsibleMembers(tx *sql.Tx, room string, w Work) ([]string, error) {
	id := w.ID
	visited := map[string]bool{}
	for id != "" && !visited[id] {
		visited[id] = true
		var status string
		err := tx.QueryRow(`SELECT status FROM work WHERE id=? AND conversation_id=?`, id, room).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if status == "stopped" {
			return nil, nil
		}

		rows, err := tx.Query(`SELECT r.member_id FROM responsibilities r JOIN participants p ON p.conversation_id=r.conversation_id AND p.member_id=r.member_id WHERE r.conversation_id=? AND r.scope_id=? AND r.revoked=0`, room, id)
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for rows.Next() {
			var member string
			if err = rows.Scan(&member); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, member)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(ids) > 0 {
			return ids, nil
		}
		var parent string
		if err = tx.QueryRow(`SELECT parent_id FROM work WHERE id=? AND conversation_id=?`, id, room).Scan(&parent); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			return nil, err
		}
		id = parent
	}
	rows, err := tx.Query(`SELECT r.member_id FROM responsibilities r JOIN participants p ON p.conversation_id=r.conversation_id AND p.member_id=r.member_id WHERE r.conversation_id=? AND r.scope_id='' AND r.revoked=0`, room)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// 保留旧版真人设置的连续性。它会作为明确标记的既有约定显示，可在聊天中改掉。
	var legacy string
	if err = tx.QueryRow(`SELECT coordinator_id FROM conversations WHERE id=?`, room).Scan(&legacy); err != nil {
		return nil, err
	}
	if legacy != "" && !contains(ids, legacy) {
		ids = append(ids, legacy)
	}
	return ids, nil
}

func (s *Store) CanDelegate(c Conversation, m Member, w Work) bool {
	if c.Kind != "group" || c.ResearchPaused || !contains(c.MemberIDs, m.ID) {
		return false
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()
	ids, err := responsibleMembers(tx, c.ID, w)
	return err == nil && contains(ids, m.ID)
}

// 取消事项收束其后续、分享与回报，晚到结果不能把已取消的话题重新拉起来。
func stopWorkFamily(tx *sql.Tx, room, id string) error {
	_, err := tx.Exec(`WITH RECURSIVE family(id) AS (SELECT id FROM work WHERE id=? AND coalesce(conversation_id,'')=? UNION SELECT w.id FROM work w JOIN family f ON w.parent_id=f.id WHERE coalesce(w.conversation_id,'')=?)
 UPDATE work SET status='stopped',result=CASE WHEN result='' THEN '用户已取消这件事，保留现有成果。' ELSE result END WHERE id IN (SELECT id FROM family) AND (status IN ('pending','running','waiting_budget','paused') OR id=?)`, id, room, room, id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`WITH RECURSIVE family(id) AS (SELECT id FROM work WHERE id=? AND coalesce(conversation_id,'')=? UNION SELECT w.id FROM work w JOIN family f ON w.parent_id=f.id) UPDATE responsibilities SET revoked=1 WHERE coalesce(conversation_id,'')=? AND scope_id IN (SELECT id FROM family)`, id, room, room)
	return err
}

func (s *Store) checkMembership(c Conversation, member string) error {
	if c.ID == "" && c.Kind == "personal" {
		_, err := s.Member(member)
		return err
	}
	var present int
	err := s.db.QueryRow(`SELECT count(*) FROM conversations c JOIN participants p ON p.conversation_id=c.id WHERE c.id=? AND c.revision=? AND p.member_id=?`, c.ID, c.Revision, member).Scan(&present)
	if err != nil {
		return err
	}
	if present != 1 {
		return ErrStale
	}
	return nil
}

// 交接或撤销只重定向尚未处理的结果回报。逐件按它自己的父链解析，
// 所以换掉长期负责人不会抢走另一个成员正在负责的临时事项。
func refreshCoordinationReviews(tx *sql.Tx, room string) error {
	rows, err := tx.Query(`SELECT `+workColumns+` FROM work WHERE conversation_id=? AND substr(id,1,13)='coordination_' AND status IN ('pending','running','waiting_budget','paused')`, room)
	if err != nil {
		return err
	}
	list := []Work{}
	for rows.Next() {
		w, err := scanWork(rows)
		if err != nil {
			rows.Close()
			return err
		}
		list = append(list, w)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, w := range list {
		ids, err := responsibleMembers(tx, room, w)
		if err != nil {
			return err
		}
		if contains(ids, w.MemberID) {
			continue
		}
		if len(ids) == 0 {
			_, err = tx.Exec(`UPDATE work SET status='stopped' WHERE id=?`, w.ID)
		} else {
			_, err = tx.Exec(`UPDATE work SET member_id=?,status=CASE WHEN status='running' THEN 'pending' ELSE status END WHERE id=?`, ids[0], w.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// 临时组织者可以在结果都已收齐后结束自己的职责。是否该结束由角色根据语境
// 判断；代码只验证真实范围和未完子工作，不能用一次工具调用静默取消别人的研究。
type pendingResearchError struct{ Work []Work }

func (e *pendingResearchError) Error() string {
	return "这件事还有工作未结束；请核对列出的实际进展，等待结果或明确交接。"
}

func (s *Store) CloseResponsibility(c Conversation, m Member, w Work, id string) error {
	return s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		var r Responsibility
		var revoked bool
		if err := tx.QueryRow(`SELECT id,member_id,scope_id,body,source_id,revoked FROM responsibilities WHERE id=? AND conversation_id=?`, id, c.ID).Scan(&r.ID, &r.MemberID, &r.ScopeID, &r.Body, &r.SourceID, &revoked); err != nil {
			return fmt.Errorf("当前聊天没有这项有效职责")
		}
		if r.MemberID != m.ID || r.ScopeID == "" {
			return fmt.Errorf("只能结束自己这件事的临时职责；长期约定由真人调整")
		}
		var related int
		if err := tx.QueryRow(`WITH RECURSIVE family(id) AS (SELECT id FROM work WHERE id=? AND conversation_id=? UNION SELECT child.id FROM work child JOIN family f ON child.parent_id=f.id WHERE child.conversation_id=?) SELECT count(*) FROM family WHERE id=?`, r.ScopeID, c.ID, c.ID, w.ID).Scan(&related); err != nil {
			return err
		}
		if related == 0 {
			return fmt.Errorf("这次行动不属于该临时事项")
		}
		// 已结束的本人职责可重复确认；仍先核对所属事项，不能借幂等绕过权限。
		if revoked {
			return nil
		}
		rows, err := tx.Query(`WITH RECURSIVE family(id) AS (SELECT id FROM work WHERE id=? AND conversation_id=? UNION SELECT child.id FROM work child JOIN family f ON child.parent_id=f.id WHERE child.conversation_id=?) SELECT `+workColumns+` FROM work WHERE id IN (SELECT id FROM family) AND id<>? AND kind='research' AND status IN ('pending','running','waiting_budget','paused','failed') ORDER BY created_at`, r.ScopeID, c.ID, c.ID, w.ID)
		if err != nil {
			return err
		}
		unfinished := []Work{}
		for rows.Next() {
			job, scanErr := scanWork(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			unfinished = append(unfinished, job)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(unfinished) > 0 {
			// 等待是正常前置条件，交还具体工作让成员据实判断，不创建轮询任务。
			// 失败研究也未完成，不能因它退出执行队列就宣称结果已经收齐。
			return &pendingResearchError{Work: unfinished}
		}
		if _, err := tx.Exec(`UPDATE responsibilities SET revoked=1 WHERE id=?`, id); err != nil {
			return err
		}
		return refreshCoordinationReviews(tx, c.ID)
	})
}
