package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// 人物说明只有 Member.Profile 一份。下面的提示用于创建、用户设置及独立后台
// 整理，不叠加到日常任务中，也不替所有成员规定严谨程度、观点或表达风格。
const profileEditingPrompt = `你正在更新这位成员当前的人物说明。这份说明直接展示给用户，也用于成员之后的交流和行动。
用自然、连贯的第一人称表达“现在的我是谁”。兴趣、关注点、判断方式、互动、语言和主动性可以自然体现，不必按固定栏目填写。保留这个人的具体味道。
根据本次材料把说明写得更贴切。有了新的理解就修改相关部分，其他仍然成立的特点继续保留；可以增加、替换、合并或删除。
具体事件帮助理解变化，人物说明表达可以用于以后生活与互动的倾向。举例的含义已经清楚时写出含义；仍不清楚时保留宽泛表达，不替例子补出确定的长期偏好。
事件经过留在来源记录，说明表达当前的自己。返回 JSON：profile 为更新后的完整说明，没有变化则为 null；change_note 简述实际变化；source_ids 列出本次使用的实际来源 ID。`

const profileCreationPrompt = `本次是创建新成员。用户可能只提供名字，也可能给出方向。
生成一个具体、自洽、保留成长空间的起点，让用户从自我介绍中认识这个人。名字用于称呼；用户没有指定的特点可以自由生成，不把职业当作全部性格。
性格与兴趣是这次生成的角色设定，实际生活经历另有来源；不要为完整小传编造童年、旅行或已经做过的事。
直接给出人物说明，不要求用户先回答问卷。`

const profileAnswerPrompt = `本次来自用户自愿参与的角色设置问答。结合完整问题、选项和回答，理解希望成员具有的倾向，直接落实已经说明的意思。
提炼回答实际支持的含义，保留其中的区别；场景中的产品或事件只是例子时，不写成长期偏好。没有回答原因时不补出原因，含义仍不清楚时保持原有抽象程度。
只对用户本人适用的相处要求和这一次的任务要求，保持其范围，不扩大成人物的整体性格。`

const profileSelfPrompt = `这是你独立于日常任务的一次自我整理。根据真实接触过的经历，更新对“我是谁”的理解。
看看当前说明和新经历：它们有没有让你更清楚什么会吸引你、什么值得投入、你怎样判断与行动，以及喜欢怎样与人相处和表达自己？
把已经形成的新理解直接整合进说明。可以发展兴趣、改变看法、调整互动方式，也可以修正原先对自己的描述。
一次有分量的经历也可以带来变化，不需要累计次数或证明永久保持。用与当前理解相称的语气和范围表达。
别人的反馈是认识自己的材料，根据反馈的具体内容形成自己的判断，不把获得赞同本身当作修改目的。对别人的了解保留其归属。
事件经过、一次打算留在相应记录中；用户明确要求持续保留的设定按其要求保留。回读或转述旧记录仍是同一段经历。
没有新的自我认识时沿用当前说明，无需为了完成整理换一种说法。可以按需回查自己的经历和原始记录。`

type profileUpdate struct {
	Profile    *string  `json:"profile"`
	ChangeNote string   `json:"change_note"`
	SourceIDs  []string `json:"source_ids"`
}

// 版本仅用于并发编辑与回看，不表示人格阶段或成熟度。
type ProfileChange struct {
	Revision   int      `json:"revision"`
	Profile    string   `json:"profile"`
	Origin     string   `json:"origin"`
	ChangeNote string   `json:"change_note"`
	SourceIDs  []string `json:"source_ids"`
	CreatedAt  string   `json:"created_at"`
}

var errProfileChanged = errors.New("人物说明已经更新，请基于当前版本修改")

func (s *Store) initPersonality() error {
	// 恢复旧行动时识别人物变更，重新判断尚未执行的调用；不取消正在执行的任务。
	var exists int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('action_checkpoints') WHERE name='profile_revision'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := s.db.Exec(`ALTER TABLE action_checkpoints ADD COLUMN profile_revision INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS profile_versions(
 member_id TEXT NOT NULL REFERENCES members(id), revision INTEGER NOT NULL,
 profile TEXT NOT NULL, origin TEXT NOT NULL, change_note TEXT NOT NULL,
 source_ids TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(member_id,revision));
CREATE TABLE IF NOT EXISTS profile_experiences(
 seq INTEGER PRIMARY KEY AUTOINCREMENT, member_id TEXT NOT NULL REFERENCES members(id),
 source_id TEXT NOT NULL, UNIQUE(member_id,source_id));
CREATE TABLE IF NOT EXISTS profile_reflections(
 member_id TEXT PRIMARY KEY REFERENCES members(id), after_seq INTEGER NOT NULL DEFAULT 0,
 next_at INTEGER NOT NULL DEFAULT 0);
INSERT OR IGNORE INTO profile_reflections(member_id) SELECT id FROM members;`)
	return err
}

func recordProfileVersion(tx *sql.Tx, m Member, origin, note string, sources []string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO profile_versions VALUES(?,?,?,?,?,?,?)`, m.ID,
		m.ProfileRevision, m.Profile, origin, note, asJSON(uniqueMemoryIDs(sources)), now())
	return err
}

// 所有入口复用同一份版本写入。调用者在事务里读取最新 Member，因而只更新人物
// 说明，不把模型输入里的旧名字、头像或工具配置覆盖回来。旧资料首次修改时留档。
func setMemberProfile(tx *sql.Tx, m *Member, profile, origin, note string, sources []string) (bool, error) {
	profile = strings.TrimSpace(profile)
	if profile == m.Profile {
		return false, nil
	}
	if err := recordProfileVersion(tx, *m, "previous", "修改前的人物说明", nil); err != nil {
		return false, err
	}
	m.Profile, m.ProfileRevision = profile, m.ProfileRevision+1
	if _, err := tx.Exec(`UPDATE members SET data=? WHERE id=?`, asJSON(m), m.ID); err != nil {
		return false, err
	}
	return true, recordProfileVersion(tx, *m, origin, note, sources)
}

func generateMemberProfile(ctx context.Context, model Model, m Member) (Member, error) {
	if model == nil {
		return m, ErrUnavailable
	}
	ctx = withUsageCall(ctx, usageCall{purpose: "preferences", member: m.ID})
	var out profileUpdate
	input := map[string]any{"name": m.Name, "direction": m.Direction, "questions": m.Questions, "answers": m.Answers}
	if err := modelJSON(ctx, model, profileEditingPrompt+"\n"+profileCreationPrompt, input, &out); err != nil {
		return m, err
	}
	if out.Profile == nil || strings.TrimSpace(*out.Profile) == "" {
		return m, fmt.Errorf("本次没有生成完整的人物说明，请重试")
	}
	m.Profile = strings.TrimSpace(*out.Profile)
	return m, nil
}

func (a *API) profileHistory(w http.ResponseWriter, r *http.Request) {
	changes, err := a.engine.Store.profileHistory(r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	respond(w, http.StatusOK, changes)
}

func (s *Store) profileHistory(member string) ([]ProfileChange, error) {
	rows, err := s.db.Query(`SELECT revision,profile,origin,change_note,source_ids,created_at
 FROM profile_versions WHERE member_id=? ORDER BY revision DESC`, member)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ProfileChange{}
	for rows.Next() {
		var change ProfileChange
		var sources string
		if err := rows.Scan(&change.Revision, &change.Profile, &change.Origin, &change.ChangeNote, &sources, &change.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(sources), &change.SourceIDs); err != nil {
			return nil, err
		}
		result = append(result, change)
	}
	return result, rows.Err()
}
