package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebParametersFullResultsRefreshAndBilling(t *testing.T) {
	// 模拟供应商检查实际请求，而不是只检查工具声明；全程不调用付费服务。
	s, c, members := groupWithMembers(t, 1)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["refresh"]; ok {
			t.Error("local option leaked upstream")
		}
		if strings.HasSuffix(r.URL.Path, "fetch") {
			json.NewEncoder(w).Encode(map[string]any{"title": "long", "content": strings.Repeat("正文", 12000) + "尾部风险"})
			return
		}
		switch body["query"] {
		case "bad":
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"query invalid"}}`)
		case "broken":
			fmt.Fprint(w, `{}`)
		case "empty":
			fmt.Fprint(w, `{"results":[]}`)
		default:
			if body["count"] != float64(40) || body["need_content"] != true || body["ori_question"] != "原问题" || body["localization"] != "1.1.1.1" {
				t.Errorf("parameters lost: %v", body)
			}
			fmt.Fprint(w, `{"results":[{"url":"https://example.com","date":"2026-11-07","authority":3,"summary":"原摘要","content":"原正文"}]}`)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.SearchURL = server.URL
	box := Toolbox{Store: s, Config: cfg, Hub: NewHub()}
	m := members[0]
	work := addWork(t, s, c, m, "research")
	execute := func(name string, args any) string {
		t.Helper()
		result, err := box.Execute(context.Background(), c, m, work, toolCall(name, args))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	params := map[string]any{"query": "different view", "count": 40, "need_content": true, "ori_question": "原问题", "localization": "1.1.1.1"}
	first := execute("search", params)
	if !strings.Contains(first, "2026-11-07") || !strings.Contains(first, "原正文") {
		t.Fatal(first)
	}
	execute("search", params)
	if calls != 1 {
		t.Fatal("identical request not cached")
	}
	params["refresh"] = true
	execute("search", params)
	if calls != 2 {
		t.Fatal("refresh reused stale result")
	}
	// 无效响应不能进入缓存；有效空结果免费且可缓存；400 可交还模型修正。
	bad := execute("search", map[string]any{"query": "bad"})
	if !strings.Contains(bad, "query invalid") {
		t.Fatal(bad)
	}
	execute("search", map[string]any{"query": "empty"})
	execute("search", map[string]any{"query": "empty"})
	usage, _ := s.Usage()
	if usage.CostMicros != 2*cfg.ToolCostMicros || usage.UnsettledMicros != 0 {
		t.Fatalf("billing: %+v", usage)
	}
	before := calls
	execute("search", map[string]any{"query": "broken"})
	execute("search", map[string]any{"query": "broken"})
	if calls != before+2 {
		t.Fatal("malformed response cached")
	}
	page := execute("read_page", map[string]any{"url": "https://example.invalid/long"})
	if !strings.Contains(page, "尾部风险") {
		t.Fatal("page tail lost")
	}
	var result struct {
		ID string `json:"tool_run_id"`
	}
	json.Unmarshal([]byte(page), &result)
	tail := execute("read_tool_result", map[string]any{"id": result.ID, "query": "尾部风险", "length": 20000})
	if !strings.Contains(tail, "尾部风险") {
		t.Fatal("cannot locate original tail")
	}
	index := execute("read_tool_result", map[string]any{"query": "example.invalid", "limit": 100})
	if !strings.Contains(index, result.ID) {
		t.Fatal("saved source undiscoverable")
	}
}

func TestReadControlsAndLongWrites(t *testing.T) {
	// 显式读取量和写入量超过旧限制仍成功；旧版本能按版本号读取。
	s, c, members := groupWithMembers(t, 1)
	m := members[0]
	work := addWork(t, s, c, m, "research")
	box := Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	content := strings.Repeat("a", 90000) + "tail"
	result, err := box.Execute(context.Background(), c, m, work, toolCall("save_artifact", map[string]any{"title": "large", "content": content}))
	if err != nil || !strings.Contains(result, "succeeded") {
		t.Fatalf("%s %v", result, err)
	}
	var saved struct {
		Result struct {
			ID string `json:"artifact_id"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(result), &saved)
	for i := 0; i < 6; i++ {
		_, err = box.Execute(context.Background(), c, m, work, toolCall("save_artifact", map[string]any{"id": saved.Result.ID, "title": "large", "content": "revision"}))
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := box.Execute(context.Background(), c, m, work, toolCall("read_artifact", map[string]any{"id": saved.Result.ID, "version": 1, "length": 100000}))
	if err != nil || !strings.Contains(raw, "tail") {
		t.Fatalf("old version lost: %v", err)
	}
	// 第九个查询词仍参与实际检索。
	clauses, _ := likeTerms("a b c d e f g h tail")
	if len(clauses) != 9 {
		t.Fatal("query silently shortened")
	}
}

func TestWebRetryReleasesKnownFreeReservations(t *testing.T) {
	// 两次明确的限流失败后成功，只应保留一次成功费用，不能积累三份预留。
	s, c, members := groupWithMembers(t, 1)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"try later"}}`)
			return
		}
		fmt.Fprint(w, `{"results":[{"url":"https://example.com"}]}`)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.SearchURL = server.URL
	box := Toolbox{Store: s, Config: cfg, Hub: NewHub()}
	work := addWork(t, s, c, members[0], "research")
	raw, err := box.Execute(context.Background(), c, members[0], work, toolCall("search", map[string]any{"query": "retry"}))
	if err != nil || !strings.Contains(raw, "succeeded") {
		t.Fatalf("%s %v", raw, err)
	}
	usage, _ := s.Usage()
	if calls != 3 || usage.CostMicros != cfg.ToolCostMicros || usage.UnsettledMicros != 0 {
		t.Fatalf("calls=%d usage=%+v", calls, usage)
	}
}
