package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExampleModelConfigPreservesSceneDefaults(t *testing.T) {
	// 示例是迁移入口。直接从文件解析，防止示例与启动校验或当前默认行为脱节。
	file, err := os.Open("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cfg, err := decodeConfig(file, func(key string) string {
		if key == "OPENAI_API_KEY" {
			return "test-only"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		scene  modelScene
		model  string
		effort string
		output int
	}{
		{sceneChat, "gpt-6.1-terra", "none", 8000},
		{sceneResearch, "gpt-6.1-sol", "high", 25000},
		{sceneExploration, "gpt-6.1-sol", "high", 25000},
		{sceneMemberSettings, "gpt-6.1-sol", "low", 8000},
	} {
		route := cfg.Models.route(tc.scene)
		if route.Provider != "OpenAI" || route.Protocol != responsesProtocol || route.apiKey != "test-only" ||
			!route.steeringEnabled() || !route.cacheEnabled() || route.Model != tc.model ||
			route.reasoningEffort() != tc.effort || route.maxOutputTokens != tc.output || route.ContextTokenBudget != 100000 {
			t.Errorf("场景 %s 没有沿用现有配置", tc.scene)
		}
	}
}

func TestLoadConfigRoutesScenesToIndependentResponsesConnections(t *testing.T) {
	// 同一个 model ID 配到两个接入点。并发请求用独立密钥、推理参数及原生扩展，
	// 最终以相同的模型费率结算，并在账本记下实际供应商。
	s, _, _ := fixture(t)
	endpoint := func(key, effort string, output float64) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+key {
				t.Error("请求未选择当前场景的接入点或凭据")
			}
			var body struct {
				Model     string            `json:"model"`
				Reasoning map[string]any    `json:"reasoning"`
				MaxOutput float64           `json:"max_output_tokens"`
				Text      map[string]any    `json:"text"`
				Metadata  map[string]string `json:"metadata"`
				Include   []string          `json:"include"`
				Tools     []json.RawMessage `json:"tools"`
				Input     []json.RawMessage `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			format, _ := body.Text["format"].(map[string]any)
			if body.Model != "gpt-6.1-terra" || body.Reasoning["effort"] != effort ||
				body.MaxOutput != output || body.Metadata["marker"] != effort ||
				body.Text["verbosity"] != "low" || format["type"] != "json_object" ||
				len(body.Tools) != 1 || len(body.Input) != 1 ||
				!slices.Equal(body.Include, []string{"message.output_text.logprobs", "reasoning.encrypted_content"}) {
				t.Errorf("原生参数和业务请求未正确合并: %+v", body)
			}
			io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}],"usage":{"input_tokens":100,"output_tokens":10}}`)
		}))
		t.Cleanup(server.Close)
		return server
	}
	direct := endpoint("test-direct", "low", 400)
	relay := endpoint("test-relay", "high", 600)
	file := filepath.Join(t.TempDir(), "config.yaml")
	config := fmt.Sprintf(`
models:
  providers:
    direct: {protocol: openai_responses, base_url: %s/v1/, api_key_env: TEST_DIRECT_KEY, api_key: test-direct}
    relay: {protocol: openai_responses, base_url: %s/v1, api_key_env: TEST_RELAY_KEY}
  scenes:
    chat:
      provider: direct
      model: gpt-6.1-terra
      options: {reasoning: {effort: low}, max_output_tokens: 400, text: {verbosity: low}, metadata: {marker: low}, include: [message.output_text.logprobs]}
    research:
      provider: relay
      model: gpt-6.1-terra
      options: {reasoning: {effort: high}, max_output_tokens: 600, text: {verbosity: low}, metadata: {marker: high}, include: [message.output_text.logprobs]}
    personal_exploration: {provider: relay, model: gpt-6.1-sol}
    member_settings: {provider: direct, model: gpt-6.1-sol}`, direct.URL, relay.URL)
	if err := os.WriteFile(file, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROPBY_CONFIG", file)
	t.Setenv("TEST_DIRECT_KEY", "should-not-be-used")
	t.Setenv("TEST_RELAY_KEY", "test-relay")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	model := &HTTPModel{Config: cfg, Store: s, Client: direct.Client()}
	defs := []ToolDefinition{{Type: "function", Function: ToolFunction{Name: "read_history", Parameters: map[string]string{"type": "object"}}}}
	var calls sync.WaitGroup
	for _, scene := range []modelScene{sceneChat, sceneResearch} {
		calls.Go(func() {
			ctx := context.WithValue(t.Context(), modelSceneKey{}, scene)
			if reply, err := model.Complete(ctx, []ChatMessage{{Role: "user", Content: "返回 JSON"}}, defs, true); err != nil || reply.Content != "{}" {
				t.Errorf("场景 %s 请求失败: %v", scene, err)
			}
		})
	}
	calls.Wait()
	start, end := budgetWeek(time.Now())
	report, err := s.readUsageReport(t.Context(), start, end, time.Now())
	if err != nil || len(report.Entries) != 2 {
		t.Fatalf("两次请求应分别结算: %v %+v", err, report.Entries)
	}
	seen := map[string]bool{}
	for _, entry := range report.Entries {
		if entry.Model != "gpt-6.1-terra" || entry.CostMicros != 150 {
			t.Errorf("费用未按实际模型官方费率结算: %+v", entry)
		}
		seen[entry.Provider] = true
	}
	if len(seen) != 2 || !seen["direct"] || !seen["relay"] {
		t.Fatalf("账本未记下实际接入点: %v", seen)
	}
}

func TestModelConfigRejectsInvalidRoutes(t *testing.T) {
	// 拼写错误、无效预算和争抢协议字段在启动时报告，避免运行到工具续接才出错。
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ from, to, want string }{
		{"protocol: openai_responses", "protocol: unsupported", "providers.OpenAI.protocol"},
		{"provider: OpenAI", "provider: missing", "scenes.chat.provider"},
		{"gpt-6.1-terra", "unknown-model", "scenes.chat：model"},
		{"max_output_tokens: 8000", "max_output_tokens: 0", "options.max_output_tokens"},
		{"max_output_tokens: 8000", "max_output_tokens: 100000", "context_token_budget"},
		{"max_output_tokens: 8000", "input: forged", "options.input"},
		{"  chat:", "  misspelled_chat:", "models.scenes 包含未识别"},
	} {
		config := strings.Replace(string(raw), tc.from, tc.to, 1)
		if _, err := decodeConfig(strings.NewReader(config), os.Getenv); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("应指出 %s，实际 %v", tc.want, err)
		}
	}
}

func TestMissingProviderKeyKeepsOnlyItsOwnWorkQueued(t *testing.T) {
	// 聊天连接不可用时，其工作留在队列。不让积压的聊天占满原先
	// 的调度批次，否则可用的研究连接永远拿不到本轮工作。
	s, room, member := fixture(t)
	nowMillis := time.Now().UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 101 {
		err := insertWork(tx, Work{ID: fmt.Sprintf("blocked_%d", i), ConversationID: room.ID, Kind: "chat", Status: "pending", DueAt: nowMillis, CreatedAt: now()})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := insertWork(tx, Work{ID: "research_ready", ConversationID: room.ID, MemberID: member.ID, Kind: "research", Status: "pending", DueAt: nowMillis, CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ready, err := s.dueWorks(false, true, false)
	if err != nil || len(ready) != 1 || ready[0].ID != "research_ready" {
		t.Fatalf("可调度的研究被聊天积压挡住: %v %+v", err, ready)
	}
	var pending int
	if err := s.db.QueryRow("SELECT count(*) FROM work WHERE kind='chat' AND status='pending'").Scan(&pending); err != nil || pending != 101 {
		t.Fatalf("缺密钥场景的工作应保留：%d %v", pending, err)
	}
}

func TestConfigFileWinsAndOnlyMissingValuesUseEnvironment(t *testing.T) {
	// 文件中的值即使刻意为空或 false，也覆盖环境。只给省略的字段补值。
	env := map[string]string{
		"DROPBY_ADDR": "invalid-address", "DROPBY_DATA_DIR": "env-data",
		"DROPBY_WEEKLY_BUDGET_USD": "invalid-budget", "DROPBY_SEARCH_KEY": "wrong-key",
		"OPENAI_API_KEY": "legacy-openai-key", "DROPBY_BASE_URL": "https://legacy.example",
		"DROPBY_CHAT_MODEL": "gpt-6-luna", "DROPBY_MAX_OUTPUT_TOKENS": "500",
		"DROPBY_MID_TURN_STEERING": "true",
	}
	file := `
server: {address: '127.0.0.1:8091'}
budget: {weekly_usd: 7}
search: {api_key: ""}
models:
  providers:
    OpenAI: {protocol: openai_responses, base_url: 'https://file.example/v1', mid_turn_steering: false}
  scenes:
    chat: {provider: OpenAI, model: gpt-6.1-terra}
`
	cfg, err := decodeConfig(strings.NewReader(file), func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	chat := cfg.Models.route(sceneChat)
	if cfg.Address != "127.0.0.1:8091" || cfg.DataDir != "env-data" || cfg.WeeklyBudgetMicros != 7_000_000 || cfg.SearchKey != "" {
		t.Fatal("应用配置没有按文件、环境、默认值的顺序加载")
	}
	if chat.BaseURL != "https://file.example/v1" || chat.Model != "gpt-6.1-terra" ||
		chat.apiKey != "legacy-openai-key" || chat.steeringEnabled() || chat.maxOutputTokens != 500 {
		t.Fatal("接入点和场景的显式配置没有优先，或遗漏环境回退")
	}
}
