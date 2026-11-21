package observe

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestObservatoryServesItself(t *testing.T) {
	h := NewHandler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8097"+path, nil))
		return rec
	}
	// 正式首页和样例从同一份 HTML 加载渲染器，随后由脚本选择数据源。
	home, demo := get("/"), get("/demo.html")
	if home.Code != http.StatusOK || demo.Code != http.StatusOK || home.Body.String() != demo.Body.String() {
		t.Fatalf("首页和样例应共用页面，分别返回 %d 和 %d", home.Code, demo.Code)
	}
	if !strings.Contains(home.Body.String(), `type="module" src="/studio.js`) {
		t.Fatal("测光首页缺少共用渲染器")
	}
	page := get("/spectrum.html")
	if page.Code != http.StatusOK {
		t.Fatalf("色谱页面状态 %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, "测光") || !strings.Contains(body, "/studio.js") {
		t.Fatal("色谱页面缺少自己的前端")
	}
	if strings.Contains(body, "glass.css") || strings.Contains(body, "有空，来坐坐") {
		t.Fatal("测光不应套用聊天页面")
	}
	script := get("/meter.js?v=5")
	if script.Code != http.StatusOK || !strings.Contains(script.Body.String(), "原生压缩") {
		t.Fatalf("测光样例 %d", script.Code)
	}
	health := get("/api/health")
	var payload map[string]string
	if health.Code != http.StatusOK || json.Unmarshal(health.Body.Bytes(), &payload) != nil || payload["status"] != "ok" {
		t.Fatalf("健康检查 %d %s", health.Code, health.Body.String())
	}
	old := get("/observe")
	if old.Code != http.StatusOK || !strings.Contains(old.Body.String(), "测光") || strings.Contains(old.Body.String(), "glass.css") {
		t.Fatalf("原来的测光页面 %d", old.Code)
	}
	foreign := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/spectrum.html", nil)
	h.ServeHTTP(foreign, req)
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("非本机访问 %d", foreign.Code)
	}
	named := getHost("https://www.dropby.ob.ai/spectrum.html")
	if named.Code != http.StatusOK || !strings.Contains(named.Body.String(), "测光") {
		t.Fatalf("测光域名 %d", named.Code)
	}
}

func getHost(raw string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, raw, nil))
	return rec
}

func TestListenAddressStaysLoopback(t *testing.T) {
	t.Setenv("OBSERVE_ADDR", "0.0.0.0:8097")
	if _, err := ListenAddress(); err == nil {
		t.Fatal("测光不应监听非回环地址")
	}
	t.Setenv("OBSERVE_ADDR", "")
	addr, err := ListenAddress()
	if err != nil || addr != "127.0.0.1:8097" {
		t.Fatalf("默认地址 %q %v", addr, err)
	}
}

func TestChatProjectionReturnsOnlyObservatoryFields(t *testing.T) {
	// 用假聊天服务复现正式页的两种只读请求，检查真实上游路径和浏览器收到的字段。
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("测光只能读取聊天接口，收到 %s", r.Method)
		}
		switch r.URL.RequestURI() {
		case "/api/state":
			w.Write([]byte(`{"members":[{"id":"m1","name":"阿澄","profile":"不需要的画像"}],"conversations":[{"id":"room1","name":"群聊","kind":"group","member_ids":["m1"],"guidance":"不需要的指令"}]}`))
		case "/api/conversations/room1?activity=paged":
			w.Write([]byte(`{"messages":[{"id":"message1","author_id":"m1","body":"已发出的正文","created_at":"2026-11-14T21:28:00+08:00","evidence":["不需要的引用"]}],"has_earlier":true,"work":[{"instruction":"不需要的行动指令"}]}`))
		case "/api/usage/scope?conversation=room1&period=all":
			w.Write([]byte(`{"direct":{"estimate_micros":235}}`))
		default:
			t.Errorf("意外的聊天接口 %s", r.URL.RequestURI())
			http.NotFound(w, r)
		}
	}))
	defer chat.Close()

	h := newHandler(newChatReader(chat.URL))
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/api/catalog", `{"members":[{"id":"m1","name":"阿澄"}],"conversations":[{"id":"room1","name":"群聊","kind":"group","member_ids":["m1"]}]}`},
		{"/api/conversations/room1/messages", `{"messages":[{"id":"message1","author_id":"m1","body":"已发出的正文","created_at":"2026-11-14T21:28:00+08:00"}],"has_earlier":true}`},
		{"/api/costs?conversation=room1&period=all", `{"direct":{"estimate_micros":235}}`},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://www.dropby.ob.ai"+tc.path, nil))
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != tc.want {
			t.Fatalf("%s 返回 %d %s", tc.path, rec.Code, rec.Body.String())
		}
	}
}
