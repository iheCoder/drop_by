package dropby

import (
	"net/http"
	"os"
	"testing"
	"time"
)

func TestLocalProxyFallbackLive(t *testing.T) {
	if os.Getenv("DROPBY_PROXY_LIVE") != "1" {
		t.Skip("local network check")
	}
	// 不携带凭据、不调用生成接口；401 只验证请求经备用代理抵达服务。
	c := http.Client{Transport: newProxyTransport(), Timeout: 15 * time.Second}
	r, e := c.Get("https://api.openai.com/v1/models")
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("HTTP %d", r.StatusCode)
	}
}
