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

	// 字节上界只负责快速放行；超过时交给真实 token 计数与原生压缩，
	// 不能在计数前擅自截短刚读取的正文。
	estimate, err := requestInputUpperBound(messages, encode)
	if err != nil {
		return nil, 0, err
	}
	if estimate <= inputLimit {
		raw, err := encode(messages)
		return raw, estimate, err
	}
	return nil, estimate, fmt.Errorf("输入需要核对实际 token 容量")
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
			if string(fields["type"]) == `"reasoning"` || string(fields["type"]) == `"compaction"` {
				delete(fields, "encrypted_content")
				visible[i].ResponseItems[j], _ = json.Marshal(fields)
			}
		}
	}
	raw, err := encode(visible)
	return len(raw) + reasoningAllowance, err
}
