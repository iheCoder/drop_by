package dropby

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ChatMessage struct {
	ResponseItems []json.RawMessage `json:"-"` // Responses 的推理与工具项目仅在本次行动中回传。
	Role          string            `json:"role"`
	Content       string            `json:"content"`
	ToolCalls     []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID    string            `json:"tool_call_id,omitempty"`
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

// 测试替换这个边界即可检验真实调度、取消和落盘路径；生产只使用 HTTPModel。
type Model interface {
	Complete(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error)
}

// 只标记本次调用需要深入推理；不改变成员身份，也不持久化额外生命周期。
type deepReasoningKey struct{}

type HTTPModel struct {
	Config Config
	Store  *Store
	Client *http.Client
}

type Usage struct {
	Day          string `json:"day"`
	Calls        int    `json:"calls"`
	CostMicros   int64  `json:"cost_micros"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// 预算按上海日期结算，重启不会清空。请求前按输入字节数和最大输出 token 预留，
// 并发请求不能各自以为自己还有完整余额。未知结果保留预留额，宁可少用也不越界。
func budgetDay() string {
	return time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
}
func nextBudgetDay() time.Time {
	t := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
}
func (s *Store) Reserve(c Config, cost int64) (string, error) {
	day := budgetDay()
	tx, e := s.db.Begin()
	if e != nil {
		return day, e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`INSERT OR IGNORE INTO usage(day) VALUES(?)`, day); e != nil {
		return day, e
	}
	r, e := tx.Exec(`UPDATE usage SET calls=calls+1,cost_micros=cost_micros+? WHERE day=? AND calls<? AND cost_micros+?<=?`, cost, day, c.DailyCalls, cost, c.DailyBudgetMicros)
	if e != nil {
		return day, e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return day, e
	}
	if n == 0 {
		return day, ErrBudget
	}
	return day, tx.Commit()
}
func (s *Store) Settle(day string, reserved, actual int64, input, output int) error {
	_, e := s.db.Exec(`UPDATE usage SET cost_micros=max(0,cost_micros-?+?),input_tokens=input_tokens+?,output_tokens=output_tokens+? WHERE day=?`, reserved, actual, input, output, day)
	return e
}
func (s *Store) Usage() (Usage, error) {
	u := Usage{Day: budgetDay()}
	e := s.db.QueryRow(`SELECT calls,cost_micros,input_tokens,output_tokens FROM usage WHERE day=?`, u.Day).Scan(&u.Calls, &u.CostMicros, &u.InputTokens, &u.OutputTokens)
	if errors.Is(e, sql.ErrNoRows) {
		e = nil
	}
	return u, e
}

type modelResult struct {
	Choices []struct {
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (m *HTTPModel) Complete(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool) (ChatMessage, error) {
	// 使用副本调整请求配置，避免多个成员并发调用时互相污染。
	local := *m
	m = &local
	if deep, _ := ctx.Value(deepReasoningKey{}).(bool); deep {
		m.Config.Reasoning = m.Config.DeepReasoning
		if m.Config.DeepMaxOutputTokens > 0 {
			m.Config.MaxOutputTokens = m.Config.DeepMaxOutputTokens
		}
	}
	if m.Config.APIKey == "" {
		return ChatMessage{}, ErrUnavailable
	}
	body := map[string]any{"model": m.Config.Model, "messages": messages, "max_tokens": m.Config.MaxOutputTokens, "stream": false}
	// OpenAI 的 GPT-5 使用新的输出额度字段；按本次交流阶段分配推理强度。
	if strings.HasPrefix(m.Config.Model, "gpt-5") {
		delete(body, "max_tokens")
		body["max_completion_tokens"] = m.Config.MaxOutputTokens
		if m.Config.Reasoning != "" {
			body["reasoning_effort"] = m.Config.Reasoning
		}
	}
	if len(defs) > 0 {
		body["tools"] = defs
	}
	if jsonOutput {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if strings.Contains(m.Config.BaseURL, "api.deepseek.com") {
		body["thinking"] = map[string]string{"type": "disabled"}
	}
	endpoint, _ := url.Parse(m.Config.BaseURL)
	useResponses := endpoint.Hostname() == "api.openai.com" || (m.Config.Reasoning != "" && m.Config.Reasoning != "none" && strings.HasPrefix(m.Config.Model, "gpt-5"))
	encoded, e := json.Marshal(body)
	if useResponses {
		encoded, e = m.responseBody(messages, defs, jsonOutput)
	}
	if e != nil {
		return ChatMessage{}, e
	}
	// 给超长工具输出一个明确边界，避免无限上下文吞掉预算；这不影响原始聊天的保存。
	if len(encoded) > 180_000 {
		return ChatMessage{}, fmt.Errorf("本轮上下文过长，请缩小当前探索范围")
	}
	reserve := int64(math.Ceil(float64(len(encoded))*m.Config.InputPrice + float64(m.Config.MaxOutputTokens)*m.Config.OutputPrice))
	day, e := m.Store.Reserve(m.Config, reserve)
	if e != nil {
		return ChatMessage{}, e
	}
	var response modelResult
	// OpenAI 全部走推荐的 Responses；聊天接口仅保留给原有兼容服务。
	if useResponses {
		response, e = m.responses(ctx, encoded)
	} else {
		e = requestJSON(ctx, m.Client, m.Config.BaseURL+"/chat/completions", m.Config.APIKey, encoded, &response)
	}
	if e != nil {
		return ChatMessage{}, e
	}
	if response.Usage.PromptTokens+response.Usage.CompletionTokens > 0 {
		actual := int64(math.Ceil(float64(response.Usage.PromptTokens)*m.Config.InputPrice + float64(response.Usage.CompletionTokens)*m.Config.OutputPrice))
		if e = m.Store.Settle(day, reserve, actual, response.Usage.PromptTokens, response.Usage.CompletionTokens); e != nil {
			return ChatMessage{}, e
		}
	}
	if len(response.Choices) == 0 {
		return ChatMessage{}, fmt.Errorf("模型没有返回可用结果")
	}
	c := response.Choices[0]
	if c.FinishReason == "length" {
		return ChatMessage{}, fmt.Errorf("模型输出达到长度上限，本次未发布不完整内容")
	}
	if strings.TrimSpace(c.Message.Content) == "" && len(c.Message.ToolCalls) == 0 {
		return ChatMessage{}, fmt.Errorf("模型返回了空内容，本次没有生成消息")
	}
	return c.Message, nil
}

func requestJSON(ctx context.Context, client *http.Client, endpoint, key string, body []byte, out any) error {
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	r, e := client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("服务连接失败或超时")
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return fmt.Errorf("外部服务返回 HTTP %d，本次未获得可用结果", r.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("读取服务结果失败")
	}
	if len(b) > 4<<20 {
		return fmt.Errorf("服务结果超过大小限制")
	}
	if e = json.Unmarshal(b, out); e != nil {
		return fmt.Errorf("服务返回了无法识别的 JSON")
	}
	return nil
}

func modelJSON(ctx context.Context, m Model, system string, input any, out any) error {
	r, e := m.Complete(ctx, []ChatMessage{{Role: "system", Content: system + "\n只返回 JSON 对象。"}, {Role: "user", Content: asJSON(input)}}, nil, true)
	if e != nil {
		return e
	}
	return parseJSON(r.Content, out)
}
func parseJSON(raw string, out any) error {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) > 2 {
			raw = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}
	if e := json.Unmarshal([]byte(raw), out); e != nil {
		return fmt.Errorf("模型结果格式不完整，本次没有提交")
	}
	return nil
}

func CreateQuestions(ctx context.Context, m Model, direction string) ([]Question, error) {
	var out struct {
		Questions []Question `json:"questions"`
	}
	e := modelJSON(ctx, m, `你在帮助用户创造一个独立 AI 朋友。根据用户给出的方向，提出 3 个具体情境问题，了解这个成员如何好奇、作判断和与人相处。不是人格评分，不把成员固定为支持者/反对者。包含选择题和可自填的问题，选择题提供 2-4 个有真实取舍的选项，所有题都可自填。不要虚构共同经历。返回 {"questions":[{"text":"...","options":["...","..."]},{"text":"...","options":[]}]}`, direction, &out)
	if e != nil {
		return nil, e
	}
	if len(out.Questions) < 2 || len(out.Questions) > 5 {
		return nil, fmt.Errorf("情境问题数量异常")
	}
	for _, q := range out.Questions {
		if strings.TrimSpace(q.Text) == "" || len([]rune(q.Text)) > 500 || len(q.Options) > 6 {
			return nil, fmt.Errorf("情境问题内容异常")
		}
	}
	return out.Questions, nil
}
func CompleteMember(ctx context.Context, model Model, m Member) (Member, error) {
	var out struct {
		Profile string   `json:"profile"`
		Tools   []string `json:"tools"`
	}
	e := modelJSON(ctx, model, `根据用户的大方向和情境回答写一份 400 字内的成员起始说明，包含关注点、选择材料的倾向、判断方式、参与意愿、可以自然变化的地方。保留矛盾和余地，不使用性格数值，不固定站队，不编造真实行动或已经发生的关系。工具池只有 search（搜索）、read_page（网页阅读）、save_artifact（保存和修改文本成果），根据成员方向选择合适工具；不需要用户配置模型或工具。返回 {"profile":"自然语言说明","tools":["search","read_page","save_artifact"]}`, m, &out)
	if e != nil {
		return m, e
	}
	if strings.TrimSpace(out.Profile) == "" {
		return m, fmt.Errorf("成员说明为空")
	}
	m.ID = newID("member")
	m.Profile = clip(out.Profile, 3000)
	m.CreatedAt = now()
	m.Tools = []string{}
	for _, name := range out.Tools {
		if contains([]string{"search", "read_page", "save_artifact"}, name) && !contains(m.Tools, name) {
			m.Tools = append(m.Tools, name)
		}
	}
	// 真实探索和轻量创作是产品的基本能力；模型未给出有效选择时保留安全工具池。
	if len(m.Tools) == 0 {
		m.Tools = []string{"search", "read_page", "save_artifact"}
	}
	return m, nil
}

// 高推理工具链需携带供应商返回的推理项目，不能只把工具结果拼成聊天文本。
// store=false + 加密推理项目让服务不依赖供应商保存会话；原始推理不显示给用户。
func (m *HTTPModel) responseBody(messages []ChatMessage, defs []ToolDefinition, jsonOutput bool) ([]byte, error) {
	input := []any{}
	for _, msg := range messages {
		if len(msg.ResponseItems) > 0 {
			for _, item := range msg.ResponseItems {
				input = append(input, item)
			}
			continue
		}
		if msg.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": msg.ToolCallID, "output": msg.Content})
			continue
		}
		input = append(input, map[string]any{"role": msg.Role, "content": msg.Content})
	}
	body := map[string]any{"model": m.Config.Model, "input": input, "max_output_tokens": m.Config.MaxOutputTokens, "store": false, "include": []string{"reasoning.encrypted_content"}}
	if strings.HasPrefix(m.Config.Model, "gpt-5") && m.Config.Reasoning != "" {
		body["reasoning"] = map[string]string{"effort": m.Config.Reasoning}
	}
	if len(defs) > 0 {
		tools := []any{}
		for _, d := range defs {
			tools = append(tools, map[string]any{"type": "function", "name": d.Function.Name, "description": d.Function.Description, "parameters": d.Function.Parameters, "strict": false})
		}
		body["tools"] = tools
	}
	if jsonOutput {
		body["text"] = map[string]any{"format": map[string]string{"type": "json_object"}}
	}
	return json.Marshal(body)
}
func (m *HTTPModel) responses(ctx context.Context, raw []byte) (modelResult, error) {
	var err error
	var out struct {
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
		Usage  struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err = requestJSON(ctx, m.Client, m.Config.BaseURL+"/responses", m.Config.APIKey, raw, &out); err != nil {
		return modelResult{}, err
	}
	result := modelResult{}
	result.Usage.PromptTokens = out.Usage.Input
	result.Usage.CompletionTokens = out.Usage.Output
	msg := ChatMessage{Role: "assistant", ResponseItems: out.Output}
	for _, rawItem := range out.Output {
		var item struct {
			Type, Name, Arguments string
			CallID                string `json:"call_id"`
			Content               []struct{ Type, Text string }
		}
		if err = json.Unmarshal(rawItem, &item); err != nil {
			return result, err
		}
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					msg.Content += part.Text
				}
			}
		case "function_call":
			call := ToolCall{ID: item.CallID, Type: "function"}
			call.Function.Name = item.Name
			call.Function.Arguments = item.Arguments
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	finish := "stop"
	if out.Status == "incomplete" {
		finish = "length"
	}
	result.Choices = append(result.Choices, struct {
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	}{msg, finish})
	return result, nil
}
