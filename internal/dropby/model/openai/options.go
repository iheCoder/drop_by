package openai

import (
	"encoding/json"
	"fmt"
)

func ValidateOptions(options map[string]any) (limit int, err error) {
	// 这些字段由行动窗口、工具权限或传输实现生成。服务端会话与
	// 本地断点语义不同，因此原生参数不能替换这部分请求。
	for _, key := range []string{"model", "input", "tools", "type", "store", "conversation", "previous_response_id", "stream", "background"} {
		if _, ok := options[key]; ok {
			return 0, fmt.Errorf("options.%s 由 Responses 接入设置", key)
		}
	}
	for _, field := range []struct{ name, managed string }{
		{"text", "format"}, {"prompt_cache_options", "comparison_response_id"},
	} {
		if value, exists := options[field.name]; exists {
			options, ok := value.(map[string]any)
			if !ok {
				return 0, fmt.Errorf("options.%s 必须是对象", field.name)
			}
			if _, exists := options[field.managed]; exists {
				return 0, fmt.Errorf("options.%s.%s 由 Responses 接入设置", field.name, field.managed)
			}
		}
	}

	if value, exists := options["include"]; exists {
		list, ok := value.([]any)
		if !ok {
			return 0, fmt.Errorf("options.include 必须是字符串列表")
		}
		for _, item := range list {
			if _, ok := item.(string); !ok {
				return 0, fmt.Errorf("options.include 必须是字符串列表")
			}
		}
	}

	// 预算和容量检查使用与线上的请求相同的值，确保原生配置不会绕过预留。
	limit = 8000
	if value, exists := options["max_output_tokens"]; exists {
		value, ok := value.(int)
		if !ok || value <= 0 {
			return 0, fmt.Errorf("options.max_output_tokens 必须为正整数")
		}
		limit = value
	}
	// 也检查任意原生扩展（例如 YAML 支持但 JSON 不支持的值），启动时即发现配置错误。
	if _, err := json.Marshal(options); err != nil {
		return 0, fmt.Errorf("options 必须能编码为 JSON")
	}
	return limit, nil
}
