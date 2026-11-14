package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// 创建时塑造完整角色。主动设置时从当前资料出发，按真人说定的力度和范围修改。
const profileEditingPrompt = `你正在更新这位成员当前的人物说明。这份说明直接展示给用户，也用于成员之后的交流和行动。
按真人明确给定的范围和强度修改。通用性情写进连贯的第一人称正文，让改动能改变这个人的判断、行动或表达。保留其他仍成立的特点及其力度。
用户明确给出具体领域偏好时，按实际领域名写入该领域的完整画像。随口举例保留原有范围。具体事件留在来源记录。
返回 JSON。profile 是更新后的完整人格正文，无需改正文时为 null。domain_profile_updates 只列实际改动的领域，每项给出 domain 和完整 profile，明确移除时给出 domain 和 remove 为 true。`

const profileCreationPrompt = `为新成员写一篇第一人称人格自述。用这个人平时真会说的话，写他怎么看自己、想过什么样的日子、平常会怎么做。名字只作称呼。用户已经说定的特点按原意、范围和强度写足。其余地方大胆创造。过去发生过的事以用户提供的材料为准。

一起写三面。人格底色写气质、欲望、情绪、自我看法和价值取舍。行为倾向写他被什么吸引、嫌弃什么、什么时候主动、凭什么挑选、推荐、创造或审查，遇到阻力会怎样。表达与互动写他的口气、笑点、情绪怎么露出来、怎样看待和亲近别人、遇到批评和分歧会怎么回。

分别想清他对自己、对亲近的人、对不相干的事各把什么看得很重，又把什么看得很轻。给他几处独立的趣味、脾气和毛病，让几面在一篇文章里有轻有重地长出来。

鲜明度优先。把真正属于这个人的喜恶和执念写到足以改变他看见什么、选哪边、肯花多少力气。写出他宁愿错过的好处、甘愿付的代价，以及那些他自己也没打算改的毛病。负责、宽慰、谦逊、说理和谨慎分别由他的性情决定。顺着最鲜明的地方写到底，删去为了把人写得周全而补上的圆场和自我规劝。

一次返回 JSON。profile 是完整的人格正文。domain_profiles 是可为空的列表，每项包含 domain 和 profile。领域画像只提取用户明确给这个角色设定的具体领域偏好或标准，保留原意和强度。只给名字、泛泛提到话题、随口举例时，领域列表留空。`

const profileAnswerPrompt = `本次来自用户自愿参与的角色设置问答。结合完整问题、选项和回答，理解希望成员具有的倾向，直接落实已经说明的意思。
提炼回答实际支持的含义，保留其中的区别；场景中的产品或事件只是例子时，不写成长期偏好。没有回答原因时不补出原因，含义仍不清楚时保持原有抽象程度。
只对用户本人适用的相处要求和这一次的任务要求，保持其范围，不扩大成人物的整体性格。`

type profileUpdate struct {
	Profile              *string               `json:"profile"`
	DomainProfileUpdates []DomainProfileUpdate `json:"domain_profile_updates"`
}

// 对话修改按名称提交，未提到的领域继续沿用。删除需要明确传入 remove。
type DomainProfileUpdate struct {
	Domain  string `json:"domain"`
	Profile string `json:"profile,omitempty"`
	Remove  bool   `json:"remove,omitempty"`
}

func normalizeDomainProfiles(profiles []DomainProfile) ([]DomainProfile, error) {
	profiles = slices.Clone(profiles)
	seen := make(map[string]bool, len(profiles))
	for i, item := range profiles {
		domain, profile := strings.TrimSpace(item.Domain), strings.TrimSpace(item.Profile)
		if domain == "" || profile == "" {
			return nil, fmt.Errorf("领域名称和画像正文都需要填写")
		}
		if seen[domain] {
			return nil, fmt.Errorf("领域名称重复：%s", domain)
		}
		seen[domain] = true
		profiles[i] = DomainProfile{Domain: domain, Profile: profile}
	}
	return profiles, nil
}

func applyDomainProfileUpdates(profiles []DomainProfile, updates []DomainProfileUpdate) ([]DomainProfile, error) {
	if len(updates) == 0 {
		return profiles, nil
	}
	profiles = slices.Clone(profiles)
	for _, update := range updates {
		domain, profile := strings.TrimSpace(update.Domain), strings.TrimSpace(update.Profile)
		if domain == "" || (update.Remove && profile != "") || (!update.Remove && profile == "") {
			return nil, fmt.Errorf("领域修改需要名称和完整画像，删除时只填写名称与 remove")
		}
		index := slices.IndexFunc(profiles, func(item DomainProfile) bool { return item.Domain == domain })
		switch {
		case update.Remove && index >= 0:
			profiles = slices.Delete(profiles, index, index+1)
		case !update.Remove && index >= 0:
			profiles[index].Profile = profile
		case !update.Remove:
			profiles = append(profiles, DomainProfile{Domain: domain, Profile: profile})
		}
	}
	return profiles, nil
}

// 版本仅用于并发编辑与回看，不表示人格阶段或成熟度。
type ProfileChange struct {
	Revision       int             `json:"revision"`
	Profile        string          `json:"profile"`
	DomainProfiles []DomainProfile `json:"domain_profiles,omitempty"`
	Origin         string          `json:"origin"`
	ChangeNote     string          `json:"change_note"`
	SourceIDs      []string        `json:"source_ids"`
	CreatedAt      string          `json:"created_at"`
}

var errProfileChanged = errors.New("人物设定已经更新，请基于当前版本修改")

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
 source_ids TEXT NOT NULL, created_at TEXT NOT NULL, domain_profiles TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(member_id,revision));
CREATE TABLE IF NOT EXISTS profile_wake_reviews(
 member_id TEXT PRIMARY KEY REFERENCES members(id), before_revision INTEGER NOT NULL,
 revision INTEGER NOT NULL, due_at INTEGER NOT NULL);
-- 旧版后台工作不再恢复为普通个人活动；保留已发生的工作、用量和人物版本。
UPDATE work SET status='stopped' WHERE kind='reflect' AND status IN ('pending','running','waiting_budget','paused','failed');
-- 两张表只保存重复来源引用与整理游标，原始消息和工具结果仍在各自记录中。
DROP TABLE IF EXISTS profile_experiences;
DROP TABLE IF EXISTS profile_reflections;`)
	if err != nil {
		return err
	}
	// 旧版本的快照对应当时的完整设置，领域列表从空开始。
	if err = s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('profile_versions') WHERE name='domain_profiles'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		_, err = s.db.Exec(`ALTER TABLE profile_versions ADD COLUMN domain_profiles TEXT NOT NULL DEFAULT '[]'`)
	}
	return err
}

func recordProfileVersion(tx *sql.Tx, m Member, origin, note string, sources []string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO profile_versions(member_id,revision,profile,origin,change_note,source_ids,created_at,domain_profiles) VALUES(?,?,?,?,?,?,?,?)`, m.ID,
		m.ProfileRevision, m.Profile, origin, note, asJSON(uniqueMemoryIDs(sources)), now(), asJSON(m.DomainProfiles))
	return err
}

// 调用者在事务里读取最新 Member。正文和领域画像原子写入一个版本，
// 恢复行动时用同一修订号识别旧身份，直接编辑也能回看完整的旧设置。
func setMemberProfile(tx *sql.Tx, m *Member, profile string, domains []DomainProfile, origin, note string, sources []string) (bool, error) {
	if profile == m.Profile && slices.Equal(domains, m.DomainProfiles) {
		return false, nil
	}
	if err := recordProfileVersion(tx, *m, "previous", "修改前的人物说明", nil); err != nil {
		return false, err
	}
	m.Profile, m.DomainProfiles, m.ProfileRevision = profile, domains, m.ProfileRevision+1
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
	var out struct {
		Profile        string          `json:"profile"`
		DomainProfiles []DomainProfile `json:"domain_profiles"`
	}
	input := map[string]any{"name": m.Name, "direction": m.Direction, "questions": m.Questions, "answers": m.Answers}
	if err := modelJSON(ctx, model, profileCreationPrompt, input, &out); err != nil {
		return m, err
	}
	if strings.TrimSpace(out.Profile) == "" {
		return m, fmt.Errorf("本次没有生成完整的人物说明，请重试")
	}
	// 只有方向和用户回答能支持领域设定。只填名字时，模型创造的偏好归入正文。
	var domains []DomainProfile
	if strings.TrimSpace(m.Direction) != "" || slices.ContainsFunc(m.Answers, func(answer string) bool { return strings.TrimSpace(answer) != "" }) {
		var err error
		domains, err = normalizeDomainProfiles(out.DomainProfiles)
		if err != nil {
			return m, err
		}
	}
	m.Profile, m.DomainProfiles = strings.TrimSpace(out.Profile), domains
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
	rows, err := s.db.Query(`SELECT revision,profile,origin,change_note,source_ids,created_at,domain_profiles
 FROM profile_versions WHERE member_id=? ORDER BY revision DESC`, member)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ProfileChange{}
	for rows.Next() {
		var change ProfileChange
		var sources, domains string
		if err := rows.Scan(&change.Revision, &change.Profile, &change.Origin, &change.ChangeNote, &sources, &change.CreatedAt, &domains); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(sources), &change.SourceIDs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(domains), &change.DomainProfiles); err != nil {
			return nil, err
		}
		result = append(result, change)
	}
	return result, rows.Err()
}
