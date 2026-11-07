package dropby

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
)

func newProxyTransport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		return requestProxy(req, httpproxy.FromEnvironment(), readSystemProxy)
	}
	return transport
}

// 显式配置对相应协议优先；失败也不改用另一个地址。未配置才读取系统，
// 每次请求按需读取，因此更改系统端口无需重启，也不需要后台轮询和缓存。
func requestProxy(req *http.Request, config *httpproxy.Config, read func(context.Context) (map[string]string, []string, error)) (*url.URL, error) {
	explicit := config.HTTPProxy
	if req.URL.Scheme == "https" {
		explicit = config.HTTPSProxy
	}
	if explicit != "" {
		return config.ProxyFunc()(req.URL)
	}
	// 用标准 NO_PROXY 匹配规则先判断直连，避免本地请求触发系统查询。
	check := *config
	check.HTTPProxy = "http://proxy.invalid"
	check.HTTPSProxy = check.HTTPProxy
	selected, err := check.ProxyFunc()(req.URL)
	if err != nil || selected == nil {
		return nil, err
	}
	settings, exceptions, err := read(req.Context())
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(req.URL.Hostname())
	if settings["ExcludeSimpleHostnames"] == "1" && !strings.Contains(host, ".") && net.ParseIP(host) == nil {
		return nil, nil
	}
	for _, pattern := range exceptions {
		// macOS 例外包含 192.168.* 一类通配符；CIDR 和域名规则交给标准匹配器。
		if matched, _ := path.Match(strings.ToLower(pattern), host); matched {
			return nil, nil
		}
	}
	config.NoProxy = strings.Join(append([]string{config.NoProxy}, exceptions...), ",")
	prefix := "HTTP"
	if req.URL.Scheme == "https" {
		prefix = "HTTPS"
	}
	proxy := ""
	if settings[prefix+"Enable"] == "1" {
		proxy = systemProxyURL(settings, prefix, "http")
	} else if settings["SOCKSEnable"] == "1" {
		proxy = systemProxyURL(settings, "SOCKS", "socks5")
	} else if settings["ProxyAutoConfigEnable"] == "1" || settings["ProxyAutoDiscoveryEnable"] == "1" {
		return nil, fmt.Errorf("当前系统使用自动代理脚本，请显式配置 HTTP_PROXY 或 HTTPS_PROXY")
	}
	config.HTTPProxy = proxy
	config.HTTPSProxy = proxy
	return config.ProxyFunc()(req.URL)
}

func systemProxyURL(settings map[string]string, prefix, scheme string) string {
	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort(settings[prefix+"Proxy"], settings[prefix+"Port"])}).String()
}

// scutil 读取 macOS 当前有效的代理配置；其他系统继续使用环境变量或直连。
func readSystemProxy(ctx context.Context) (map[string]string, []string, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return nil, nil, fmt.Errorf("无法读取 macOS 系统代理设置")
	}
	values, exceptions := parseSystemProxy(string(output))
	return values, exceptions, nil
}

func parseSystemProxy(output string) (map[string]string, []string) {
	values := map[string]string{}
	var exceptions []string
	depth := 0
	inExceptions := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "{") {
			depth++
			if depth == 2 && strings.HasPrefix(line, "ExceptionsList :") {
				inExceptions = true
			}
			continue
		}
		if line == "}" {
			if depth == 2 {
				inExceptions = false
			}
			depth--
			continue
		}
		key, value, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		if depth == 1 {
			values[key] = value
		} else if depth == 2 && inExceptions {
			exceptions = append(exceptions, value)
		}
	}
	return values, exceptions
}

var serviceTransport = newProxyTransport()
