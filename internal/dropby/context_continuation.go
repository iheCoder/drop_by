package dropby

import (
	"context"
	"encoding/json"
	"fmt"
)

// completeModel 让一次工具循环继续使用压缩后的上下文。普通 Model（包括测试替身）
// 无需实现压缩；HTTP 边界仍负责所有调用的容量检查，包括只有一次请求的核对阶段。
func completeModel(ctx context.Context, model Model, messages *[]ChatMessage, defs []ToolDefinition, jsonOutput bool) (ChatMessage, error) {
	if httpModel, ok := model.(*HTTPModel); ok {
		return httpModel.complete(ctx, *messages, defs, jsonOutput, messages)
	}
	return model.Complete(ctx, *messages, defs, jsonOutput)
}

// countResponseInput 使用实际模型与工具 schema 计数。字节上界只用于快速放行短请求，
// 不能把中文、长作品参数或可见输出的重复估算当成供应商上下文已满。
func (m *HTTPModel) countResponseInput(ctx context.Context, raw []byte) (int, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, err
	}
	// 计数接受输入、模型、工具和推理设置；生成的输出长度与存储设置不属于输入。
	for _, key := range []string{"max_output_tokens", "store", "include", "text"} {
		delete(body, key)
	}
	encoded, _ := json.Marshal(body)
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := retryRequest(ctx, func() error {
		return requestJSON(ctx, m.Client, m.Config.BaseURL+"/responses/input_tokens", m.Config.APIKey, encoded, &out)
	}); err != nil {
		return 0, err
	}
	if out.InputTokens <= 0 {
		return 0, fmt.Errorf("输入容量计数未返回有效结果")
	}
	return out.InputTokens, nil
}

// prepareLargeResponse 在工具调用已成对完成的边界压缩整段行动，而非篡改某个函数
// 的参数或删掉加密推理项目。作品、工具与聊天原文仍在 SQLite，压缩只影响工作窗口。
func (m *HTTPModel) prepareLargeResponse(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool) ([]byte, int, []ChatMessage, error) {
	raw, err := m.responseBody(messages, defs, jsonOutput)
	if err != nil {
		return nil, 0, nil, err
	}
	count, err := m.countResponseInput(ctx, raw)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("无法核对实际输入容量：%w", err)
	}
	capacity := m.Config.ContextTokenBudget
	if capacity == 0 {
		capacity = 100000
	}
	limit := capacity - m.Config.MaxOutputTokens
	if count <= limit {
		return raw, count, messages, nil
	}

	// 稳定规则单独保留并在压缩后重新提供。其余完整 Responses 输入交由原生
	// compaction 维护协议与必要推理状态，不能用手工截断 call arguments 冒充压缩。
	core, history := []ChatMessage{}, []ChatMessage{}
	for _, message := range messages {
		if message.Role == "system" || message.Role == "developer" {
			core = append(core, message)
		} else {
			history = append(history, message)
		}
	}
	encoded, _ := m.responseBody(history, nil, false)
	var request map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &request); err != nil {
		return nil, 0, nil, err
	}
	compactBody, _ := json.Marshal(map[string]any{"model": m.Config.Model, "input": request["input"],
		"instructions": "整理这位成员接下来继续交流所需的上下文。结合当前关注保留有用的原话、不同人的看法、更正、明确授权与未完事项。保持说话者、自己的判断、他人的说法和已确认事实各自的含义，不合并分歧，不把一次表达写成永久特质。暂时无关的长正文可用真实消息或工具 ID、取得时间、版本和状态替代；失败不能写成成功。原文仍可回读，整理后继续当前行动。"})
	price, err := priceForModel(m.Config.Model)
	if err != nil {
		return nil, 0, nil, err
	}
	reserve := price.reserveMicros(count, m.Config.MaxOutputTokens)
	call, err := m.Store.reserveCall(ctx, m.Config, reserve, "OpenAI", m.Config.Model, "compact")
	if err != nil {
		return nil, 0, nil, &modelAccountingError{err}
	}
	defer m.Store.finishUsageCall(call)
	var compacted struct {
		Output []json.RawMessage `json:"output"`
		Usage  responseUsage     `json:"usage"`
	}
	if err = requestJSON(ctx, m.Client, m.Config.BaseURL+"/responses/compact", m.Config.APIKey, compactBody, &compacted); err != nil {
		return nil, 0, nil, fmt.Errorf("上下文压缩尚未完成：%w", err)
	}
	if compacted.Usage.InputTokens+compacted.Usage.OutputTokens > 0 {
		actual, costErr := compacted.Usage.costMicros(price)
		if costErr != nil {
			return nil, 0, nil, costErr
		}
		if err = m.Store.settleCall(call, actual, compacted.Usage, ""); err != nil {
			return nil, 0, nil, &modelAccountingError{err}
		}
	}
	valid := false
	for _, item := range compacted.Output {
		var value struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &value) == nil && value.Type == "compaction" {
			valid = true
		}
	}
	if !valid {
		return nil, 0, nil, fmt.Errorf("%w：服务未返回可续接的压缩记录", ErrContextCapacity)
	}
	// 原生输出整体保留，不能挑选其中几个项目。下一轮从这个窗口继续，避免每轮
	// 都重新压缩原来的长历史、重复花费预算。
	prepared := append(core, ChatMessage{Role: "assistant", ResponseItems: compacted.Output, OutputTokens: compacted.Usage.OutputTokens})
	raw, err = m.responseBody(prepared, defs, jsonOutput)
	if err != nil {
		return nil, 0, nil, err
	}
	count, err = m.countResponseInput(ctx, raw)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("压缩后容量核对未完成：%w", err)
	}
	if count > limit {
		return nil, 0, nil, fmt.Errorf("%w：压缩后输入 %d，输出预留 %d；已保存内容可从记录续接", ErrContextCapacity, count, m.Config.MaxOutputTokens)
	}
	return raw, count, prepared, nil
}
