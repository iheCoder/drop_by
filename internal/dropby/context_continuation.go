package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"drop_by/internal/dropby/model"
)

// completeModel 让一次工具循环继续使用压缩后的上下文。普通 Model（包括测试替身）
// 无需实现压缩；HTTP 边界仍负责所有调用的容量检查，包括只有一次请求的核对阶段。
func completeModel(ctx context.Context, model Model, messages *[]ChatMessage, defs []ToolDefinition, jsonOutput bool) (ChatMessage, error) {
	if httpModel, ok := model.(*HTTPModel); ok {
		return httpModel.complete(ctx, *messages, defs, jsonOutput, messages)
	}
	return model.Complete(ctx, *messages, defs, jsonOutput)
}

// 计数重试和记录归应用管理，计数请求的字段及用量由供应商解释。
func (m *modelCall) countResponseInput(ctx context.Context, raw []byte) (int, error) {
	counter, ok := m.backend.(model.InputCounter)
	if !ok {
		return 0, fmt.Errorf("%w：当前接入点未提供精确输入计数", ErrContextCapacity)
	}
	payload, err := counter.CountBody(raw)
	if err != nil {
		return 0, err
	}
	var count int
	var previous string
	attempt := 0
	err = retryRequest(ctx, func() error {
		attempt++
		value, response, err := counter.Count(ctx, payload)
		count = value
		previous = noteInputCount(ctx, attempt, previous, string(payload), scrub(response, m.apiKey), err)
		return err
	})
	return count, err
}

// 真实请求放不下时，先为待交付的内容留空间，再处理已经读过的历史。
// 本人整理选中的内容、最新人物和行动目标继续由应用原样交付。
func (m *modelCall) prepareLargeResponse(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool) ([]byte, int, []ChatMessage, error) {
	encode := func(window []ChatMessage) ([]byte, error) { return m.responseBody(window, defs, jsonOutput) }
	recount := func(window []ChatMessage) ([]byte, int, error) {
		raw, err := encode(window)
		if err != nil {
			return nil, 0, err
		}
		// 没有远程计数的接入点用同一套本地规则检查缩小后的窗口，
		// 仍按原流程保留未交付内容和工具回执。
		if m.TokenCounting == "local" {
			count, err := m.backend.Estimate(window, func([]ChatMessage) ([]byte, error) { return raw, nil })
			return raw, count, err
		}
		count, err := m.countResponseInput(ctx, raw)
		return raw, count, err
	}
	raw, count, err := recount(messages)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("无法核对实际输入容量：%w", err)
	}
	capacity := m.ContextTokenBudget
	if capacity == 0 {
		capacity = 100000
	}
	limit := capacity - m.maxOutputTokens
	if count <= limit {
		return raw, count, messages, nil
	}

	core, history, next := splitCompactionWindow(messages)
	canReadResults, canReadHistory := false, false
	for _, def := range defs {
		canReadResults = canReadResults || def.Function.Name == "read_tool_result"
		canReadHistory = canReadHistory || def.Function.Name == "read_history"
	}
	tooLarge := func(count int) error {
		if m.TokenCounting == "local" {
			return fmt.Errorf("%w：本地估算续接需要 %d 个输入 token，可用 %d，原始记录已保留", ErrContextCapacity, count, limit)
		}
		return fmt.Errorf("%w：续接仍需 %d 个输入 token，可用 %d，原始记录已保留", ErrContextCapacity, count, limit)
	}
	save := func(window []ChatMessage) error {
		if save, ok := ctx.Value(preparedContextKey{}).(savePreparedContext); ok {
			return save(window)
		}
		return nil
	}

	// 先看当前资料自己能否放下。只有实测确实超限，才将其中最占空间的
	// 已存结果改成明确的待交付回执。已有同批短结果仍完整交付。
	required := slices.Concat(core, next)
	deferred := false
	_, _, estimateErr := boundedRequest(required, capacity, m.maxOutputTokens, m.backend.Estimate, encode)
	// 已被供应商拒绝时，即使只保留当前资料也要实际缩短一次，
	// 否则会把刚刚被拒绝的正文再发一遍。
	if len(history) == 0 || estimateErr != nil || m.inputRejected {
		requiredRaw, requiredCount := raw, count
		if len(history) > 0 {
			requiredRaw, requiredCount, err = recount(required)
			if err != nil {
				return nil, 0, nil, fmt.Errorf("无法核对当前资料容量：%w", err)
			}
		}
		for requiredCount > limit || (m.inputRejected && !deferred && canReadResults) {
			if !canReadResults || !deferLargestResult(required) {
				if requiredCount > limit {
					return nil, 0, nil, tooLarge(requiredCount)
				}
				break
			}
			deferred = true
			requiredRaw, requiredCount, err = recount(required)
			if err != nil {
				return nil, 0, nil, fmt.Errorf("无法核对分段续接容量：%w", err)
			}
		}
		if len(history) == 0 {
			return requiredRaw, requiredCount, required, save(required)
		}
		core, next = required[:len(core)], required[len(core):]
	}
	if deferred {
		window := slices.Concat(core, history, next)
		raw, count, err = recount(window)
		if err != nil {
			return nil, 0, nil, err
		}
		if count <= limit {
			return raw, count, window, save(window)
		}
	}
	if _, ok := m.backend.(model.CompactionSession); !ok {
		// 接入点没有原生压缩时，只替换已读且可按来源回查的正文。
		// 当前目标、未交付内容以及函数调用与回执的配对均保留。
		shorter, changed := referenceOldContext(history, canReadHistory, canReadResults)
		if changed {
			window := slices.Concat(core, shorter, next)
			raw, count, err = recount(window)
			if err != nil {
				return nil, 0, nil, err
			}
			if count <= limit {
				return raw, count, window, save(window)
			}
		}
		return nil, 0, nil, tooLarge(count)
	}

	compact := func(history []ChatMessage, count int) ([]byte, int, []ChatMessage, error) {
		item, err := m.compactHistory(ctx, history, count)
		if err != nil {
			return nil, 0, nil, err
		}
		window := slices.Concat(core, []ChatMessage{item}, next)
		raw, actual, err := recount(window)
		if err != nil {
			// 计数中断时，能由保守上界确认放得下的压缩成果仍可落盘。
			// 无法确认的候选保留在本次请求中，断点仍从原来的历史恢复。
			if _, _, estimateErr := boundedRequest(window, capacity, m.maxOutputTokens, m.backend.Estimate, encode); estimateErr == nil {
				if saveErr := save(window); saveErr != nil {
					return nil, 0, nil, saveErr
				}
			}
			return nil, 0, nil, fmt.Errorf("压缩后容量核对未完成：%w", err)
		}
		if actual <= limit {
			err = save(window)
		}
		return raw, actual, window, err
	}
	preparedRaw, preparedCount, prepared, err := compact(history, count)
	if err != nil {
		return nil, 0, nil, err
	}
	if preparedCount <= limit {
		return preparedRaw, preparedCount, prepared, nil
	}

	// 供应商可能原样保留旧原话。重新从本地旧历史出发，把可回读的旧正文
	// 换成来源。若已经能继续，直接生成，避免再花一次压缩费用。
	shorter, changed := referenceOldContext(history, canReadHistory, canReadResults)
	if changed {
		window := slices.Concat(core, shorter, next)
		raw, smaller, err := recount(window)
		if err != nil {
			return nil, 0, nil, err
		}
		if smaller <= limit {
			return raw, smaller, window, save(window)
		}
		// 只有输入确实变小时才尝试第二次，仍完整使用新的原生输出。
		if smaller < count {
			retryRaw, retryCount, retryWindow, err := compact(shorter, smaller)
			if err != nil {
				return nil, 0, nil, err
			}
			if retryCount <= limit {
				return retryRaw, retryCount, retryWindow, nil
			}
			if retryCount < preparedCount {
				prepared, preparedCount = retryWindow, retryCount
			}
		}
	}

	// 已读历史的替代路径都试过后，仍可能是压缩状态与一份新资料合起来
	// 放不下。沿用原生窗口，只延期超限的可回读资料，避免重复付费压缩。
	if canReadResults {
		deferredWindow := slices.Clone(prepared)
		for deferLargestResult(deferredWindow) {
			raw, actual, err := recount(deferredWindow)
			if err != nil {
				return nil, 0, nil, err
			}
			if actual <= limit {
				return raw, actual, deferredWindow, save(deferredWindow)
			}
			preparedCount = actual
		}
	}
	return nil, 0, nil, tooLarge(preparedCount)
}

// 请求组装已把当前现场和行动说明移到末尾。压缩只接收已读前缀，遇到
// 待交付工具回执时把原 assistant 批次一并保留，确保每个 call_id 都成对。
func splitCompactionWindow(messages []ChatMessage) (core, history, next []ChatMessage) {
	messages, _ = memberRequestMessages(messages)
	var live, transcript []ChatMessage
	for _, item := range messages {
		switch {
		case item.Role == "system" || item.Role == "developer":
			core = append(core, item)
		case item.Role == "user" && len(item.ResponseItems) == 0 && strings.HasPrefix(item.Content, organizedContextPrefix):
			core = append(core, item)
		case sceneFields(item) != nil || (item.Role == "user" && len(item.ResponseItems) == 0 && strings.HasPrefix(item.Content, actionCausePrefix)):
			live = append(live, item)
		default:
			transcript = append(transcript, item)
		}
	}
	boundary, assistant := len(transcript), -1
	for i, item := range transcript {
		if item.Role == "assistant" {
			assistant = i
		}
		if item.PendingDelivery {
			boundary = i
			if item.Role == "tool" && assistant >= 0 {
				boundary = assistant
			}
			break
		}
	}
	history, goal := retainActionSource(transcript[:boundary], live)
	return core, history, slices.Concat(goal, transcript[boundary:], live)
}

// 行动来由通常只带任务与消息 ID，原话已经在历史中去重。容量恢复时把这条
// 仍在处理的原话单独带上，避免只留下一个 ID 或由压缩替成员解释它。
func retainActionSource(history, live []ChatMessage) ([]ChatMessage, []ChatMessage) {
	var source string
	for _, item := range live {
		if raw, ok := strings.CutPrefix(item.Content, actionCausePrefix); ok {
			var cause struct {
				Trigger Work `json:"trigger"`
			}
			if json.Unmarshal([]byte(raw), &cause) == nil {
				source = cause.Trigger.SourceID
			}
		}
	}
	if source == "" {
		return history, nil
	}
	for i, item := range history {
		prefix, messages, ok := messageBlock(item)
		if !ok {
			continue
		}
		for j, message := range messages {
			if message.ID == source {
				remaining := slices.Delete(messages, j, j+1)
				kept := slices.Clone(history)
				if len(remaining) == 0 {
					kept = slices.Delete(kept, i, i+1)
				} else {
					kept[i].Content = prefix + asJSON(remaining)
				}
				return kept, appendDeliveredMessages(nil, []Message{message})
			}
		}
	}
	return history, nil
}

// 原生压缩只接收历史，使用供应商默认的推理续接。应用拥有的最新现场留在
// 外面，原生返回项按原顺序整体回传，包括供应商保留的用户消息。
func (m *modelCall) compactHistory(ctx context.Context, history []ChatMessage, count int) (msg ChatMessage, err error) {
	encoded, err := m.responseBody(history, nil, false)
	if err != nil {
		return ChatMessage{}, err
	}
	compactor, ok := m.backend.(model.CompactionSession)
	if !ok {
		return ChatMessage{}, fmt.Errorf("%w：当前接入点未提供原生压缩", ErrContextCapacity)
	}
	compactBody, err := compactor.CompactBody(encoded)
	if err != nil {
		return ChatMessage{}, err
	}
	// 预留只针对发给压缩的旧输入。
	if upper, err := m.backend.Estimate(history, func(input []ChatMessage) ([]byte, error) {
		return m.responseBody(input, nil, false)
	}); err == nil {
		count = min(count, upper)
	}
	reserve := m.reserveMicros(count, m.maxOutputTokens)
	stepID := beginCompact(ctx, string(compactBody))
	var call usageReservation
	settled := false
	var actual int64
	var result modelResult
	defer func() {
		finishCompact(ctx, stepID, result.Raw, err, settled, actual, reserve, call, m.apiKey)
	}()
	call, err = m.Store.reserveCall(ctx, m.Config, reserve, m.Provider, m.Model, "compact")
	if err != nil {
		return ChatMessage{}, &modelAccountingError{err}
	}
	defer m.Store.finishUsageCall(call)
	if result, err = compactor.Compact(ctx, compactBody); err != nil {
		if releaseErr := m.releaseUnsent(call, err); releaseErr != nil {
			return ChatMessage{}, releaseErr
		}
		return ChatMessage{}, fmt.Errorf("上下文压缩尚未完成：%w", err)
	}
	if result.Usage.InputTokens+result.Usage.OutputTokens > 0 {
		actual, err = result.CostMicros, result.CostError
		if err != nil {
			return ChatMessage{}, err
		}
		if err = m.Store.settleCall(call, actual, result.Usage, ""); err != nil {
			return ChatMessage{}, &modelAccountingError{err}
		}
		settled = true
	}
	if len(result.Message.ResponseItems) > 0 {
		return result.Message, nil
	}
	return ChatMessage{}, fmt.Errorf("%w：服务未返回可续接的压缩记录", ErrContextCapacity)
}
