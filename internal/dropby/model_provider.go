package dropby

import (
	"fmt"
	"strconv"

	"drop_by/internal/dropby/model/openai"
)

// 协议选择集中在这一处。应用的场景、工具权限和工作恢复都不需要供应商分支。
func (m *HTTPModel) newModelCall(route modelRoute) (*modelCall, error) {
	switch route.Protocol {
	case openai.Protocol:
		price, err := openai.PriceForModel(route.Model)
		if err != nil {
			return nil, err
		}
		backend := &openai.Client{
			HTTP: serviceClient(m.Client), Provider: route.Provider, BaseURL: route.BaseURL,
			APIKey: route.apiKey, Model: route.Model,
			Options: route.Options, MaxOutputTokens: route.maxOutputTokens, Price: price,
			CacheBreakpoints: route.cacheEnabled(), AllowLegacyHistory: route.Provider == defaultModelProvider,
		}
		return &modelCall{HTTPModel: m, modelRoute: route, backend: backend, reserveMicros: price.ReserveMicros}, nil
	default:
		return nil, fmt.Errorf("未接入模型协议 %q", route.Protocol)
	}
}

func validateProviderProtocol(protocol string) error {
	switch protocol {
	case openai.Protocol:
		return nil
	default:
		return fmt.Errorf("当前支持 %s", openai.Protocol)
	}
}

func resolveProviderScene(s *modelSettings, scene modelScene, protocol string, legacyFallback bool, getenv func(string) string) error {
	switch protocol {
	case openai.Protocol:
		return resolveOpenAIScene(s, scene, legacyFallback, getenv)
	default:
		return validateProviderProtocol(protocol)
	}
}

func resolveOpenAIScene(s *modelSettings, scene modelScene, legacyFallback bool, getenv func(string) string) error {
	// 老环境参数只补原 OpenAI 接入点的空缺。自定义接入点很可能支持不同参数。
	modelName, outputEnv, effortEnv, effort, output := "gpt-6.1-sol", "DROPBY_MAX_OUTPUT_TOKENS", "DROPBY_REASONING", "low", 8000
	modelEnv := "DROPBY_MODEL"
	switch scene {
	case sceneChat:
		modelName, modelEnv, effortEnv, effort = "gpt-6.1-terra", "DROPBY_CHAT_MODEL", "", "none"
	case sceneResearch, sceneExploration:
		outputEnv, effortEnv, effort, output = "DROPBY_DEEP_MAX_OUTPUT_TOKENS", "DROPBY_DEEP_REASONING", "high", 25000
	}
	if s.Model == "" && legacyFallback {
		s.Model = firstValue(getenv(modelEnv), modelName)
	}
	if _, err := openai.PriceForModel(s.Model); err != nil {
		return fmt.Errorf("model：%w", err)
	}
	if s.Options == nil {
		s.Options = make(map[string]any)
	}
	if legacyFallback {
		if _, exists := s.Options["max_output_tokens"]; !exists {
			if raw := getenv(outputEnv); raw != "" {
				n, err := strconv.Atoi(raw)
				if err != nil || n <= 0 {
					return fmt.Errorf("%s 必须为正整数", outputEnv)
				}
				output = n
			}
			s.Options["max_output_tokens"] = output
		}
		if _, exists := s.Options["reasoning"]; !exists {
			effort = firstValue(getenv(effortEnv), effort)
			if effort == "none" && s.Model == "gpt-6.1-sol" {
				effort = "low"
			}
			s.Options["reasoning"] = map[string]any{"effort": effort}
		}
	}
	var err error
	s.maxOutputTokens, err = openai.ValidateOptions(s.Options)
	if err != nil {
		return err
	}
	if s.ContextTokenBudget == 0 {
		s.ContextTokenBudget = 100_000
		if raw := getenv("DROPBY_CONTEXT_TOKEN_BUDGET"); legacyFallback && raw != "" {
			s.ContextTokenBudget, err = strconv.Atoi(raw)
			if err != nil {
				return fmt.Errorf("DROPBY_CONTEXT_TOKEN_BUDGET 必须为整数")
			}
		}
	}
	if s.ContextTokenBudget > 1_000_000 || s.ContextTokenBudget <= s.maxOutputTokens {
		return fmt.Errorf("context_token_budget 必须大于 options.max_output_tokens，且不超过 1000000")
	}
	return nil
}
