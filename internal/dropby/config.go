package dropby

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MidTurnSteering                                                bool   // GPT-6 Responses WebSocket；仅用于持续成员行动。
	ChatModel                                                      string // 普通交流的行动模型；不另设参与分类节点。
	DeepMaxOutputTokens, ContextTokenBudget                        int
	Reasoning, DeepReasoning                                       string
	Address, DataDir, Model, BaseURL, APIKey, SearchURL, SearchKey string
	ExploreInterval                                                time.Duration
	MaxOutputTokens                                                int
	WeeklyBudgetMicros                                             int64
	TemporaryBudgetMicros                                          int64
	TemporaryBudgetUntil                                           time.Time
	ToolCostMicros                                                 int64
}

// 密钥只从环境读取，既不持久化到数据库，也不出现在配置响应或日志中。
func LoadConfig() (Config, error) {
	c := Config{
		MidTurnSteering: true,
		Address:         env("DROPBY_ADDR", "127.0.0.1:8090"), DataDir: env("DROPBY_DATA_DIR", "data"),
		Model: env("DROPBY_MODEL", "gpt-6.1-sol"), BaseURL: env("DROPBY_BASE_URL", "https://api.openai.com/v1"),
		// 聊天与个人探索使用相应行动模型，没有单独的参与分类模型。
		ChatModel: env("DROPBY_CHAT_MODEL", "gpt-6.1-terra"),
		APIKey:    env("DROPBY_API_KEY", os.Getenv("OPENAI_API_KEY")),
		SearchURL: env("DROPBY_SEARCH_URL", "https://api.deepseek.com/v1/tools"), SearchKey: os.Getenv("DEEPSEEK_API_KEY"),
		ContextTokenBudget: 100_000,
		ExploreInterval:    30 * time.Minute, MaxOutputTokens: 8000, DeepMaxOutputTokens: 25000, Reasoning: env("DROPBY_REASONING", "low"), DeepReasoning: env("DROPBY_DEEP_REASONING", "high"),
		WeeklyBudgetMicros: 20_000_000,
		ToolCostMicros:     750,
	}
	if value := os.Getenv("DROPBY_MID_TURN_STEERING"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return c, fmt.Errorf("DROPBY_MID_TURN_STEERING 必须为 true 或 false")
		}
		c.MidTurnSteering = enabled
	}
	for _, model := range []string{c.Model, c.ChatModel} {
		if _, err := priceForModel(model); err != nil {
			return c, err
		}
	}
	// 日额度不能直接改名解释成周额度，否则原来的 3 美元会变成整周只有 3 美元。
	// 允许部署环境暂留旧变量，但必须显式提供对应的新值，才能确定迁移意图。
	for old, current := range map[string]string{
		"DROPBY_DAILY_BUDGET_USD": "DROPBY_WEEKLY_BUDGET_USD",
	} {
		if os.Getenv(old) != "" && os.Getenv(current) == "" {
			return c, fmt.Errorf("%s 是旧的每日额度配置；请移除以使用周默认值，或显式设置 %s", old, current)
		}
	}
	if v := os.Getenv("DROPBY_SEARCH_KEY"); v != "" {
		c.SearchKey = v
	}
	ints := map[string]*int{"DROPBY_CONTEXT_TOKEN_BUDGET": &c.ContextTokenBudget, "DROPBY_DEEP_MAX_OUTPUT_TOKENS": &c.DeepMaxOutputTokens, "DROPBY_MAX_OUTPUT_TOKENS": &c.MaxOutputTokens}
	for key, p := range ints {
		if v := os.Getenv(key); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 1_000_000 {
				return c, fmt.Errorf("%s 必须是合理的正整数", key)
			}
			*p = n
		}
	}

	if v := os.Getenv("DROPBY_WEEKLY_BUDGET_USD"); v != "" {
		n, e := strconv.ParseFloat(v, 64)
		if e != nil || n <= 0 || n > 1000 || math.IsNaN(n) {
			return c, fmt.Errorf("DROPBY_WEEKLY_BUDGET_USD 必须在 0 到 1000 之间")
		}
		c.WeeklyBudgetMicros = int64(n * 1e6)
	}
	// 临时开发额度必须带明确截止时间；运行中到期即回到通常周额度，无需重启。
	if v := os.Getenv("DROPBY_TEMPORARY_BUDGET_USD"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		until, dateErr := time.Parse(time.RFC3339, os.Getenv("DROPBY_TEMPORARY_BUDGET_UNTIL"))
		if err != nil || dateErr != nil || math.IsNaN(n) || n <= 0 || n > 1000 {
			return c, fmt.Errorf("临时预算需要有效金额与带时区的截止时间")
		}
		c.TemporaryBudgetMicros, c.TemporaryBudgetUntil = int64(n*1e6), until
	}
	if v := os.Getenv("DROPBY_EXPLORE_INTERVAL"); v != "" {
		d, e := time.ParseDuration(v)
		if e != nil || d < time.Minute {
			return c, fmt.Errorf("DROPBY_EXPLORE_INTERVAL 至少为 1m")
		}
		c.ExploreInterval = d
	}
	// 首版只有一名本机用户。没有登录体系时绝不默认暴露到局域网或公网。
	host, _, e := net.SplitHostPort(c.Address)
	if e != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		return c, fmt.Errorf("DROPBY_ADDR 必须绑定本机回环地址")
	}
	for _, raw := range []string{c.BaseURL, c.SearchURL} {
		u, e := url.Parse(raw)
		if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return c, fmt.Errorf("服务地址必须为有效的 HTTP(S) URL")
		}
		if u.Scheme == "http" && u.Hostname() != "localhost" && (net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback()) {
			return c, fmt.Errorf("远程模型服务必须使用 HTTPS")
		}
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	c.SearchURL = strings.TrimRight(c.SearchURL, "/")
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// weeklyBudgetAt 是用户还没有保存自己的每周档位时的上限。
// 临时调额只在到期前生效，之后回到配置里的每周金额；两者都不改写账本。
func (c Config) weeklyBudgetAt(at time.Time) int64 {
	if c.TemporaryBudgetMicros > 0 && at.Before(c.TemporaryBudgetUntil) {
		return c.TemporaryBudgetMicros
	}
	return c.WeeklyBudgetMicros
}
