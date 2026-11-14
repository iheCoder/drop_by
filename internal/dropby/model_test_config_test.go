package dropby

import (
	"maps"
	"net/http"
)

// 测试直接调整已解析的配置，沿用实际的场景路由，避免在测试用的
// HTTP 请求里保留第二套旧环境变量行为。
func (c *Config) editScenes(scenes []modelScene, edit func(*modelSettings)) {
	c.Models.Scenes = maps.Clone(c.Models.Scenes)
	for _, scene := range scenes {
		settings := c.Models.Scenes[scene]
		settings.Options = maps.Clone(settings.Options)
		edit(&settings)
		c.Models.Scenes[scene] = settings
	}
}

func (c *Config) editProviders(edit func(*modelProvider)) {
	c.Models.Providers = maps.Clone(c.Models.Providers)
	for name, provider := range c.Models.Providers {
		edit(&provider)
		c.Models.Providers[name] = provider
	}
}

func (c *Config) setModelEndpoint(url string) {
	c.editProviders(func(p *modelProvider) { p.BaseURL = url })
}

func (c *Config) setSteering(enabled bool) {
	c.editProviders(func(p *modelProvider) { p.MidTurnSteering = &enabled })
}

func (c *Config) setModelLimit(ordinary, deep int) {
	c.editScenes([]modelScene{sceneChat, sceneMemberSettings}, func(s *modelSettings) { s.setOutputLimit(ordinary) })
	c.editScenes([]modelScene{sceneResearch, sceneExploration}, func(s *modelSettings) { s.setOutputLimit(deep) })
}

func (s *modelSettings) setOutputLimit(limit int) {
	s.Options["max_output_tokens"] = limit
	s.maxOutputTokens = limit
}

func (s *modelSettings) setReasoning(effort string) {
	s.Options["reasoning"] = map[string]any{"effort": effort}
}

func (s modelSettings) reasoningEffort() any {
	reasoning, _ := s.Options["reasoning"].(map[string]any)
	return reasoning["effort"]
}

func responsesForTest(c Config) *modelCall {
	m := &HTTPModel{Config: c, Client: &http.Client{}}
	call, err := m.newModelCall(c.Models.route(sceneMemberSettings))
	if err != nil {
		panic(err)
	}
	return call
}
