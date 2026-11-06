package dropby

import (
	"encoding/json"
	"fmt"
)

// boundedRequest 是传输前的最后一道容量检查，不替角色决定什么值得记住。
// 输入按 UTF-8 字节和结构开销估算保守 token 上界，并给本次输出留空间；
// 它不是特定模型的精确 tokenizer，也不宣称这个配置就是供应商的窗口大小。
func boundedRequest(messages []ChatMessage, capacity, output int, encode func([]ChatMessage) ([]byte, error)) ([]byte, int, error) {
	if capacity == 0 {
		capacity = 100_000 // 程序内直接构造的 Config 也使用相同保护。
	}
	inputLimit := capacity - output
	if inputLimit <= 0 {
		return nil, 0, fmt.Errorf("上下文容量配置 %d 不足以预留本次 %d 个输出 token，本次未发送模型请求", capacity, output)
	}

	// 不修改行动层保存的原始消息；本次压缩也不影响下一轮按工具 ID 回读正文。
	working := append([]ChatMessage(nil), messages...)
	estimate, err := requestInputUpperBound(working, encode)
	if err != nil {
		return nil, 0, err
	}
	if estimate <= inputLimit {
		raw, err := encode(working)
		return raw, estimate, err
	}

	// 先从较早的工具结果收缩，一旦能放下就停止，尽量保留眼前正在使用的材料。
	// 每个摘录仍有 tool_run_id；角色可用 read_tool_result 按需取回，原记录不会被删除。
	for _, limit := range []int{4000, 1200, 400, 100} {
		for i := range working {
			if working[i].Role != "tool" {
				continue
			}
			folded := toolExcerpt(working[i].Content, limit)
			if len(folded) >= len(working[i].Content) {
				continue
			}
			working[i].Content = folded
			estimate, err = requestInputUpperBound(working, encode)
			if err != nil {
				return nil, 0, err
			}
			if estimate <= inputLimit {
				raw, err := encode(working)
				return raw, estimate, err
			}
		}
	}
	return nil, estimate, fmt.Errorf("当前请求的固定内容超出上下文容量：输入保守估算 %d，加输出预留 %d，超过配置 %d；人物设定、当前目标和原始记录已保留，本次未发送模型请求", estimate, output, capacity)
}

func requestInputUpperBound(messages []ChatMessage, encode func([]ChatMessage) ([]byte, error)) (int, error) {
	// Responses 的 encrypted_content 是加密传输状态，字节大小不代表输入 token。
	// 估算时只移除这项不透明编码，并加入供应商上轮报告的 OutputTokens；真实请求
	// 仍逐字发送原 ResponseItems，不能丢 call_id 或推理项目来换取“更短”的上下文。
	visible := append([]ChatMessage(nil), messages...)
	reasoningAllowance := 0
	for i, message := range messages {
		reasoningAllowance += max(0, message.OutputTokens)
		if len(message.ResponseItems) == 0 {
			continue
		}
		visible[i].ResponseItems = append([]json.RawMessage(nil), message.ResponseItems...)
		for j, item := range message.ResponseItems {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(item, &fields); err != nil {
				return 0, fmt.Errorf("模型返回的推理或工具协议记录无效，已保留本次行动记录")
			}
			if string(fields["type"]) == `"reasoning"` {
				delete(fields, "encrypted_content")
				visible[i].ResponseItems[j], _ = json.Marshal(fields)
			}
		}
	}
	raw, err := encode(visible)
	return len(raw) + reasoningAllowance, err
}
