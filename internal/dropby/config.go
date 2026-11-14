package dropby

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Models                modelConfig
	Address, DataDir      string
	SearchURL, SearchKey  string
	ExploreInterval       time.Duration
	WeeklyBudgetMicros    int64
	TemporaryBudgetMicros int64
	TemporaryBudgetUntil  time.Time
	ToolCostMicros        int64
}

type configFile struct {
	Models modelConfig `yaml:"models"`
	Server struct {
		Address *string `yaml:"address"`
		DataDir *string `yaml:"data_dir"`
	} `yaml:"server"`
	Search struct {
		BaseURL *string `yaml:"base_url"`
		APIKey  *string `yaml:"api_key"`
	} `yaml:"search"`
	Budget struct {
		WeeklyUSD      *float64 `yaml:"weekly_usd"`
		TemporaryUSD   *float64 `yaml:"temporary_usd"`
		TemporaryUntil *string  `yaml:"temporary_until"`
	} `yaml:"budget"`
	ExploreInterval *string `yaml:"explore_interval"`
}

func LoadConfig() (Config, error) {
	path := os.Getenv("DROPBY_CONFIG")
	file, err := os.Open(firstValue(path, "config.yaml"))
	if errors.Is(err, os.ErrNotExist) && path == "" {
		return configFile{}.resolve(os.Getenv)
	}
	if err != nil {
		return Config{}, fmt.Errorf("打开应用配置：%w", err)
	}
	defer file.Close()
	cfg, err := decodeConfig(file, os.Getenv)
	if err != nil {
		return Config{}, fmt.Errorf("应用配置 %s：%w", file.Name(), err)
	}
	return cfg, nil
}

func decodeConfig(file io.Reader, getenv func(string) string) (Config, error) {
	var raw configFile
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		// YAML 的类型错误可能回显原始值，配置允许密钥，日志只显示行/字段校验提示。
		return Config{}, fmt.Errorf("需要有效的 YAML，请检查重复字段、字段名和类型")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, fmt.Errorf("只接受一份 YAML 文档")
	}
	return raw.resolve(getenv)
}

func (raw configFile) resolve(getenv func(string) string) (Config, error) {
	c := Config{
		Models:         raw.Models,
		Address:        fileValue(raw.Server.Address, getenv("DROPBY_ADDR"), "127.0.0.1:8090"),
		DataDir:        fileValue(raw.Server.DataDir, getenv("DROPBY_DATA_DIR"), "data"),
		SearchURL:      fileValue(raw.Search.BaseURL, getenv("DROPBY_SEARCH_URL"), "https://api.deepseek.com/v1/tools"),
		SearchKey:      fileValue(raw.Search.APIKey, getenv("DROPBY_SEARCH_KEY"), getenv("DEEPSEEK_API_KEY")),
		ToolCostMicros: 750,
	}
	// 已经配置周额度时，旧的日额度不会影响启动。只有旧配置时告知用户迁移。
	if raw.Budget.WeeklyUSD == nil && getenv("DROPBY_DAILY_BUDGET_USD") != "" && getenv("DROPBY_WEEKLY_BUDGET_USD") == "" {
		return c, fmt.Errorf("DROPBY_DAILY_BUDGET_USD 是旧的每日额度配置，请设置 budget.weekly_usd 或 DROPBY_WEEKLY_BUDGET_USD")
	}
	var err error
	c.WeeklyBudgetMicros, err = budgetMicros(raw.Budget.WeeklyUSD, firstValue(getenv("DROPBY_WEEKLY_BUDGET_USD"), "20"))
	if err != nil {
		return c, fmt.Errorf("budget.weekly_usd / DROPBY_WEEKLY_BUDGET_USD 必须在 0 到 1000 之间")
	}
	// 临时额度带截止时间。过期后自动回到正常周额度。
	if v := getenv("DROPBY_TEMPORARY_BUDGET_USD"); raw.Budget.TemporaryUSD != nil || v != "" {
		amount, amountErr := budgetMicros(raw.Budget.TemporaryUSD, v)
		until, dateErr := time.Parse(time.RFC3339, fileValue(raw.Budget.TemporaryUntil, getenv("DROPBY_TEMPORARY_BUDGET_UNTIL")))
		if amountErr != nil || dateErr != nil {
			return c, fmt.Errorf("临时预算需要有效金额与带时区的截止时间")
		}
		c.TemporaryBudgetMicros, c.TemporaryBudgetUntil = amount, until
	}
	c.ExploreInterval, err = time.ParseDuration(fileValue(raw.ExploreInterval, getenv("DROPBY_EXPLORE_INTERVAL"), "30m"))
	if err != nil || c.ExploreInterval < time.Minute {
		return c, fmt.Errorf("explore_interval / DROPBY_EXPLORE_INTERVAL 至少为 1m")
	}
	// 本机单用户服务只绑定回环地址。
	host, _, err := net.SplitHostPort(c.Address)
	if err != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
		return c, fmt.Errorf("server.address / DROPBY_ADDR 必须绑定本机回环地址")
	}
	c.SearchURL, err = normalizeServiceURL(c.SearchURL)
	if err != nil {
		return c, fmt.Errorf("搜索服务地址：%w", err)
	}
	err = c.Models.resolve(getenv)
	return c, err
}

func budgetMicros(configured *float64, fallback string) (int64, error) {
	var n float64
	var err error
	if configured != nil {
		n = *configured
	} else {
		n, err = strconv.ParseFloat(fallback, 64)
	}
	if err != nil || n <= 0 || n > 1000 || math.IsNaN(n) {
		return 0, fmt.Errorf("额度须在 0 到 1000 之间")
	}
	return int64(n * 1e6), nil
}

func normalizeServiceURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("服务地址必须为有效的 HTTP(S) API 根地址")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && (net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback()) {
		return "", fmt.Errorf("远程服务必须使用 HTTPS")
	}
	return strings.TrimRight(raw, "/"), nil
}

func firstValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func fileValue(value *string, fallback ...string) string {
	if value != nil {
		return *value
	}
	return firstValue(fallback...)
}

// weeklyBudgetAt 是用户没有保存自己的周额度时使用的金额。
func (c Config) weeklyBudgetAt(at time.Time) int64 {
	if c.TemporaryBudgetMicros > 0 && at.Before(c.TemporaryBudgetUntil) {
		return c.TemporaryBudgetMicros
	}
	return c.WeeklyBudgetMicros
}
