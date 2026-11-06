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
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Direction string     `json:"direction"`
	Profile   string     `json:"profile"`
	Color     string     `json:"color"`
	Questions []Question `json:"questions"`
	Answers   []string   `json:"answers"`
	Tools     []string   `json:"tools"`
	CreatedAt string     `json:"created_at"`
}

type Question struct {
	Text    string   `json:"text"`
	Options []string `json:"options"`
}

type Conversation struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	MemberIDs        []string `json:"member_ids"`
	Revision         int64    `json:"revision"`
	DiscussionPaused bool     `json:"discussion_paused"`
	ResearchPaused   bool     `json:"research_paused"`
	Guidance         string   `json:"guidance"`
	UpdatedAt        string   `json:"updated_at"`
	LastMessage      string   `json:"last_message"`
	LastSeq          int64    `json:"last_seq"`
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

type Artifact struct {
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
	ErrBudget      = errors.New("今日运行预算已用完，未完工作已保留，下一预算日恢复")
	ErrUnavailable = errors.New("尚未配置模型服务，请设置 DROPBY_API_KEY 或 DEEPSEEK_API_KEY")
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
