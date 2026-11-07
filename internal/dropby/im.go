package dropby

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type UserProfile struct {
	Name       string `json:"name"`
	Avatar     string `json:"avatar"`
	AvatarIcon string `json:"avatar_icon"`
	Color      string `json:"color"`
}

func (s *Store) initIM() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS user_profile(id INTEGER PRIMARY KEY CHECK(id=1),data TEXT NOT NULL)`)
	if err != nil {
		return err
	}
	for _, column := range []struct{ table, name, definition string }{{"conversations", "avatar", "TEXT NOT NULL DEFAULT ''"}, {"work", "budget_required", "INTEGER NOT NULL DEFAULT 0"}} {
		var exists int
		if err = s.db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, column.table, column.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err = s.db.Exec(`ALTER TABLE ` + column.table + ` ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) UserProfile() (UserProfile, error) {
	p := UserProfile{Name: "你", AvatarIcon: "orbit", Color: "blue"}
	var raw string
	err := s.db.QueryRow(`SELECT data FROM user_profile WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err == nil {
		decode(raw, &p)
	}
	return p, err
}

func (a *API) userProfile(w http.ResponseWriter, r *http.Request) {
	var p UserProfile
	if !readBody(w, r, &p) {
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len([]rune(p.Name)) > 24 {
		respond(w, 400, map[string]string{"error": "名字需要 1 到 24 个字"})
		return
	}
	if err := validateAvatar(p.Avatar); err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !contains(avatarIcons, p.AvatarIcon) {
		p.AvatarIcon = "orbit"
	}
	if !contains([]string{"jade", "apricot", "blue", "rose", "violet", "moss"}, p.Color) {
		p.Color = "blue"
	}
	if _, err := a.engine.Store.db.Exec(`INSERT INTO user_profile(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, asJSON(p)); err != nil {
		a.fail(w, err)
		return
	}
	a.engine.Hub.Publish("")
	respond(w, 200, p)
}

type GroupEdit struct {
	Name      string   `json:"name"`
	Avatar    string   `json:"avatar"`
	MemberIDs []string `json:"member_ids"`
	Revision  int64    `json:"revision"`
}

// 群资料与成员变更是一次事务。移除成员保留历史身份与消息，只撤销其在本群
// 继续行动的机会；revision 拦住已经生成但尚未提交的旧回复与工具副作用。
func (s *Store) UpdateGroup(c Conversation, edit GroupEdit) (Conversation, error) {
	if c.Kind != "group" {
		return c, fmt.Errorf("私聊不能修改群成员")
	}
	edit.Name = strings.TrimSpace(edit.Name)
	if edit.Name == "" || len([]rune(edit.Name)) > 80 {
		return c, fmt.Errorf("群名需要 1 到 80 个字")
	}
	if err := validateAvatar(edit.Avatar); err != nil {
		return c, err
	}
	if len(edit.MemberIDs) == 0 || len(edit.MemberIDs) > 12 {
		return c, fmt.Errorf("群里需要 1 到 12 位 AI 朋友")
	}
	err := s.Guard(c.ID, edit.Revision, func(tx *sql.Tx) error {
		seen := map[string]bool{}
		for _, id := range edit.MemberIDs {
			var n int
			if seen[id] {
				return fmt.Errorf("群成员不能重复")
			}
			seen[id] = true
			if err := tx.QueryRow(`SELECT count(*) FROM members WHERE id=?`, id).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("朋友不存在")
			}
		}
		for _, id := range c.MemberIDs {
			if seen[id] {
				continue
			}
			if _, err := tx.Exec(`DELETE FROM participants WHERE conversation_id=? AND member_id=?`, c.ID, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE work SET status='stopped',result=CASE WHEN result='' THEN '这位成员已离开本群，保留已完成成果。' ELSE result END WHERE conversation_id=? AND member_id=? AND status IN ('pending','running','waiting_budget','paused')`, c.ID, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE responsibilities SET revoked=1 WHERE conversation_id=? AND member_id=?`, c.ID, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE conversations SET coordinator_id='' WHERE id=? AND coordinator_id=?`, c.ID, id); err != nil {
				return err
			}
		}
		for _, id := range edit.MemberIDs {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO participants VALUES(?,?)`, c.ID, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE conversations SET name=?,avatar=?,revision=revision+1,updated_at=? WHERE id=?`, edit.Name, edit.Avatar, now(), c.ID)
		return err
	})
	if err != nil {
		return c, err
	}
	return s.Conversation(c.ID)
}

func (a *API) updateGroup(w http.ResponseWriter, r *http.Request) {
	c, err := a.engine.Store.Conversation(r.PathValue("id"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "聊天不存在"})
		return
	}
	var edit GroupEdit
	if !readBody(w, r, &edit) {
		return
	}
	c, err = a.engine.Store.UpdateGroup(c, edit)
	if err != nil {
		code := 400
		if errors.Is(err, ErrStale) {
			code = 409
			err = fmt.Errorf("群聊已有新变化，请重新打开编辑页后保存；你的修改尚未写入")
		}
		respond(w, code, map[string]string{"error": err.Error()})
		return
	}
	a.engine.Hub.Publish(c.ID)
	respond(w, 200, c)
}

// 额度是当前账户状态。已恢复的等待不会继续制造群消息，也不能因一次大预留
// 把剩余可用额度误称为零；预留释放或跨周时重新检查可以恢复的实际工作。
func (s *Store) budgetBlocked(cfg Config, usage Usage) bool {
	if usage.CostMicros >= cfg.WeeklyBudgetMicros || usage.Calls >= cfg.WeeklyCalls {
		return true
	}
	var blocked int
	_ = s.db.QueryRow(`SELECT count(*) FROM work WHERE status='waiting_budget' AND autonomous=0 AND budget_required>?`, cfg.WeeklyBudgetMicros-usage.CostMicros).Scan(&blocked)
	return blocked > 0
}

func (s *Store) recheckBudgetWait(cfg Config) error {
	u, err := s.Usage()
	if err != nil {
		return err
	}
	if u.Calls >= cfg.WeeklyCalls {
		return nil
	}
	_, err = s.db.Exec(`UPDATE work SET status='pending',due_at=? WHERE status='waiting_budget' AND budget_required>0 AND budget_required<=? AND (autonomous=0 OR budget_required<=?)`, time.Now().UnixMilli(), max(int64(0), cfg.WeeklyBudgetMicros-u.CostMicros), max(int64(0), cfg.AutonomousBudgetMicros-u.AutonomousCostMicros))
	return err
}
