package dropby

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"drop_by/internal/dropby/model/openai"
)

type modelScene string

const (
	sceneChat           modelScene = "chat"
	sceneResearch       modelScene = "research"
	sceneExploration    modelScene = "personal_exploration"
	sceneMemberSettings modelScene = "member_settings"

	responsesProtocol    = openai.Protocol
	defaultModelProvider = "OpenAI"
)

var modelScenes = [...]modelScene{sceneChat, sceneResearch, sceneExploration, sceneMemberSettings}

type modelSceneKey struct{}

func sceneFromContext(ctx context.Context) modelScene {
	if scene, ok := ctx.Value(modelSceneKey{}).(modelScene); ok {
		return scene
	}
	// 独立的 JSON 调用服务于人物创建、情景问答与偏好理解。
	return sceneMemberSettings
}

func actionScene(personal bool, kind string) modelScene {
	if kind == "research" {
		return sceneResearch
	}
	if personal {
		return sceneExploration
	}
	return sceneChat
}

type modelConfig struct {
	Providers map[string]modelProvider     `yaml:"providers"`
	Scenes    map[modelScene]modelSettings `yaml:"scenes"`
}

type modelProvider struct {
	Protocol         string  `yaml:"protocol"`
	BaseURL          string  `yaml:"base_url"`
	APIKey           *string `yaml:"api_key"`
	APIKeyEnv        string  `yaml:"api_key_env"`
	TokenCounting    string  `yaml:"token_counting"`
	MidTurnSteering  *bool   `yaml:"mid_turn_steering"`
	CacheBreakpoints *bool   `yaml:"cache_breakpoints"`
	apiKey           string
}

type modelSettings struct {
	Provider           string         `yaml:"provider"`
	Model              string         `yaml:"model"`
	ContextTokenBudget int            `yaml:"context_token_budget"`
	Options            map[string]any `yaml:"options"`
	maxOutputTokens    int
}

// 传输只接收已选定的场景和供应商。费率仍按实际模型查官方费率表。
type modelRoute struct {
	modelSettings
	modelProvider
}

func (cfg modelConfig) route(scene modelScene) modelRoute {
	settings := cfg.Scenes[scene]
	return modelRoute{settings, cfg.Providers[settings.Provider]}
}

func (c Config) ChatModelName() string { return c.Models.Scenes[sceneChat].Model }
func (c Config) ModelReady() bool      { return c.Models.ready(sceneChat) }

func (cfg modelConfig) ready(scene modelScene) bool { return cfg.route(scene).apiKey != "" }

func (p modelProvider) steeringEnabled() bool { return p.MidTurnSteering != nil && *p.MidTurnSteering }
func (p modelProvider) cacheEnabled() bool    { return p.CacheBreakpoints != nil && *p.CacheBreakpoints }

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (cfg *modelConfig) resolve(getenv func(string) string) error {
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]modelProvider)
	}
	if cfg.Scenes == nil {
		cfg.Scenes = make(map[modelScene]modelSettings)
	}
	// 只有场景没有指定接入点时，才使用原有 OpenAI 环境配置。
	for _, scene := range modelScenes {
		if cfg.Scenes[scene].Provider == "" {
			if _, ok := cfg.Providers[defaultModelProvider]; !ok {
				enabled := true
				cfg.Providers[defaultModelProvider] = modelProvider{Protocol: responsesProtocol, CacheBreakpoints: &enabled}
			}
			break
		}
	}
	for name, provider := range cfg.Providers {
		if name == "" {
			return fmt.Errorf("models.providers 的名称不能为空")
		}
		if err := validateProviderProtocol(provider.Protocol); err != nil {
			return fmt.Errorf("models.providers.%s.protocol：%w", name, err)
		}
		switch provider.TokenCounting {
		case "", "remote":
		case "local":
			if provider.Protocol == responsesProtocol {
				return fmt.Errorf("models.providers.%s.token_counting：Responses 的长上下文处理需要远程计数", name)
			}
		default:
			return fmt.Errorf("models.providers.%s.token_counting 只能设为 local 或 remote", name)
		}
		// 旧环境变量只补默认 Responses 接入点。名称相同的其他协议
		// 也不能错用旧地址、密钥或 OpenAI 专用能力。
		legacy := name == defaultModelProvider && provider.Protocol == responsesProtocol
		if legacy && provider.BaseURL == "" {
			provider.BaseURL = firstValue(getenv("DROPBY_BASE_URL"), "https://api.openai.com/v1")
		}
		url, err := normalizeServiceURL(provider.BaseURL)
		if err != nil {
			return fmt.Errorf("models.providers.%s.base_url：%w", name, err)
		}
		if provider.APIKeyEnv != "" && !envName.MatchString(provider.APIKeyEnv) {
			return fmt.Errorf("models.providers.%s.api_key_env 需要填写环境变量名", name)
		}
		switch {
		case provider.APIKey != nil:
			provider.apiKey = *provider.APIKey
		case provider.APIKeyEnv != "":
			provider.apiKey = getenv(provider.APIKeyEnv)
		case legacy:
			provider.apiKey = firstValue(getenv("DROPBY_API_KEY"), getenv("OPENAI_API_KEY"))
		}
		provider.APIKey = nil // 调用只保留实际使用的密钥，不把文件中的原始值再传给业务。
		if legacy && provider.MidTurnSteering == nil {
			enabled, err := strconv.ParseBool(firstValue(getenv("DROPBY_MID_TURN_STEERING"), "true"))
			if err != nil {
				return fmt.Errorf("DROPBY_MID_TURN_STEERING 必须为 true 或 false")
			}
			provider.MidTurnSteering = &enabled
		}
		if provider.Protocol != responsesProtocol && (provider.steeringEnabled() || provider.cacheEnabled()) {
			return fmt.Errorf("models.providers.%s：mid_turn_steering 和 cache_breakpoints 属于 %s", name, responsesProtocol)
		}
		provider.BaseURL = url
		cfg.Providers[name] = provider
	}
	for _, scene := range modelScenes {
		settings := cfg.Scenes[scene]
		if settings.Provider == "" {
			settings.Provider = defaultModelProvider
		}
		provider, ok := cfg.Providers[settings.Provider]
		if !ok {
			return fmt.Errorf("models.scenes.%s.provider 需要引用已定义的供应商", scene)
		}
		if err := resolveProviderScene(&settings, scene, provider.Protocol, settings.Provider == defaultModelProvider, getenv); err != nil {
			return fmt.Errorf("models.scenes.%s：%w", scene, err)
		}
		cfg.Scenes[scene] = settings
	}
	if len(cfg.Scenes) != len(modelScenes) {
		return fmt.Errorf("models.scenes 包含未识别的场景")
	}
	return nil
}
