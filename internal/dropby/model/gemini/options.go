package gemini

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	Protocol              = "gemini_generate_content"
	Hydrogen              = "gemini-4-hydrogen"
	Argon                 = "gemini-4-argon"
	Carbon                = "gemini-4-carbon"
	Flash                 = "gemini-3.8-flash"
	FlashLite             = "gemini-3.5-flash-lite"
	ContextLimit          = 1_048_576
	maxSyncOutput         = 65_536
	maxSyncFrontierOutput = 32_768 // Argon 和 Carbon 的直接非流式 REST 单次上限。
)

func ValidateOptions(model string, options map[string]any) (int, error) {
	levels := []string{"low", "medium", "high"}
	maxOutput := maxSyncOutput
	switch model {
	case Hydrogen:
		levels = append(levels, "xhigh")
	case Argon, Carbon:
		maxOutput = maxSyncFrontierOutput
	case FlashLite:
		levels = append([]string{"minimal"}, levels...)
	}
	for key := range options {
		switch fieldName(key) {
		case "model", "contents", "systeminstruction", "tools", "continuationtoken":
			return 0, fmt.Errorf("options.%s 由 Gemini 接入设置", key)
		case "cachedcontent":
			return 0, fmt.Errorf("当前 Gemini 接入只记录隐式缓存，未管理显式缓存和存储费用，不能设置 options.%s", key)
		case "generationconfig", "toolconfig":
			// 同一个字段有多种 JSON 拼法时，坚持使用原生 REST 名称，防止
			// YAML 同时带两个值而绕过本地预算与工具参数检查。
			if key != "generationConfig" && key != "toolConfig" {
				return 0, fmt.Errorf("options.%s 请使用 Gemini REST 的驼峰字段名", key)
			}
			if _, ok := options[key].(map[string]any); !ok {
				return 0, fmt.Errorf("options.%s 必须是对象", key)
			}
		}
	}
	limit := maxOutput // 省略参数采用供应商默认上限，用于本地容量检查和费用预留。
	config, _ := options["generationConfig"].(map[string]any)
	for key, value := range config {
		switch fieldName(key) {
		case "maxoutputtokens":
			n, ok := value.(int)
			if key != "maxOutputTokens" || !ok || n <= 0 || n > maxOutput {
				return 0, fmt.Errorf("%s 的 options.generationConfig.maxOutputTokens 必须为 1 到 %d 的整数", model, maxOutput)
			}
			limit = n
		case "responseformat", "responsemimetype", "responseschema", "responsejsonschema":
			return 0, fmt.Errorf("options.generationConfig.%s 由应用的输出要求设置", key)
		case "temperature", "topk", "topp":
			return 0, fmt.Errorf("%s 已弃用采样参数 options.generationConfig.%s", model, key)
		case "candidatecount":
			return 0, fmt.Errorf("本地行动只处理一条回复，不能设置 options.generationConfig.%s", key)
		case "thinkingconfig":
			thinking, ok := value.(map[string]any)
			if key != "thinkingConfig" || !ok {
				return 0, fmt.Errorf("options.generationConfig.thinkingConfig 必须是对象")
			}
			for name, level := range thinking {
				switch fieldName(name) {
				case "thinkingbudget":
					return 0, fmt.Errorf("%s 使用 thinkingConfig.thinkingLevel 调节推理强度", model)
				case "thinkinglevel":
					level, ok := level.(string)
					if name != "thinkingLevel" || !ok || !slices.Contains(levels, strings.ToLower(level)) {
						return 0, fmt.Errorf("%s 的 thinkingConfig.thinkingLevel 可用 %s", model, strings.Join(levels, "、"))
					}
				}
			}
		}
	}
	if _, err := json.Marshal(options); err != nil {
		return 0, fmt.Errorf("options 必须能编码为 JSON")
	}
	return limit, nil
}

func fieldName(key string) string { return strings.ToLower(strings.ReplaceAll(key, "_", "")) }
