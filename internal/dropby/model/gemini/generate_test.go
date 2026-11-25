package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestIncompleteFinishKeepsProviderReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"role":"model","parts":[{"text":"半截"}]}}]}`)
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), BaseURL: server.URL, APIKey: "k", Model: "gemini-test"}
	result, err := client.Generate(context.Background(), []byte(`{}`))
	if err != nil || !result.Incomplete || result.IncompleteReason != "MAX_TOKENS" {
		t.Fatalf("未保留截断原因: %+v %v", result, err)
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

func TestMixedCallIDsSurviveCheckpointAndOutOfOrderReceipts(t *testing.T) {
	// 混合两种 ID 来源且重复调用完全相同的工具，不能按名称或参数配对。
	const turn = `{"role":"model","parts":[{"functionCall":{"name":"search","args":{"query":"q"}},"thoughtSignature":"c2ln"},{"functionCall":{"id":"call_native","name":"search","args":{"query":"q"}}},{"functionCall":{"name":"search","args":{"query":"q"}}}]}`
	client := Client{}
	msg, err := client.decodeContent(json.RawMessage(turn), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 3 {
		t.Fatalf("工具数：%d", len(msg.ToolCalls))
	}
	next, err := client.decodeContent(json.RawMessage(turn), 100)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ToolCalls[0].ID == next.ToolCalls[0].ID {
		t.Fatal("新一轮相同搜索复用了旧执行身份")
	}

	// 和持久化断点一样序列化再读回。故意倒序交回三个结果，检查最终请求的原生顺序。
	checkpoint, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var restored model.ChatMessage
	if err := json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	history := []model.ChatMessage{restored}
	for i := len(restored.ToolCalls) - 1; i >= 0; i-- {
		history = append(history, model.ChatMessage{Role: "tool", ToolCallID: restored.ToolCalls[i].ID, Content: fmt.Sprintf(`{"index":%d}`, i)})
	}
	raw, err := client.Build(history, -1, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sent struct{ Contents []json.RawMessage }
	if err := json.Unmarshal(raw, &sent); err != nil || len(sent.Contents) != 2 {
		t.Fatalf("工具往返：%s %v", raw, err)
	}
	const receipts = `{"role":"user","parts":[{"functionResponse":{"name":"search","response":{"index":0}}},{"functionResponse":{"id":"call_native","name":"search","response":{"index":1}}},{"functionResponse":{"name":"search","response":{"index":2}}}]}`
	if string(sent.Contents[0]) != turn || string(sent.Contents[1]) != receipts {
		t.Fatalf("原生请求或回执配对错误：%s", raw)
	}
}
