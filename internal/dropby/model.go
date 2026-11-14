package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"drop_by/internal/dropby/model"
	"drop_by/internal/dropby/upstream"
	"drop_by/internal/observe"
)

type ChatMessage = model.ChatMessage
type ToolCall = model.ToolCall
type ToolDefinition = model.ToolDefinition
type ToolFunction = model.ToolFunction
type responseUsage = model.Tokens
type modelResult = model.Result

// 测试替换这个边界即可检验真实调度、取消和落盘路径；生产只使用 HTTPModel。
type Model interface {
	Complete(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error)
}

// 调用记账失败属于共享存储故障，不能被参与层当作某位成员的请求失败忽略。
type modelAccountingError struct{ error }

func (e *modelAccountingError) Unwrap() error { return e.error }

type HTTPModel struct {
	Config    Config
	Store     *Store
	Client    *http.Client
	cacheTime func() time.Time
}

// 应用侧管理完整行动窗口和每笔费用。网络请求与原生状态都由选中的供应商处理。
type modelCall struct {
	*HTTPModel
	modelRoute
	backend       model.Session
	reserveMicros func(input, output int) int64
	outputSchema  *model.OutputSchema
	inputRejected bool
}

type Usage struct {
	UnsettledMicros      int64  `json:"unsettled_micros"` // 新版开始记录的在途/结果未知预留；旧账不倒推。
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
	message  string
	required int64
}

func (e *budgetRejection) Error() string { return e.message }
func (e *budgetRejection) Unwrap() error { return ErrBudget }

func (s *Store) Reserve(c Config, cost int64) (string, error) {
	return s.ReserveContext(context.Background(), c, cost)
}

// 同一次请求在事务中汇总本周全部日期并预留总预算，自主来源只作统计。
// 保留日期作为结算凭据，跨周返回的响应仍结算到它实际开始调用的那一天。
func (s *Store) ReserveContext(ctx context.Context, c Config, cost int64) (string, error) {
	return s.reserveContextAt(ctx, c, cost, time.Now())
}

func (s *Store) reserveContextAt(ctx context.Context, c Config, cost int64, at time.Time) (string, error) {
	reservation, err := s.reserveBudget(ctx, c, cost, at, nil)
	return reservation.day, err
}

func (s *Store) reserveBudget(ctx context.Context, c Config, cost int64, at time.Time, call *usageCall) (usageReservation, error) {
	at = at.In(time.Local)
	day := at.Format("2006-01-02")
	reservation := usageReservation{day: day}
	start, end := budgetWeek(at)
	autonomous := autonomyRoot(ctx) != ""
	var autoCost int64
	if autonomous {
		autoCost = cost
	}
	if cost < 0 {
		return reservation, fmt.Errorf("预算预留不能为负数")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return reservation, err
	}
	defer tx.Rollback()
	// 先持有写事务，避免多个调用先读到同一个旧余额再各自获准。
	if _, err = tx.Exec(`INSERT OR IGNORE INTO usage(day) VALUES(?)`, day); err != nil {
		return reservation, err
	}
	var total int64
	if err = tx.QueryRow(`SELECT coalesce(sum(cost_micros),0) FROM usage WHERE day>=? AND day<?`, start.Format("2006-01-02"), end.Format("2006-01-02")).Scan(&total); err != nil {
		return reservation, err
	}

	// 在同一笔预留事务里读上限，避免保存新档位时这次预留还看着旧配置。
	limit, _, err := budgetLimit(tx, c, at)
	if err != nil {
		return reservation, err
	}
	// 只有一个总额度；预留不足与已用完分别反馈。
	reset := end.Format("1月2日 15:04 MST")
	remaining := max(int64(0), limit-total)
	if remaining == 0 {
		return reservation, &budgetRejection{required: cost, message: fmt.Sprintf("本周 $%.2f 运行预算已用完，未完工作已保留，%s 恢复。", float64(limit)/1e6, reset)}
	}
	if cost > remaining {
		return reservation, &budgetRejection{required: cost, message: fmt.Sprintf("本周可用预算还剩 $%.3f，本次调用需预留 $%.3f，暂时无法开始；未完工作已保留，%s 恢复周额度。", float64(remaining)/1e6, float64(cost)/1e6, reset)}
	}
	if _, err = tx.Exec(`UPDATE usage SET calls=calls+1,cost_micros=cost_micros+?,autonomous_cost_micros=autonomous_cost_micros+?,unsettled_micros=unsettled_micros+? WHERE day=?`, cost, autoCost, cost, day); err != nil {
		return reservation, err
	}
	// 总账与单笔记录一同提交，避免请求已占用额度却没有可追溯的明细。
	if call != nil {
		reservation.id, err = insertUsageCall(tx, *call, day, at, cost, autonomous)
		if err != nil {
			return reservation, err
		}
	}
	return reservation, tx.Commit()
}

func (s *Store) Settle(day string, reserved, actual int64, input, output int) error {
	return s.SettleContext(context.Background(), day, reserved, actual, input, output)
}
func (s *Store) SettleContext(ctx context.Context, day string, reserved, actual int64, input, output int) error {
	var autoDelta int64
	if autonomyRoot(ctx) != "" {
		autoDelta = actual - reserved
	}
	_, e := s.db.Exec(settleDailyUsage, reserved, actual, autoDelta, input, output, reserved, day)
	return e
}

func (s *Store) Usage() (Usage, error) { return s.weeklyUsageAt(time.Now()) }

func (s *Store) weeklyUsageAt(at time.Time) (Usage, error) {
	at = at.In(time.Local)
	start, end := budgetWeek(at)
	u := Usage{WeekStart: start.Format(time.RFC3339), ResetAt: end.Format(time.RFC3339), TimeZone: at.Location().String()}
	err := s.db.QueryRow(`SELECT coalesce(sum(calls),0),coalesce(sum(cost_micros),0),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0),coalesce(sum(autonomous_cost_micros),0),coalesce(sum(unsettled_micros),0) FROM usage WHERE day>=? AND day<?`, start.Format("2006-01-02"), end.Format("2006-01-02")).Scan(&u.Calls, &u.CostMicros, &u.InputTokens, &u.OutputTokens, &u.AutonomousCostMicros, &u.UnsettledMicros)
	return u, err
}

// 只描述供应商响应的形态。生成正文并不等同于发布，非成员调用也不猜测内部决策。
func usageOutcome(ctx context.Context, r model.Result) string {
	if r.Incomplete {
		return "incomplete"
	}
	if len(r.Message.ToolCalls) > 0 {
		return "tool_calls"
	}
	call, _ := ctx.Value(usageCallKey{}).(usageCall)
	if call.purpose == "chat" || call.purpose == "research" {
		var out struct {
			Message string `json:"message"`
		}
		if parseJSON(r.Message.Content, &out) == nil {
			if strings.TrimSpace(out.Message) == "" {
				return "no_message"
			}
			return "generated_message"
		}
	}
	return ""
}

func (m *HTTPModel) Complete(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool) (ChatMessage, error) {
	return m.complete(ctx, messages, defs, jsonOutput, nil)
}

func (m *HTTPModel) complete(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool, chain *[]ChatMessage) (ChatMessage, error) {
	route := m.Config.Models.route(sceneFromContext(ctx))
	if route.apiKey == "" {
		return ChatMessage{}, ErrUnavailable
	}
	call, err := m.newModelCall(route)
	if err != nil {
		return ChatMessage{}, err
	}
	call.outputSchema, _ = ctx.Value(outputSchemaKey{}).(*model.OutputSchema)
	return call.complete(ctx, messages, defs, jsonOutput, chain)
}

func (m *modelCall) responseBody(messages []ChatMessage, defs []ToolDefinition, jsonOutput bool) ([]byte, error) {
	ordered, boundary := memberRequestMessages(messages)
	return m.backend.Build(ordered, boundary, defs, jsonOutput, m.outputSchema)
}

func (m *modelCall) complete(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool, chain *[]ChatMessage) (ChatMessage, error) {
	beginCall(ctx)
	return m.completeWithinBudget(ctx, messages, defs, jsonOutput, chain, m.TokenCounting == "local")
}

func (m *modelCall) completeWithinBudget(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool, chain *[]ChatMessage, allowSmallerRetry bool) (ChatMessage, error) {
	// 在模型边界统一检查完整请求，参与判断与行动共用此处。
	// 不先截短工具正文；保守估算超出时使用实际计数与原生压缩。
	encoded, inputEstimate, e := boundedRequest(messages, m.ContextTokenBudget, m.maxOutputTokens, m.backend.Estimate, func(input []ChatMessage) ([]byte, error) {
		return m.responseBody(input, defs, jsonOutput)
	})
	capacity := m.ContextTokenBudget
	if capacity == 0 {
		capacity = 100000
	}
	if e != nil && capacity > m.maxOutputTokens {
		var prepared []ChatMessage
		encoded, inputEstimate, prepared, e = m.prepareLargeResponse(ctx, messages, defs, jsonOutput)
		if e == nil && chain != nil {
			*chain = prepared
		}
		if e == nil {
			messages = prepared
		}
	}
	if e != nil {
		return ChatMessage{}, e
	}
	if m.TokenCounting == "local" {
		note(ctx, observe.Step{
			ID: observe.NewID("step"), Kind: "request", Name: "input_tokens_local", Status: "succeeded",
			Summary: fmt.Sprintf("本地估算输入 %d token", inputEstimate),
			Output:  asJSON(map[string]int{"estimated_input_tokens": inputEstimate}),
		})
	}
	// 容量检查结束后才观察实际会发送的请求。若压缩过，比较的是整理后的内容。
	cache, _ := ctx.Value(memberCacheKey{}).(memberCacheScope)
	cacheBackend, _ := m.backend.(model.CacheSession)
	if cacheBackend != nil && m.cacheEnabled() && cache.state != nil {
		at := time.Now()
		if m.cacheTime != nil {
			at = m.cacheTime()
		}
		cacheBackend.BeginCache(cache.state.value, cache.workID, at)
		encoded, e = m.responseBody(messages, defs, jsonOutput)
		if e != nil {
			return ChatMessage{}, e
		}
	}
	reserve := m.reserveMicros(inputEstimate, m.maxOutputTokens)
	var response modelResult
	// 每个真正发出的请求单独预留、结算。网络结果未知的旧请求不冒充免费；
	// 自动恢复只重发模型请求，已完成的本地工具不会再执行。
	steering, _ := ctx.Value(steeringKey{}).(*steeringInput)
	if steerer, ok := m.backend.(model.SteeringSession); ok && steering != nil && m.steeringEnabled() {
		response, e = steerer.Steer(ctx, encoded, inputEstimate, steering, chain, m.steeringHooks(ctx))
	} else {
		var previous string
		attempt := 0
		e = retryRequest(ctx, func() (err error) {
			attempt++
			response = modelResult{}
			var call usageReservation
			settled := false
			var actual int64
			stepID := beginModelAttempt(ctx, m.Model, string(encoded), attempt, previous, "")
			previous = stepID
			defer func() {
				finishModelAttempt(ctx, stepID, response.Message, response.Raw, err, settled, actual, reserve, call, m.apiKey)
			}()
			call, err = m.Store.reserveCall(ctx, m.Config, reserve, m.Provider, m.Model, "")
			if err != nil {
				return &modelAccountingError{err}
			}
			defer m.Store.finishUsageCall(call)
			response, err = m.backend.Generate(ctx, encoded)
			if err != nil {
				if releaseErr := m.releaseUnsent(call, err); releaseErr != nil {
					return releaseErr
				}
				return err
			}
			if response.CostError != nil {
				return response.CostError
			}
			if response.Usage.InputTokens+response.Usage.OutputTokens > 0 {
				actual = response.CostMicros
				if err = m.Store.settleCall(call, actual, response.Usage, usageOutcome(ctx, response)); err != nil {
					return &modelAccountingError{err}
				}
				settled = true
			}
			return nil
		})
	}
	if e != nil {
		if allowSmallerRetry && inputEstimate > 1 && errors.Is(e, model.ErrInputTooLarge) {
			// 把这次被拒的输入排除在重试预算外，走原有的回读整理。
			// 只重发一次更小的生成请求，工具执行仍由外层继续。
			smaller := *m
			smaller.ContextTokenBudget = min(capacity, m.maxOutputTokens+inputEstimate-1)
			smaller.inputRejected = true
			return smaller.completeWithinBudget(ctx, messages, defs, jsonOutput, chain, false)
		}
		return ChatMessage{}, e
	}
	if response.Incomplete {
		return ChatMessage{}, fmt.Errorf("模型本次未完成输出")
	}
	if strings.TrimSpace(response.Message.Content) == "" && len(response.Message.ToolCalls) == 0 {
		return ChatMessage{}, fmt.Errorf("模型返回了空内容，本次没有生成消息")
	}
	if cacheBackend != nil && m.cacheEnabled() && cache.state != nil {
		state, observation := cacheBackend.FinishCache(response)
		cache.state.value = state
		recordCache(ctx, observation, m.Model)
	}
	return response.Message, nil
}

func requestJSON(ctx context.Context, client *http.Client, endpoint, key string, body []byte, out any) error {
	return upstream.PostJSON(ctx, serviceClient(client), endpoint, key, body, out, nil)
}

func serviceClient(client *http.Client) *http.Client {
	if client.Transport != nil {
		return client
	}
	copy := *client
	copy.Transport = serviceTransport
	return &copy
}

func modelJSON(ctx context.Context, m Model, system string, input any, out any) error {
	ctx = context.WithValue(ctx, modelSceneKey{}, sceneMemberSettings)
	r, e := m.Complete(ctx, []ChatMessage{{Role: "system", Content: system + "\n只返回 JSON 对象。"}, {Role: "user", Content: asJSON(input), PendingDelivery: true}}, nil, true)
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

type outputSchemaKey struct{}

// 问答只生成这次要展示的一个问题。选项可以为空，真人可以自由回答或跳过。
// 结构校验交给供应商，文本长度、空白等业务校验继续由应用负责。
var memberQuestionsSchema = model.OutputSchema{
	Name: "member_questions",
	Schema: json.RawMessage(`{
		"type":"object","additionalProperties":false,"required":["questions"],
		"properties":{"questions":{"type":"array","minItems":1,"maxItems":1,"items":{
			"type":"object","additionalProperties":false,"required":["text","options"],
			"properties":{"text":{"type":"string"},"options":{"type":"array","maxItems":6,"items":{"type":"string"}}}
		}}}
	}`),
}

func CreateQuestions(ctx context.Context, m Model, direction string) ([]Question, error) {
	call, _ := ctx.Value(usageCallKey{}).(usageCall)
	call.purpose = "preferences"
	ctx = withUsageCall(ctx, call)
	ctx = context.WithValue(ctx, outputSchemaKey{}, &memberQuestionsSchema)
	var out struct {
		Questions []Question `json:"questions"`
	}
	e := modelJSON(ctx, m, `用户自愿通过情景问答调整这位成员的特质。根据当前说明，挑一个值得澄清的倾向，问一个简短的对比场景问题，明确问“希望这位成员怎样”。选项体现不同含义，也允许自由回答或不回答。具体例子帮助理解，不把产品名字当作偏好标签。一次只问一个问题。返回 {"questions":[{"text":"...","options":["...","..."]}]}`, direction, &out)
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
	if model == nil {
		return m, ErrUnavailable
	}
	ctx = withUsageCall(ctx, usageCall{purpose: "preferences", member: m.ID})
	var out profileUpdate
	// 创建方向和旧问答只是历史。修改以当前说明和这次回答为准，工具配置不参与。
	input := map[string]any{"name": m.Name, "current_profile": m.Profile, "current_domain_profiles": m.DomainProfiles, "questions": m.Questions, "answers": m.Answers}
	e := modelJSON(ctx, model, profileEditingPrompt+"\n"+profileAnswerPrompt, input, &out)
	if e != nil {
		return m, e
	}
	domains, err := applyDomainProfileUpdates(m.DomainProfiles, out.DomainProfileUpdates)
	if err != nil {
		return m, err
	}
	m.DomainProfiles = domains
	if out.Profile != nil {
		m.Profile = strings.TrimSpace(*out.Profile)
	}
	return m, nil
}
