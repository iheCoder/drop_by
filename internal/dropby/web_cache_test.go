package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebCacheExactRequestsAndAccounting(t *testing.T) {
	// 两位成员各自留下真实工具记录；相同请求共享服务结果，不重复收取搜索费。
	s, c, members := groupWithMembers(t, 2)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"results":[{"title":"来源","url":"https://example.com","summary":"材料"}]}`)
	}))
	defer upstream.Close()
	cfg := testConfig()
	cfg.SearchURL = upstream.URL
	box := Toolbox{Store: s, Config: cfg, Hub: NewHub()}
	var firstTime string
	for i, m := range members {
		w := addWork(t, s, c, m, "research")
		call := toolCall("search", map[string]string{"query": "节目 CP"})
		call.ID = fmt.Sprint(i)
		raw, err := box.Execute(context.Background(), c, m, w, call)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Result struct {
				Reused    bool   `json:"reused"`
				Retrieved string `json:"retrieved_at"`
			} `json:"result"`
		}
		if err = json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		if result.Result.Reused != (i > 0) {
			t.Fatalf("reuse: %s", raw)
		}
		if i == 0 {
			firstTime = result.Result.Retrieved
		} else if firstTime != result.Result.Retrieved {
			t.Fatal("cache hit changed retrieval time")
		}
	}
	usage, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || usage.CostMicros != cfg.ToolCostMicros {
		t.Fatalf("requests=%d usage=%+v", requests.Load(), usage)
	}
	runs, err := s.ToolRuns(c.ID, "")
	if err != nil || len(runs) != 2 {
		t.Fatalf("each member needs record: %v %v", runs, err)
	}
	summary, err := s.actionSummary(runs[0])
	if err != nil || summary.Title != "查看已有搜索结果" {
		t.Fatalf("cache attribution: %+v %v", summary, err)
	}
	// 关键词变化即请求；有效期从取得结果起算，命中不会续期。
	w := addWork(t, s, c, members[0], "research")
	if _, err = box.Execute(context.Background(), c, members[0], w, toolCall("search", map[string]string{"query": "节目 二创"})); err != nil {
		t.Fatal(err)
	}
	box.webCache.mu.Lock()
	for _, e := range box.webCache.entries {
		e.completed = time.Now().Add(-webCacheTTL)
	}
	box.webCache.mu.Unlock()
	if _, err = box.Execute(context.Background(), c, members[0], w, toolCall("search", map[string]string{"query": "节目 CP"})); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatalf("changed/expired requests=%d", requests.Load())
	}
}

func TestWebCacheConcurrentIsolationAndFailure(t *testing.T) {
	// 缓存层并发合并同请求；不同聊天、不同路径及不同 URL 参数不可共享。
	box := Toolbox{Config: testConfig()}
	var calls atomic.Int32
	fetch := func() (map[string]any, error) { calls.Add(1); return map[string]any{"content": "page"}, nil }
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := box.cachedWeb(context.Background(), "room", "fetch", map[string]string{"url": "https://example.com?a=1"}, fetch)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent requests=%d", calls.Load())
	}
	for _, input := range []struct{ room, path, url string }{{"other", "fetch", "https://example.com?a=1"}, {"room", "search", "https://example.com?a=1"}, {"room", "fetch", "https://example.com?a=2"}} {
		if _, err := box.cachedWeb(context.Background(), input.room, input.path, map[string]string{"url": input.url}, fetch); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("scope mismatch=%d", calls.Load())
	}
	failed := func() (map[string]any, error) { return nil, errors.New("unavailable") }
	if _, err := box.cachedWeb(context.Background(), "room", "fetch", "fail", failed); err == nil {
		t.Fatal("failure hidden")
	}
	if _, err := box.cachedWeb(context.Background(), "room", "fetch", "fail", fetch); err != nil {
		t.Fatal("failure cached", err)
	}
	// 等待者取消不影响请求发起者，也不会把取消结果留下缓存。
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := box.cachedWeb(context.Background(), "room", "fetch", "waiting", func() (map[string]any, error) { close(entered); <-release; return fetch() })
		finished <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := box.cachedWeb(ctx, "room", "fetch", "waiting", fetch); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait cancellation: %v", err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestSearchOverviewKeepsEverySource(t *testing.T) {
	// 第一篇正文很长时，旧的前缀截断会吞掉后四篇。新摘要必须保留所有入口、
	// 原始查询时间和 tool_run_id，并仍可继续缩短而不丢失列表。
	entries := []searchResult{}
	for i := 0; i < 5; i++ {
		entries = append(entries, searchResult{Title: fmt.Sprintf("source%d", i), URL: fmt.Sprintf("https://example.com/%d", i), Summary: strings.Repeat("长文章", 3000)})
	}
	raw := asJSON(map[string]any{"tool_run_id": "tool_example", "result": map[string]any{"results": entries, "retrieved_at": "2026-11-07T12:00:00Z", "reused": true}})
	for _, limit := range []int{6000, 700} {
		raw = toolExcerpt(raw, limit)
	}
	for i := 0; i < 5; i++ {
		if !strings.Contains(raw, fmt.Sprintf("source%d", i)) {
			t.Fatalf("lost source %d", i)
		}
	}
	if !json.Valid([]byte(raw)) || !strings.Contains(raw, "2026-11-07T12:00:00Z") || !strings.Contains(raw, "tool_example") {
		t.Fatal("lost provenance", raw)
	}
}

func TestNewArrangementsAreDistinctButReplaysAreSafe(t *testing.T) {
	// 相同指令的新工具调用仍建立新安排；重放原 call ID 只能返回原记录，
	// 不能重复委托、改写预约时点，也不能通过取消再安排来模拟恢复。
	for _, name := range []string{"schedule_exploration", "schedule_followup", "delegate_work"} {
		t.Run(name, func(t *testing.T) {
			s, c, members := groupWithMembers(t, 2)
			var err error
			c, err = s.SetCoordinator(c, members[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			w := addWork(t, s, c, members[0], "research")
			box := Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
			args := map[string]any{"instruction": "核对同一个节目", "reason": "本次自己的问题", "delay_seconds": 60}
			if name == "delegate_work" {
				args = map[string]any{"instruction": "核对同一个节目", "reason": "本次自己的问题", "member_id": members[1].ID}
			}
			call := toolCall(name, args)
			call.ID = "first"
			first, err := box.Execute(context.Background(), c, members[0], w, call)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := box.Execute(context.Background(), c, members[0], w, call)
			if err != nil || replay != first {
				t.Fatalf("replay changed result: %s %s %v", first, replay, err)
			}
			call.ID = "second"
			second, err := box.Execute(context.Background(), c, members[0], w, call)
			if err != nil {
				t.Fatal(err)
			}
			var a, b struct {
				Result struct {
					ID string `json:"work_id"`
				} `json:"result"`
			}
			json.Unmarshal([]byte(first), &a)
			json.Unmarshal([]byte(second), &b)
			if a.Result.ID == "" || a.Result.ID == b.Result.ID {
				t.Fatalf("new intent swallowed: %s %s", first, second)
			}
			var count int
			if err = s.db.QueryRow("SELECT count(*) FROM work WHERE parent_id=?", w.ID).Scan(&count); err != nil || count != 2 {
				t.Fatalf("arrangements=%d err=%v", count, err)
			}
		})
	}
}

func TestPageCacheUsesExactURLAndDoesNotCacheEmptyContent(t *testing.T) {
	// 使用公开 IP 字面量避免测试依赖 DNS；实际请求只到本地模拟的 Fetch API，
	// 不访问该公开地址。抓取成功和正文有效都满足后才缓存。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if strings.HasSuffix(body["url"], "empty") {
			fmt.Fprint(w, `{"title":"空页面","content":""}`)
			return
		}
		fmt.Fprint(w, `{"title":"页面","content":"实际正文"}`)
	}))
	defer upstream.Close()
	cfg := testConfig()
	cfg.SearchURL = upstream.URL
	box := Toolbox{Store: s, Config: cfg, Hub: NewHub()}
	for _, url := range []string{"https://8.8.8.8/page?a=1", "https://8.8.8.8/page?a=1", "https://8.8.8.8/page?a=2"} {
		result, err := box.perform(context.Background(), c, m, w, toolCall("read_page", map[string]string{"url": url}))
		if err != nil || !strings.Contains(asJSON(result), "实际正文") {
			t.Fatalf("page: %v %v", result, err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("page requests=%d", requests.Load())
	}
	for i := 0; i < 2; i++ {
		if _, err := box.perform(context.Background(), c, m, w, toolCall("read_page", map[string]string{"url": "https://8.8.8.8/empty"})); err == nil {
			t.Fatal("empty body accepted")
		}
	}
	if requests.Load() != 4 {
		t.Fatal("empty response was cached")
	}
}

func TestFoldedPageKeepsRetrievalTime(t *testing.T) {
	// 网页长正文被折叠两次后，仍能区别原抓取时间与本次读取时间。
	raw := asJSON(map[string]any{"tool_run_id": "page1", "result": map[string]any{"content": strings.Repeat("正文", 5000), "retrieved_at": "2026-11-07T12:00:00Z", "reused": true}})
	raw = toolExcerpt(toolExcerpt(raw, 6000), 700)
	var excerpt map[string]any
	if err := json.Unmarshal([]byte(raw), &excerpt); err != nil {
		t.Fatal(err)
	}
	if excerpt["retrieved_at"] != "2026-11-07T12:00:00Z" || excerpt["reused"] != true {
		t.Fatal("lost page provenance", excerpt)
	}
}
