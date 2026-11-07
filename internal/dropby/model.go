package dropby

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

type ChatMessage struct {
	ResponseItems []json.RawMessage `json:"-"` // Responses 的推理与工具项目仅在本次行动中回传。
	OutputTokens  int               `json:"-"` // 服务商报告的输出量；加密推理不能按 Base64 字节当作输入 token。
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

// 只在现有调用节点标记职责。没有另一次模型分类，也不保存新的任务状态。
// 空职责使用主模型，供控制理解、真实性核对和实际研究使用。
type modelRoleKey struct{}

// 调用记账失败属于共享存储故障，不能被参与层当作某位成员的请求失败忽略。
type modelAccountingError struct{ error }

func (e *modelAccountingError) Unwrap() error { return e.error }

type HTTPModel struct {
	Config Config
	Store  *Store
	Client *http.Client
}

type Usage struct {
	WeekStart            string `json:"week_start"`
	ResetAt              string `json:"reset_at"`
	TimeZone             string `json:"time_zone"`
	AutonomousCostMicros int64  `json:"autonomous_cost_micros"`
	Calls                int    `json:"calls"`
	CostMicros           int64  `json:"cost_micros"`
	InputTokens          int    `json:"input_tokens"`
	OutputTokens         int    `json:"output_tokens"`
}

// 账本仍按日期保存，让升级前本周的真实消费自动计入新额度，不清零、不复制账目。
// 预算范围按服务进程本地时区的周一 00:00 计算；AddDate 保留夏令时切换时的本地午夜。
func budgetDay() string { return time.Now().Format("2006-01-02") }

func budgetWeek(at time.Time) (time.Time, time.Time) {
	midnight := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	daysSinceMonday := (int(at.Weekday()) + 6) % 7
	start := midnight.AddDate(0, 0, -daysSinceMonday)
	return start, start.AddDate(0, 0, 7)
}

func nextBudgetWeek() time.Time {
	_, end := budgetWeek(time.Now())
	return end
}

// 本次请求放不下，并不代表额度已经用完。错误保留准确余额和预留要求，
// 同时仍能被调度器按 ErrBudget 识别；后台子上限也能单独识别而不阻断真人交流。
type budgetRejection struct {
	message    string
	required   int64
	autonomous bool
}

func (e *budgetRejection) Error() string { return e.message }
func (e *budgetRejection) Unwrap() error { return ErrBudget }
func (e *budgetRejection) Is(target error) bool {
	return e.autonomous && target == errAutonomyBudget
}

func (s *Store) Reserve(c Config, cost int64) (string, error) {
	return s.ReserveContext(context.Background(), c, cost)
}

// 同一次请求在事务中汇总本周全部日期，再同时预留总预算和自主预算。
// 保留日期作为结算凭据，跨周返回的响应仍结算到它实际开始调用的那一天。
func (s *Store) ReserveContext(ctx context.Context, c Config, cost int64) (string, error) {
	return s.reserveContextAt(ctx, c, cost, time.Now())
}

func (s *Store) reserveContextAt(ctx context.Context, c Config, cost int64, at time.Time) (string, error) {
	at = at.In(time.Local)
	day := at.Format("2006-01-02")
	start, end := budgetWeek(at)
	autonomous := autonomyRoot(ctx) != ""
	var autoCost int64
	if autonomous {
		autoCost = cost
	}
	if cost < 0 {
		return day, fmt.Errorf("预算预留不能为负数")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return day, err
	}
	defer tx.Rollback()
	// 先持有写事务，避免多个调用先读到同一个旧余额再各自获准。
	if _, err = tx.Exec(`INSERT OR IGNORE INTO usage(day) VALUES(?)`, day); err != nil {
		return day, err
	}
	var calls int
	var total, background int64
	if err = tx.QueryRow(`SELECT coalesce(sum(calls),0),coalesce(sum(cost_micros),0),coalesce(sum(autonomous_cost_micros),0) FROM usage WHERE day>=? AND day<?`, start.Format("2006-01-02"), end.Format("2006-01-02")).Scan(&calls, &total, &background); err != nil {
		return day, err
	}

	// 先判断整体额度，避免总额度也不足时误报“只有自主探索受限”。
	reset := end.Format("1月2日 15:04 MST")
	if calls >= c.WeeklyCalls {
		return day, &budgetRejection{required: cost, message: fmt.Sprintf("本周已达到 %d 次调用上限，未完工作已保留，%s 恢复。", c.WeeklyCalls, reset)}
	}
	remaining := max(int64(0), c.WeeklyBudgetMicros-total)
	if remaining == 0 {
		return day, &budgetRejection{required: cost, message: fmt.Sprintf("本周 $%.2f 运行预算已用完，未完工作已保留，%s 恢复。", float64(c.WeeklyBudgetMicros)/1e6, reset)}
	}
	if cost > remaining {
		return day, &budgetRejection{required: cost, message: fmt.Sprintf("本周可用预算还剩 $%.3f，本次调用需预留 $%.3f，暂时无法开始；未完工作已保留，%s 恢复周额度。", float64(remaining)/1e6, float64(cost)/1e6, reset)}
	}
	if autonomous && cost > max(int64(0), c.AutonomousBudgetMicros-background) {
		autoRemaining := max(int64(0), c.AutonomousBudgetMicros-background)
		return day, &budgetRejection{required: cost, message: fmt.Sprintf("本周自主探索可用额度还剩 $%.3f，本次需预留 $%.3f，已保留后续；真人聊天仍可使用总预算余量，%s 恢复自主额度。", float64(autoRemaining)/1e6, float64(cost)/1e6, reset), autonomous: true}
	}
	if _, err = tx.Exec(`UPDATE usage SET calls=calls+1,cost_micros=cost_micros+?,autonomous_cost_micros=autonomous_cost_micros+? WHERE day=?`, cost, autoCost, day); err != nil {
		return day, err
	}
	return day, tx.Commit()
}

func (s *Store) Settle(day string, reserved, actual int64, input, output int) error {
	return s.SettleContext(context.Background(), day, reserved, actual, input, output)
}
func (s *Store) SettleContext(ctx context.Context, day string, reserved, actual int64, input, output int) error {
	var autoDelta int64
	if autonomyRoot(ctx) != "" {
		autoDelta = actual - reserved
	}
	_, e := s.db.Exec(`UPDATE usage SET cost_micros=max(0,cost_micros-?+?),autonomous_cost_micros=max(0,autonomous_cost_micros+?),input_tokens=input_tokens+?,output_tokens=output_tokens+? WHERE day=?`, reserved, actual, autoDelta, input, output, day)
	return e
}

func (s *Store) Usage() (Usage, error) { return s.weeklyUsageAt(time.Now()) }

func (s *Store) weeklyUsageAt(at time.Time) (Usage, error) {
	at = at.In(time.Local)
	start, end := budgetWeek(at)
	u := Usage{WeekStart: start.Format(time.RFC3339), ResetAt: end.Format(time.RFC3339), TimeZone: at.Location().String()}
	err := s.db.QueryRow(`SELECT coalesce(sum(calls),0),coalesce(sum(cost_micros),0),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0),coalesce(sum(autonomous_cost_micros),0) FROM usage WHERE day>=? AND day<?`, start.Format("2006-01-02"), end.Format("2006-01-02")).Scan(&u.Calls, &u.CostMicros, &u.InputTokens, &u.OutputTokens, &u.AutonomousCostMicros)
	return u, err
}

// 模型边界只保留当前 Responses 的结果，不再转换成旧 choices 协议。
type modelResult struct {
	Message                   ChatMessage
	InputTokens, OutputTokens int
	Incomplete                bool
}

func (m *HTTPModel) Complete(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool) (ChatMessage, error) {
	return m.complete(ctx, messages, defs, jsonOutput, nil)
}

func (m *HTTPModel) complete(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool, chain *[]ChatMessage) (ChatMessage, error) {
	// 使用副本调整请求配置，避免多个成员并发调用时互相污染。
	local := *m
	m = &local
	role, _ := ctx.Value(modelRoleKey{}).(string)
	selected := ""
	switch role {
	case "decision":
		selected = m.Config.DecisionModel
	case "chat":
		selected = m.Config.ChatModel
	}
	if selected != "" {
		m.Config.Model = selected
		m.Config.Reasoning = "none"
	}
	if deep, _ := ctx.Value(deepReasoningKey{}).(bool); deep && selected == "" {
		m.Config.Reasoning = m.Config.DeepReasoning
		if m.Config.DeepMaxOutputTokens > 0 {
			m.Config.MaxOutputTokens = m.Config.DeepMaxOutputTokens
		}
	}
	// 当前本地 API 的 Sol 不接受 none。只修正这个已实测的参数组合，
	// 不因调用失败悄悄换型号或重放已经完成的工具动作。
	if m.Config.Model == "gpt-6.1-sol" && m.Config.Reasoning == "none" {
		m.Config.Reasoning = "low"
	}
	if m.Config.APIKey == "" {
		return ChatMessage{}, ErrUnavailable
	}
	// 在模型边界统一检查完整请求，参与判断、行动和发布核对都不能绕过此处。
	// 只折叠已经存档的工具正文；身份、目标、原话以及调用/推理协议保持完整。
	encoded, inputEstimate, e := boundedRequest(messages, m.Config.ContextTokenBudget, m.Config.MaxOutputTokens, func(input []ChatMessage) ([]byte, error) {
		return m.responseBody(input, defs, jsonOutput)
	})
	capacity := m.Config.ContextTokenBudget
	if capacity == 0 {
		capacity = 100000
	}
	if e != nil && capacity > m.Config.MaxOutputTokens {
		var prepared []ChatMessage
		encoded, inputEstimate, prepared, e = m.prepareLargeResponse(ctx, messages, defs, jsonOutput)
		if e == nil && chain != nil {
			*chain = prepared
		}
	}
	if e != nil {
		return ChatMessage{}, e
	}
	// 工具文本由行动层保留近期正文并折叠旧结果。这里仅限制异常传输体积，
	// 不能用 180 KB 的 HTTP 请求大小冒充模型的上下文窗口。
	if len(encoded) > 8<<20 {
		return ChatMessage{}, fmt.Errorf("本次行动的内部请求过大，已保存工具记录")
	}
	reserve := int64(math.Ceil(float64(inputEstimate)*m.Config.InputPrice + float64(m.Config.MaxOutputTokens)*m.Config.OutputPrice))
	day, e := m.Store.ReserveContext(ctx, m.Config, reserve)
	if e != nil {
		return ChatMessage{}, &modelAccountingError{e}
	}
	// 只有 Responses 一条调用路径；失败直接报告，不自动切换接口或重放工具。
	response, e := m.responses(ctx, encoded)
	if e != nil {
		return ChatMessage{}, e
	}
	if response.InputTokens+response.OutputTokens > 0 {
		actual := int64(math.Ceil(float64(response.InputTokens)*m.Config.InputPrice + float64(response.OutputTokens)*m.Config.OutputPrice))
		if e = m.Store.SettleContext(ctx, day, reserve, actual, response.InputTokens, response.OutputTokens); e != nil {
			return ChatMessage{}, &modelAccountingError{e}
		}
	}
	if response.Incomplete {
		return ChatMessage{}, fmt.Errorf("模型输出达到长度上限，本次未发布不完整内容")
	}
	if strings.TrimSpace(response.Message.Content) == "" && len(response.Message.ToolCalls) == 0 {
		return ChatMessage{}, fmt.Errorf("模型返回了空内容，本次没有生成消息")
	}
	response.Message.OutputTokens = response.OutputTokens
	return response.Message, nil
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
	e := modelJSON(ctx, m, `用户自愿想补充一个 AI 朋友的偏好。只提出一个简短、具体、易回答的情境问题，提供 2-4 个自然选项，也允许自填或跳过。没有已知方向时问一个轻松的相处偏好。不是入门测验或人格评分，不固定支持/反对，不虚构共同经历。返回 {"questions":[{"text":"...","options":["...","..."]}]}`, direction, &out)
	if e != nil {
		return nil, e
	}
	if len(out.Questions) != 1 {
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
	input := map[string]any{"name": m.Name, "direction": m.Direction, "existing_profile": m.Profile, "questions": m.Questions, "answers": m.Answers}
	e := modelJSON(ctx, model, `根据已有说明和用户这次自愿提供的情境回答，合并为 400 字内的成员说明。保留既有兴趣，只修改被这次回答影响的部分；未提供的偏好留有余地。包含关注点、判断方式、参与意愿、可以自然变化的地方。不使用性格数值，不固定站队，不编造真实行动或已经发生的关系。工具池只有 search（搜索）、read_page（网页阅读）、save_artifact（保存和修改 Markdown 或 HTML 成果；HTML 用 format=html，可制作交互小网页），根据成员方向选择合适工具。所有成员另有对话提问 ask_question、安排后续 schedule_followup、取消过时后续 cancel_work 等本地能力，不要把这些列入 tools 数组。返回 {"profile":"自然语言说明","tools":["search","read_page","save_artifact"]}`, input, &out)
	if e != nil {
		return m, e
	}
	if strings.TrimSpace(out.Profile) == "" {
		return m, fmt.Errorf("成员说明为空")
	}
	if m.ID == "" {
		m.ID = newID("member")
	}
	m.Profile = clip(out.Profile, 3000)
	if m.CreatedAt == "" {
		m.CreatedAt = now()
	}
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
	if m.Config.Reasoning != "" {
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
	result.InputTokens = out.Usage.Input
	result.OutputTokens = out.Usage.Output
	msg := ChatMessage{Role: "assistant", ResponseItems: out.Output}
	finalText, unphasedText := "", ""
	hasFinal := false
	for _, rawItem := range out.Output {
		var item struct {
			Type, Name, Arguments, Phase string
			CallID                       string `json:"call_id"`
			Content                      []struct{ Type, Text string }
		}
		if err = json.Unmarshal(rawItem, &item); err != nil {
			return result, err
		}
		switch item.Type {
		case "message":
			// 新模型可能同时返回 commentary 与 final_answer。中间说明不是
			// 业务 JSON，不能拼接成最终答案；原始项目仍完整保留用于下一轮续接。
			if item.Phase == "final_answer" {
				hasFinal = true
			}
			for _, part := range item.Content {
				if part.Type == "output_text" {
					if item.Phase == "final_answer" {
						finalText += part.Text
					} else if item.Phase == "" {
						unphasedText += part.Text
					}
				}
			}
		case "function_call":
			call := ToolCall{ID: item.CallID, Type: "function"}
			call.Function.Name = item.Name
			call.Function.Arguments = item.Arguments
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
	}
	msg.Content = unphasedText
	if hasFinal {
		msg.Content = finalText
	}
	result.Message = msg
	result.Incomplete = out.Status == "incomplete"
	return result, nil
}
