package dropby

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatDoesNotServeObservatory(t *testing.T) {
	h := NewHandler(nil)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil))
		return rec
	}
	for _, path := range []string{"/observe", "/observe/", "/spectrum.html", "/studio.js", "/prototypes/spectrum.html"} {
		page := get(path)
		if page.Code == http.StatusOK && strings.Contains(page.Body.String(), "测光") {
			t.Fatalf("%s 仍由聊天服务提供测光", path)
		}
	}
	chat := get("/")
	if chat.Code != http.StatusOK || !strings.Contains(chat.Body.String(), "有空，来坐坐") {
		t.Fatalf("聊天首页 %d", chat.Code)
	}
	if strings.Contains(chat.Body.String(), "测光") {
		t.Fatal("聊天首页不应带上测光")
	}
}
