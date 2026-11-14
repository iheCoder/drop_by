package gemini

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"drop_by/internal/dropby/model"
)

func TestGeminiStandardRatesAcrossEffectiveDates(t *testing.T) {
	// 在公开调价的前一刻和生效日检查普通输入、缓存和输出三种价格。
	for _, tc := range []struct {
		model         string
		change        time.Time
		before, after Price
	}{
		{Argon, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), Price{2000, 100, 10000}, Price{4000, 200, 20000}},
		{Carbon, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), Price{2000, 100, 10000}, Price{4000, 200, 20000}},
		{Flash, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Price{750, 75, 3750}, Price{1500, 150, 7500}},
		{Hydrogen, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Price{1500, 75, 7500}, Price{1500, 75, 7500}},
		{FlashLite, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Price{300, 30, 2500}, Price{300, 30, 2500}},
	} {
		before, beforeErr := PriceForModel(tc.model, tc.change.Add(-time.Second))
		after, afterErr := PriceForModel(tc.model, tc.change)
		if beforeErr != nil || afterErr != nil || before != tc.before || after != tc.after {
			t.Errorf("%s 费率生效日期或数值不符：%+v / %+v", tc.model, before, after)
		}
	}
}

func TestGeminiRejectsThinkingLevelsFromOtherModels(t *testing.T) {
	// Hydrogen 专用 xhigh 和 Flash-Lite 专用 minimal 不能被其他型号继承。
	for name, level := range map[string]string{Argon: "xhigh", Carbon: "xhigh", Flash: "minimal", Hydrogen: "minimal", FlashLite: "xhigh"} {
		if _, err := ValidateOptions(name, map[string]any{"generationConfig": map[string]any{
			"thinkingConfig": map[string]any{"thinkingLevel": level},
		}}); err == nil {
			t.Errorf("%s 不应接受 %s", name, level)
		}
	}
}

func TestNonStreamingOutputLimitByModel(t *testing.T) {
	// 配置省略时预留实际的 REST 上限，超出单次上限则在启动时发现。
	for name, want := range map[string]int{Argon: 32768, Carbon: 32768, Hydrogen: 65536, Flash: 65536, FlashLite: 65536} {
		if got, err := ValidateOptions(name, nil); err != nil || got != want {
			t.Errorf("%s 默认上限 = %d，期望 %d：%v", name, got, want, err)
		}
		generation := map[string]any{"maxOutputTokens": want}
		options := map[string]any{"generationConfig": generation}
		if got, err := ValidateOptions(name, options); err != nil || got != want {
			t.Errorf("%s 显式上限 = %d，期望 %d：%v", name, got, want, err)
		}
		generation["maxOutputTokens"] = want + 1
		if _, err := ValidateOptions(name, options); err == nil {
			t.Errorf("%s 接受了超出非流式单次上限的值", name)
		}
	}
}

func TestGenerateContentReplaysParallelResultsAndForeignHistory(t *testing.T) {
	// 旧 OpenAI 断点含私有压缩状态与可见原话。新 Gemini 回合并行调用
	// 两次同名工具，两个同名回执仍须依原始 ID 放在同一个 user 回合。
	client := Client{Provider: "native", BaseURL: "https://example.invalid", Model: Hydrogen}
	native := json.RawMessage(`{"role":"model","parts":[{"functionCall":{"id":"alpha","name":"read","args":{"page":1}},"thoughtSignature":"c2ln"},{"functionCall":{"id":"beta","name":"read","args":{"page":2}}}]}`)
	reply, err := client.decodeContent(native, 100)
	if err != nil {
		t.Fatal(err)
	}
	foreign := model.ChatMessage{Role: "assistant", Content: "可见的上一轮回复", NativeSource: "other-endpoint", ResponseItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"private-thought"}`)}}
	call := model.ToolCall{ID: "old", Type: "function"}
	call.Function.Name, call.Function.Arguments = "read", `{"page":3}`
	foreign.ToolCalls = []model.ToolCall{call}
	history := []model.ChatMessage{
		{Role: "system", Content: "保持既有角色"},
		{Role: "user", ResponseItems: []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"不能丢弃的旧原话"}]}`),
			json.RawMessage(`{"type":"compaction","encrypted_content":"private-compression"}`),
		}},
		foreign,
		{Role: "tool", ToolCallID: "old", Content: "旧的工具回执"},
		{Role: "user", Content: "读前两页"},
		reply,
		{Role: "tool", ToolCallID: "alpha", Content: `{"result":{"body":"第一页"}}`},
		{Role: "tool", ToolCallID: "beta", Content: `[{"body":"第二页"}]`},
	}
	raw, err := client.Build(history, -1, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-") || !strings.Contains(string(raw), "不能丢弃的旧原话") ||
		!strings.Contains(string(raw), "可见的上一轮回复") || !strings.Contains(string(raw), "旧的工具回执") || !strings.Contains(string(raw), "先前的工具调用") {
		t.Fatalf("切换应只转交旧模型的可见原话和工具事实: %s", raw)
	}
	var sent struct{ Contents []json.RawMessage }
	if err := json.Unmarshal(raw, &sent); err != nil || len(sent.Contents) < 2 {
		t.Fatalf("无法读取实际历史: %v", err)
	}
	if string(sent.Contents[len(sent.Contents)-2]) != string(native) || string(sent.Contents[len(sent.Contents)-1]) !=
		`{"role":"user","parts":[{"functionResponse":{"id":"alpha","name":"read","response":{"result":{"body":"第一页"}}}},{"functionResponse":{"id":"beta","name":"read","response":{"output":[{"body":"第二页"}]}}}]}` {
		t.Fatalf("工具回执必须按两个独立 ID 紧跟完整原生回合: %s", raw)
	}
}
