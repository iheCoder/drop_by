package model

import (
	"context"
	"encoding/json"
	"time"
)

type ChatMessage struct {
	ResponseItems   []json.RawMessage `json:"response_items,omitempty"` // 原生项目仅由相容的模型接入复用。
	NativeSource    string            `json:"native_source,omitempty"`
	OutputTokens    int               `json:"output_tokens,omitempty"` // 服务商报告的输出量；加密推理不能按 Base64 字节当作输入 token。
	Role            string            `json:"role"`
	Content         string            `json:"content"`
	ToolCalls       []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID      string            `json:"tool_call_id,omitempty"`
	PendingDelivery bool              `json:"pending_delivery,omitempty"` // 已进入断点、尚未交给正常生成的原话或工具回执。
}
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type ToolDefinition struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type Result struct {
	ID              string
	Message         ChatMessage
	Usage           Tokens
	CostMicros      int64 // 按供应商模型费率估算，账本可直接结算。
	CostError       error // 用量有矛盾时保留预留，同时保留响应原文。
	Incomplete      bool
	CacheDiagnostic json.RawMessage // 诊断详情只由所属的适配器解释。
	Raw             string          // 供应商原文，只给观测，不进入成员上下文。
}

type Tokens struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	InputDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// Update 携带应用已经序列化的原话。供应商不需要了解聊天消息的存储结构。
type Update struct {
	ID   string
	Data json.RawMessage
}

func (u Update) MarshalJSON() ([]byte, error) { return u.Data, nil }

type SteeringInput struct {
	Events  <-chan string
	Room    string
	Read    func() ([]Update, error)
	Applied map[string]bool
}

type SteeringCall interface {
	Start(body, parent string)
	Complete(Result, error) error
	Release() error
	Close()
}

type SteeringHooks struct {
	Reserve      func(inputTokens int) (SteeringCall, error)
	Connection   func(attempt int, err error)
	SendUpdate   func(body string, err error) string
	FinishUpdate func(id string, accepted bool)
}

type Session interface {
	Build(messages []ChatMessage, boundary int, tools []ToolDefinition, jsonOutput bool) ([]byte, error)
	Estimate([]ChatMessage, func([]ChatMessage) ([]byte, error)) (int, error)
	Generate(context.Context, []byte) (Result, error)
}

type InputCounter interface {
	CountBody([]byte) ([]byte, error)
	Count(context.Context, []byte) (int, string, error)
}

type CompactionSession interface {
	CompactBody([]byte) ([]byte, error)
	Compact(context.Context, []byte) (Result, error)
}

// 插话和长缓存取决于接入点。只有支持这些能力的会话才实现相应的接口。
type SteeringSession interface {
	Steer(context.Context, []byte, int, *SteeringInput, *[]ChatMessage, SteeringHooks) (Result, error)
}

type CacheSession interface {
	BeginCache(state any, workID string, at time.Time)
	FinishCache(Result) (state any, observation CacheObservation)
}

type CacheObservation struct {
	Message string
	Fields  []any
}
