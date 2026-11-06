package dropby

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// 这里的结构只描述真实记录和执行边界。性格、兴趣、关系与经历保留为自然语言，
// 不把朋友的变化压成分数，也不把每句话都转换成一个待办。
type Member struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Direction  string     `json:"direction"`
	Profile    string     `json:"profile"`
	Color      string     `json:"color"`
	Avatar     string     `json:"avatar,omitempty"`
	AvatarIcon string     `json:"avatar_icon,omitempty"`
	Questions  []Question `json:"questions"`
	Answers    []string   `json:"answers"`
	Tools      []string   `json:"tools"`
	CreatedAt  string     `json:"created_at"`
}

type Question struct {
	Text    string   `json:"text"`
	Options []string `json:"options"`
}

type Conversation struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	MemberIDs []string `json:"member_ids"`
	// 协调者是这一个群里的可选职责，不改变成员的长期性格或其他群里的身份。
	CoordinatorID    string `json:"coordinator_id,omitempty"`
	Revision         int64  `json:"revision"`
	DiscussionPaused bool   `json:"discussion_paused"`
	ResearchPaused   bool   `json:"research_paused"`
	Guidance         string `json:"guidance"`
	UpdatedAt        string `json:"updated_at"`
	LastMessage      string `json:"last_message"`
	LastSeq          int64  `json:"last_seq"`
}

type Message struct {
	Seq            int64    `json:"seq"`
	ID             string   `json:"id"`
	ConversationID string   `json:"conversation_id"`
	AuthorID       string   `json:"author_id"`
	Body           string   `json:"body"`
	ReplyTo        string   `json:"reply_to,omitempty"`
	Mentions       []string `json:"mentions"`
	Evidence       []string `json:"evidence"`
	Notify         bool     `json:"notify"`
	CreatedAt      string   `json:"created_at"`
	// 可选问题是聊天消息的一部分；问题关闭后正文仍留在原位置，不能变成弹窗或必答表单。
	Question       *Question `json:"question,omitempty"`
	QuestionStatus string    `json:"question_status,omitempty"`
	// 仅从这条公开消息的真实工具引用派生；保存作品本身不等于向群里分享。
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

type Memory struct {
	ID             string `json:"id"`
	MemberID       string `json:"member_id"`
	ConversationID string `json:"conversation_id"`
	Body           string `json:"body"`
	SourceID       string `json:"source_id"`
	UpdatedAt      string `json:"updated_at"`
}

// Work 是可以真正执行和恢复的工作，不代表兴趣的生命周期。
// chat/explore 是一次有限的交流机会；research 才是一项已安排的后台行动。
type Work struct {
	// 真实委托者用于任务回报；空串仍表示成员自行安排的行动。
	AssignedBy     string `json:"assigned_by,omitempty"`
	Autonomous     bool   `json:"autonomous,omitempty"` // 计费来源与引用消息分离，记得真人原话不等于持续占用前台额度。
	RootID         string `json:"root_id,omitempty"`    // 同一自主机会的后续共享执行边界，不能靠新建工作重置。
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	MemberID       string `json:"member_id"`
	Kind           string `json:"kind"`
	Instruction    string `json:"instruction"`
	Reason         string `json:"reason"`
	SourceID       string `json:"source_id"`
	Status         string `json:"status"`
	DueAt          int64  `json:"due_at"`
	Attempts       int    `json:"attempts"`
	Result         string `json:"result"`
	CreatedAt      string `json:"created_at"`
}

type ToolRun struct {
	ID             string          `json:"id"`
	WorkID         string          `json:"work_id"`
	ConversationID string          `json:"conversation_id"`
	MemberID       string          `json:"member_id"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	Result         string          `json:"result"`
	Status         string          `json:"status"`
	CreatedAt      string          `json:"created_at"`
}

// Activity 是进程此刻真实执行的动作；Participation 是可回看的一次参与判断。
// 判断记录解释这次为何参与或安静，不把成员的性格固化成活跃度分数。
type Activity struct {
	ConversationID string `json:"conversation_id"`
	MemberID       string `json:"member_id"`
	Label          string `json:"label"`
}
type Participation struct {
	WorkID    string `json:"work_id"`
	MemberID  string `json:"member_id"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
	Selected  bool   `json:"selected"`
	CreatedAt string `json:"created_at"`
}

type Artifact struct {
	// 每个版本保存自己的格式，避免修改 HTML 时悄悄变成 Markdown。
	Format         string `json:"format"`
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	MemberID       string `json:"member_id"`
	Title          string `json:"title"`
	Content        string `json:"content"`
	Version        int    `json:"version"`
	SourceID       string `json:"source_id"`
	CreatedAt      string `json:"created_at"`
}

var (
	ErrStale       = errors.New("用户已补充新消息，本次结果不再适用")
	ErrBudget      = errors.New("本周运行预算不足，未完工作已保留")
	ErrUnavailable = errors.New("尚未配置模型服务，请设置 OPENAI_API_KEY 或 DROPBY_API_KEY")
	ErrRunLimit    = errors.New("本次工具运行已达到上限，未完工作已保留")
)

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
func now() string         { return time.Now().UTC().Format(time.RFC3339Nano) }
func asJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "\n[内容截断]"
	}
	return s
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
