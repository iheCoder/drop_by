package dropby

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 从有旧历史的个人窗口连续开启独立行动，捕获真正发出的请求。第一次仍用短缓存；
// 间隔半小时后把相同旧历史存久一点，刚产生的工具结果继续只用短缓存。
func TestPersonalExplorationKeepsLongCacheAtRepeatedHistory(t *testing.T) {
	s, _, member := fixture(t)
	room, err := s.personalConversation(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := "先前确认的原文：" + strings.Repeat("多次活动会用到的资料。", 420)
	window := []ChatMessage{{Role: "user", Content: old}}
	if _, err = s.db.Exec(`INSERT INTO working_contexts VALUES('',?,0,?)`, member.ID, asJSON(window)); err != nil {
		t.Fatal(err)
	}

	type modelRequest struct {
		Input   []map[string]json.RawMessage `json:"input"`
		Tools   json.RawMessage              `json:"tools"`
		Options map[string]string            `json:"prompt_cache_options"`
	}
	requests := make(chan modelRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request modelRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		number := len(requests) + 1
		requests <- request
		output := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"outcome\":\"已经看过\"}"}]}`)
		if number == 2 {
			output = json.RawMessage(`{"type":"function_call","name":"remember","call_id":"call_cache_fixture","arguments":"{\"body\":\"本次新发现\"}"}`)
		}
		response := responseEnvelope{ID: "resp_" + strconv.Itoa(number), Status: "completed", Output: []json.RawMessage{output}}
		response.Usage.InputTokens, response.Usage.OutputTokens = 2600, 50
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.BaseURL, cfg.Model, cfg.DeepReasoning = server.URL, "gpt-6.1-sol", "high"
	engine := NewEngine(s, &HTTPModel{Store: s, Config: cfg, Client: server.Client()}, cfg)
	run := func() {
		t.Helper()
		work := Work{ID: newID("work"), MemberID: member.ID, Kind: "explore", Status: "pending",
			Instruction: "这是你自己的活动时间。", CreatedAt: now(), DueAt: time.Now().UnixMilli()}
		if err := s.Guard(room.ID, room.Revision, func(tx *sql.Tx) error { return insertWork(tx, work) }); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.memberAction(t.Context(), room, member, work); err != nil {
			t.Fatal(err)
		}
	}
	run()
	// 模拟已过 30 分钟，只改变缓存观察时钟，不改成员当前情况与工具流程。
	engine.gate(member.ID).requestCaches[""].lastAt = time.Now().Add(-31 * time.Minute)
	run()
	engine.gate(member.ID).requestCaches[""].lastAt = time.Now().Add(-31 * time.Minute)
	run()
	if len(requests) != 4 {
		t.Fatalf("应捕获首次、下次行动、工具续接和第三次行动四份请求，实际 %d", len(requests))
	}
	first, next, tool, third := <-requests, <-requests, <-requests, <-requests
	wantComparison := []string{"", "resp_1", "resp_2", "resp_3"}
	wantMarks := []map[int]string{{1: ""}, {1: "12h"}, {1: "12h", 3: ""}, {1: "12h", 3: ""}}
	for i, req := range []modelRequest{first, next, tool, third} {
		if !reflect.DeepEqual(first.Tools, req.Tools) {
			t.Fatal("缓存改变了工具")
		}
		if req.Options["comparison_response_id"] != wantComparison[i] {
			t.Fatal("诊断应只引用同成员实际收到的上一份响应")
		}
		if got := requestCacheMarks(t, req.Input); !reflect.DeepEqual(got, wantMarks[i]) {
			t.Fatalf("请求 %d 的缓存标记为 %v，应为 %v", i, got, wantMarks[i])
		}
		if !reflect.DeepEqual(first.Input[:2], req.Input[:2]) {
			t.Fatal("增加缓存标记改变了身份或原始文字")
		}
	}
	if !strings.Contains(string(tool.Input[3]["output"]), "tool_run_id") ||
		string(tool.Input[3]["call_id"]) != `"call_cache_fixture"` {
		t.Fatal("本次工具的原始配对丢失")
	}
}

// 改写旧窗口后，即使最后那条消息文字相同，也不能继续按原来的开头写长缓存。
func TestPersonalCacheDropsLongMarkWhenEarlierInputChanges(t *testing.T) {
	model := &HTTPModel{Config: testConfig()}
	at := time.Now()
	first := memberCacheRequest{workID: "one", at: at}
	messages := []ChatMessage{
		{Role: "system", Content: "角色设定"},
		{Role: "user", Content: "旧记录"},
		{Role: "user", Content: "仍原样保留的最后一句"},
		{Role: "user", Content: `{"self":{},"trigger":{},"time":"today"}`},
	}
	plan := func(r *memberCacheRequest) {
		t.Helper()
		if _, err := model.responseBodyWithCache(messages, nil, true, r); err != nil {
			t.Fatal(err)
		}
		r.record(t.Context(), model.Config.Model, modelResult{})
	}
	plan(&first)
	quick := memberCacheRequest{next: first.next, workID: "quick", at: at.Add(time.Minute)}
	plan(&quick)
	if quick.longWrite {
		t.Fatal("短间隔的独立行动应沿用短缓存")
	}
	second := memberCacheRequest{next: first.next, workID: "two", at: at.Add(31 * time.Minute)}
	plan(&second)
	if !second.longWrite {
		t.Fatal("同一段历史跨活动出现，应启用长缓存")
	}
	messages[1] = ChatMessage{Role: "user", ResponseItems: []json.RawMessage{
		json.RawMessage(`{"type":"compaction","encrypted_content":"opaque"}`),
	}}
	compacted := memberCacheRequest{next: second.next, workID: "two", at: at.Add(32 * time.Minute)}
	plan(&compacted)
	if compacted.longWrite || !compacted.next.longUntil.IsZero() {
		t.Fatal("压缩后沿用了旧缓存线")
	}
}

func TestLongCacheCostWithMissingDurationBreakdown(t *testing.T) {
	price, _ := priceForModel("gpt-6.1-sol")
	model := &HTTPModel{longCacheRequested: true}
	u := responseUsage{InputTokens: 2000}
	u.InputDetails.CachedTokens, u.InputDetails.CacheWriteTokens = 1000, 1000
	// 网关仅给出写入总量时，长标记可能覆盖这 1000 token。读中部分仍按读取价。
	got, err := model.responseCost(u, price)
	if err != nil || got != 5100 {
		t.Fatalf("长标记缺时长明细应保守估算 $0.0051，实际 %d 微美元 (%v)", got, err)
	}
}

// 去掉标记后逐项比对模型看到的文字，同时核对缓存线实际写入的位置和时长。
func requestCacheMarks(t *testing.T, input []map[string]json.RawMessage) map[int]string {
	t.Helper()
	marks := map[int]string{}
	for i, item := range input {
		for _, field := range []string{"content", "output"} {
			var block []struct {
				Text   string            `json:"text"`
				Marker map[string]string `json:"prompt_cache_breakpoint"`
			}
			if json.Unmarshal(item[field], &block) == nil && len(block) == 1 && block[0].Marker["mode"] == "explicit" {
				marks[i] = block[0].Marker["ttl"]
				item[field], _ = json.Marshal(block[0].Text)
			}
		}
	}
	return marks
}
