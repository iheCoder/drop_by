package dropby

import (
	"net/http"
	"os"
	"testing"
	"time"
)

func TestSystemProxyLive(t *testing.T) {
	if os.Getenv("DROPBY_PROXY_LIVE") != "1" {
		t.Skip("local network check")
	}
	// 清空显式代理，验证实际系统配置；不携带凭据或调用生成接口。
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(key, "")
	}
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
