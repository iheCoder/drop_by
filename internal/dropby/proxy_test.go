package dropby

import (
	"context"
	"golang.org/x/net/http/httpproxy"
	"net/http"
	"testing"
)

func TestProxyConfigurationPriority(t *testing.T) {
	// 系统端口故意使用任意值，验证没有限定旧端口；显式配置和 NO_PROXY 都不读取系统。
	for _, tc := range []struct {
		name, target, explicit, noProxy, want string
		reads                                 int
	}{
		{"explicit", "https://example.com", "http://localhost:4567", "", "http://localhost:4567", 0},
		{"system", "https://example.com", "", "", "http://127.0.0.1:12345", 1},
		{"no proxy", "https://example.com", "", "example.com", "", 0},
		{"explicit exclusion", "https://example.com", "http://localhost:4567", "example.com", "", 0},
		{"localhost", "http://127.0.0.1:8090", "", "", "", 0},
		{"system wildcard", "https://192.168.1.3", "", "", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			req, _ := http.NewRequest("GET", tc.target, nil)
			config := &httpproxy.Config{HTTPSProxy: tc.explicit, NoProxy: tc.noProxy}
			proxy, err := requestProxy(req, config, func(context.Context) (map[string]string, []string, error) {
				calls++
				return map[string]string{"HTTPSEnable": "1", "HTTPSProxy": "127.0.0.1", "HTTPSPort": "12345"}, []string{"192.168.*"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if proxy != nil {
				got = proxy.String()
			}
			if got != tc.want || calls != tc.reads {
				t.Fatalf("proxy=%q reads=%d", got, calls)
			}
		})
	}
}

func TestSystemProxyChangesWithoutRestart(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	settings := map[string]string{"HTTPSEnable": "1", "HTTPSProxy": "localhost", "HTTPSPort": "1234"}
	read := func(context.Context) (map[string]string, []string, error) { return settings, nil, nil }
	for _, port := range []string{"1234", "5678"} {
		settings["HTTPSPort"] = port
		proxy, err := requestProxy(req, &httpproxy.Config{}, read)
		if err != nil || proxy.Port() != port {
			t.Fatalf("did not follow port %s: %v %v", port, proxy, err)
		}
	}
	// 系统关闭代理应回到直连，不继续沿用旧地址。
	settings["HTTPSEnable"] = "0"
	proxy, err := requestProxy(req, &httpproxy.Config{}, read)
	if proxy != nil || err != nil {
		t.Fatalf("disabled: %v %v", proxy, err)
	}
}

func TestParseSystemProxy(t *testing.T) {
	// 忽略接口级嵌套配置，只使用系统当前有效的顶层值和例外列表。
	settings, exceptions := parseSystemProxy(`<dictionary> {
 HTTPSProxy : 127.0.0.1
 HTTPSPort : 3456
 HTTPSEnable : 1
 ExceptionsList : <array> {
  0 : *.local
 }
 __SCOPED__ : <dictionary> {
  HTTPSPort : 9999
 }
}`)
	if settings["HTTPSPort"] != "3456" || len(exceptions) != 1 || exceptions[0] != "*.local" {
		t.Fatalf("%v %v", settings, exceptions)
	}
}

// HTTP 和 SOCKS 都从系统给出的任意地址构造；PAC 不会被误当成直连。
func TestSystemProxyModes(t *testing.T) {
	for _, tc := range []struct {
		target   string
		settings map[string]string
		want     string
		wantErr  bool
	}{
		{"http://example.com", map[string]string{"HTTPEnable": "1", "HTTPProxy": "proxy.example", "HTTPPort": "8888"}, "http://proxy.example:8888", false},
		{"https://example.com", map[string]string{"SOCKSEnable": "1", "SOCKSProxy": "::1", "SOCKSPort": "2345"}, "socks5://[::1]:2345", false},
		{"https://example.com", map[string]string{"ProxyAutoConfigEnable": "1"}, "", true},
	} {
		req, _ := http.NewRequest("GET", tc.target, nil)
		proxy, err := requestProxy(req, &httpproxy.Config{}, func(context.Context) (map[string]string, []string, error) { return tc.settings, nil, nil })
		got := ""
		if proxy != nil {
			got = proxy.String()
		}
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Fatalf("proxy=%q err=%v", got, err)
		}
	}
}
