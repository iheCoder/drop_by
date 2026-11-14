package dropby

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// 一次真人设定可以改正文，也可以按名称改领域画像。来源 ID 对应真人提出设置的原话。
type PreferenceUpdate struct {
	MemberID             string                `json:"member_id"`
	Profile              string                `json:"profile,omitempty"`
	DomainProfileUpdates []DomainProfileUpdate `json:"domain_profile_updates,omitempty"`
	SourceID             string                `json:"source_id"`
	ExpectedRevision     *int                  `json:"expected_revision,omitempty"`
	ChangeNote           string                `json:"change_note,omitempty"`
}

// 控制层用同一个模型请求理解普通指令和明确偏好，避免每句话再串行调用一个模型。
// 这里包含刚关闭的问题，是为了自由回答在选项已经收起后仍能正确归属。
func (s *Store) PreferenceContext(c Conversation, sourceID string) (map[string]any, error) {
	var user Message
	err := s.db.QueryRow(`SELECT id,author_id,body,reply_to FROM messages WHERE id=? AND conversation_id=? AND author_id='user'`, sourceID, c.ID).Scan(&user.ID, &user.AuthorID, &user.Body, &user.ReplyTo)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	people := []map[string]string{}
	for _, id := range c.MemberIDs {
		m, err := s.Member(id)
		if err != nil {
			return nil, err
		}
		people = append(people, map[string]string{"id": m.ID, "name": m.Name, "profile": m.Profile})
	}
	result := map[string]any{"latest_human_message": user, "members": people}
	var id, owner, question string
	err = s.db.QueryRow(`SELECT m.id,m.author_id,q.question FROM message_questions q JOIN messages m ON m.id=q.message_id WHERE m.conversation_id=? AND (q.response_id=? OR m.id=?) ORDER BY m.seq DESC LIMIT 1`, c.ID, sourceID, user.ReplyTo).Scan(&id, &owner, &question)
	if err == nil {
		var q Question
		if err = json.Unmarshal([]byte(question), &q); err != nil {
			return nil, err
		}
		result["preceding_question"] = map[string]any{"message_id": id, "member_id": owner, "question": q}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return result, nil
}

func previewPreference(m Member, update PreferenceUpdate) (Member, bool, error) {
	profile := strings.TrimSpace(update.Profile)
	if profile == "" {
		profile = m.Profile
	}
	domains, err := applyDomainProfileUpdates(m.DomainProfiles, update.DomainProfileUpdates)
	if err != nil {
		return m, false, err
	}
	changed := profile != m.Profile || !slices.Equal(domains, m.DomainProfiles)
	m.Profile, m.DomainProfiles = profile, domains
	return m, changed, nil
}

// 只修改人物说明，名字、头像、创建时间和已有经历保持原身份。事务内重新读取人物，
// 避免把模型输入中的旧头像、旧名字整个覆盖回数据库。
func applyPreferenceUpdates(tx *sql.Tx, c Conversation, sourceID string, updates []PreferenceUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	var author, replyTo string
	if err := tx.QueryRow(`SELECT author_id,reply_to FROM messages WHERE id=? AND conversation_id=?`, sourceID, c.ID).Scan(&author, &replyTo); err != nil {
		return err
	}
	if author != "user" {
		return fmt.Errorf("人物偏好只能来自真人明确说明")
	}
	var latestHumanID string
	if err := tx.QueryRow(`SELECT id FROM messages WHERE conversation_id=? AND author_id='user' ORDER BY seq DESC LIMIT 1`, c.ID).Scan(&latestHumanID); err != nil {
		return err
	}
	if latestHumanID != sourceID {
		return ErrStale
	}
	var questionOwner string
	err := tx.QueryRow(`SELECT m.author_id FROM message_questions q JOIN messages m ON m.id=q.message_id WHERE m.id=? AND m.conversation_id=?`, replyTo, c.ID).Scan(&questionOwner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, update := range updates {
		profile := strings.TrimSpace(update.Profile)
		if update.SourceID != sourceID || !contains(c.MemberIDs, update.MemberID) || (profile == "" && len(update.DomainProfileUpdates) == 0) {
			return fmt.Errorf("人物偏好修改的来源或成员无效")
		}
		if questionOwner != "" && questionOwner != update.MemberID {
			return fmt.Errorf("问题回答只能修改提问成员的偏好")
		}
		var raw string
		if err := tx.QueryRow(`SELECT data FROM members WHERE id=?`, update.MemberID).Scan(&raw); err != nil {
			return err
		}
		var m Member
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		if update.ExpectedRevision != nil && m.ProfileRevision != *update.ExpectedRevision {
			return errProfileChanged
		}
		if profile == "" {
			profile = m.Profile
		}
		domains, err := applyDomainProfileUpdates(m.DomainProfiles, update.DomainProfileUpdates)
		if err != nil {
			return err
		}
		note := update.ChangeNote
		if note == "" {
			note = "用户在聊天中调整了人物设定"
		}
		profileChanged, err := setMemberProfile(tx, &m, profile, domains, "instruction", note, []string{sourceID})
		if err != nil {
			return err
		}
		// 人物说明属于同一个成员，其他房间中尚未发出的旧性格草稿也需要重新考虑。
		// 当前房间的控制阶段尚未开始生成，保留其修订，后续会直接读取新说明。
		if profileChanged {
			if _, err := tx.Exec(`UPDATE conversations SET revision=revision+1 WHERE id<>? AND id IN (SELECT conversation_id FROM participants WHERE member_id=?)`, c.ID, m.ID); err != nil {
				return err
			}
			if err := markIdentityRetry(tx, m.ID, c.ID); err != nil {
				return err
			}
		}
		// 自由回答只有被确认为相处偏好后才记作已回答；改聊其他内容保留 dismissed。
		if _, err := tx.Exec(`UPDATE message_questions SET status='answered' WHERE response_id=? AND message_id IN (SELECT id FROM messages WHERE author_id=? AND conversation_id=?)`, sourceID, m.ID, c.ID); err != nil {
			return err
		}
	}
	return nil
}

// 只有实际的人物说明更新才说明“原真人消息里的旧偏好已被更晚编辑覆盖”。
// 把这件事实与资料写入放在同一事务，避免把协调者变化等普通修订误判为
// 人物编辑，也避免请求恰好等待预算/退出时丢掉先后关系。
func markIdentityRetry(tx *sql.Tx, memberID, exceptRoom string) error {
	_, err := tx.Exec(`UPDATE work SET result=? || CASE WHEN result='' THEN '' ELSE char(10)||result END
WHERE kind='chat' AND status IN ('pending','running','waiting_budget','paused') AND conversation_id<>?
AND conversation_id IN (SELECT conversation_id FROM participants WHERE member_id=?)
AND substr(result,1,length(?))<>?`, identityRevisionRetry, exceptRoom, memberID, identityRevisionRetry, identityRevisionRetry)
	return err
}

var avatarIcons = []string{"portrait-sky", "portrait-cat", "portrait-traveler", "orbit", "cat", "fox", "bear", "rabbit", "owl", "sprout", "flower", "sun", "moon", "cloud", "ghost"}
var avatarColors = []string{"jade", "apricot", "blue", "rose", "violet", "moss"}

// 显式编辑和对话中更新使用同一份人物说明。可选字段保留未修改的数据；清空 avatar
// 表示恢复默认图标，图片仍经过与创建相同的小型栅格验证。
func (a *API) updateMember(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name            *string          `json:"name"`
		Profile         *string          `json:"profile"`
		DomainProfiles  *[]DomainProfile `json:"domain_profiles"`
		Color           *string          `json:"color"`
		Avatar          *string          `json:"avatar"`
		AvatarIcon      *string          `json:"avatar_icon"`
		ProfileRevision *int             `json:"profile_revision"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if in.Name != nil {
		*in.Name = strings.TrimSpace(*in.Name)
		if *in.Name == "" || len([]rune(*in.Name)) > 24 {
			respond(w, 400, map[string]string{"error": "名字需要 1 到 24 个字"})
			return
		}
	}
	if in.DomainProfiles != nil {
		domains, err := normalizeDomainProfiles(*in.DomainProfiles)
		if err != nil {
			respond(w, 400, map[string]string{"error": err.Error()})
			return
		}
		in.DomainProfiles = &domains
	}
	if in.Avatar != nil {
		if err := validateAvatar(*in.Avatar); err != nil {
			respond(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	if in.AvatarIcon != nil && !contains(avatarIcons, *in.AvatarIcon) {
		respond(w, 400, map[string]string{"error": "请选择已有头像图案"})
		return
	}
	if in.Color != nil && !contains(avatarColors, *in.Color) {
		respond(w, 400, map[string]string{"error": "请选择已有头像颜色"})
		return
	}
	tx, err := a.engine.Store.db.Begin()
	if err != nil {
		a.fail(w, err)
		return
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRow(`SELECT data FROM members WHERE id=?`, r.PathValue("id")).Scan(&raw); err != nil {
		respond(w, 404, map[string]string{"error": "成员不存在"})
		return
	}
	var m Member
	if err = json.Unmarshal([]byte(raw), &m); err != nil {
		a.fail(w, err)
		return
	}
	editProfile := in.Profile != nil || in.DomainProfiles != nil
	if editProfile && in.ProfileRevision != nil && m.ProfileRevision != *in.ProfileRevision {
		respond(w, http.StatusConflict, map[string]string{"error": errProfileChanged.Error()})
		return
	}
	nameChanged := in.Name != nil && *in.Name != m.Name
	if in.Name != nil {
		m.Name = *in.Name
	}
	profileChanged := false
	if editProfile {
		profile, domains := m.Profile, m.DomainProfiles
		if in.Profile != nil {
			profile = strings.TrimSpace(*in.Profile)
		}
		if in.DomainProfiles != nil {
			domains = *in.DomainProfiles
		}
		profileChanged, err = setMemberProfile(tx, &m, profile, domains, "edit", "用户直接编辑了人物设定", nil)
		if err != nil {
			a.fail(w, err)
			return
		}
		if profileChanged {
			if err = queueProfileWakeReview(tx, m); err != nil {
				a.fail(w, err)
				return
			}
		}
	}
	if in.Color != nil {
		m.Color = *in.Color
	}
	if in.Avatar != nil {
		m.Avatar = *in.Avatar
	}
	if in.AvatarIcon != nil {
		m.AvatarIcon = *in.AvatarIcon
	}
	if _, err = tx.Exec(`UPDATE members SET data=? WHERE id=?`, asJSON(m), m.ID); err != nil {
		a.fail(w, err)
		return
	}
	if _, err = tx.Exec(`UPDATE conversations SET name=? WHERE kind='direct' AND id IN (SELECT conversation_id FROM participants WHERE member_id=?)`, m.Name, m.ID); err != nil {
		a.fail(w, err)
		return
	}
	// 新设定立即生效：已用旧人物说明生成但尚未发布的内容不能跨过这次修改。
	// 单独更换头像/颜色不影响成员正在想说的话，也不取消聊天中的后台行动。
	if nameChanged || profileChanged {
		if _, err = tx.Exec(`UPDATE conversations SET revision=revision+1 WHERE id IN (SELECT conversation_id FROM participants WHERE member_id=?)`, m.ID); err != nil {
			a.fail(w, err)
			return
		}
	}
	if profileChanged {
		if err := markIdentityRetry(tx, m.ID, ""); err != nil {
			a.fail(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		a.fail(w, err)
		return
	}
	a.engine.Hub.Publish("")
	respond(w, 200, m)
}

func validateQuestion(q Question) error {
	if strings.TrimSpace(q.Text) == "" || len(q.Options) < 2 {
		return fmt.Errorf("一个问题需要 至少 2 个简短选项")
	}
	seen := map[string]bool{}
	for _, option := range q.Options {
		option = strings.TrimSpace(option)
		if option == "" || seen[option] {
			return fmt.Errorf("问题选项需要简短且互不重复")
		}
		seen[option] = true
	}
	return nil
}

// 每次新调用发布自己的问题；同次调用恢复由 Toolbox 处理，不替换成无关旧问题。
func (s *Store) AskQuestion(c Conversation, m Member, q Question, body string, explicit bool) (Message, error) {
	if !contains(c.MemberIDs, m.ID) {
		return Message{}, fmt.Errorf("提问成员不在当前聊天中")
	}
	if err := validateQuestion(q); err != nil {
		return Message{}, err
	}
	out := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: m.ID, Body: strings.TrimSpace(body), Question: &q, QuestionStatus: "open", CreatedAt: now()}
	if out.Body == "" {
		out.Body = q.Text
	}
	err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		var paused bool
		if err := tx.QueryRow(`SELECT discussion_paused FROM conversations WHERE id=?`, c.ID).Scan(&paused); err != nil {
			return err
		}
		if paused && !explicit {
			return ErrStale
		}
		return insertMessage(tx, out)
	})
	return out, err
}

// 首次见面直接展示生效的人物说明。情景问题由用户在资料中按需发起，
// 不再把统一的相处选择题当作每个成员的第一句话。
func (s *Store) EnsureWelcome(c Conversation) error {
	if c.Kind != "direct" || len(c.MemberIDs) != 1 {
		return nil
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id=?`, c.ID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	m, err := s.Member(c.MemberIDs[0])
	if err != nil {
		return err
	}
	return s.Publish(Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: m.ID, Body: m.Profile, CreatedAt: now()}, c.Revision)
}

func (a *API) memberQuestion(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConversationID string `json:"conversation_id"`
	}
	if !readBody(w, r, &in) {
		return
	}
	m, err := a.engine.Store.Member(r.PathValue("id"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "成员不存在"})
		return
	}
	c, err := a.engine.Store.Conversation(in.ConversationID)
	if err != nil || c.Kind != "direct" || !contains(c.MemberIDs, m.ID) {
		respond(w, 400, map[string]string{"error": "请在这位朋友的私聊里聊偏好"})
		return
	}
	// 已有问题时先返回它，不为重复点击再发模型请求或再插入一张卡片。
	messages, err := a.engine.Store.Messages(c.ID, 0, 60)
	if err != nil {
		a.fail(w, err)
		return
	}
	for _, message := range messages {
		if message.Question != nil && message.QuestionStatus == "open" {
			respond(w, 200, message)
			return
		}
	}
	ctx := withUsageCall(r.Context(), usageCall{member: m.ID, room: c.ID})
	qs, err := CreateQuestions(ctx, a.engine.Model, m.Profile)
	if err != nil {
		a.fail(w, err)
		return
	}
	message, err := a.engine.Store.AskQuestion(c, m, qs[0], "一起调整我的特质："+qs[0].Text, true)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.engine.Hub.Publish(c.ID)
	respond(w, 201, message)
}
