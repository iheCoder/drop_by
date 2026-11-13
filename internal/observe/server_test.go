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
	page := get("/spectrum.html")
	if page.Code != http.StatusOK {
		t.Fatalf("色谱页面状态 %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, "测光") || !strings.Contains(body, "/studio.js") || !strings.Contains(body, "/meter.js") {
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
	if health.Code != http.StatusOK || json.Unmarshal(health.Body.Bytes(), &payload) != nil || payload["source"] != "sample" {
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
