package dropby

import (
	"context"
	"net"
	"net/http"
	"time"
)

// 只为配置中的本机 HTTP 代理提供端口兼容。NO_PROXY、直连、远程代理
// 和显式带凭据的代理仍遵循原配置，不扫描端口或改变系统网络设置。
type localProxyTransport struct{ transport *http.Transport }
type localProxyAddress struct{}

func newProxyTransport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = proxyDial(dialer.DialContext)
	return &localProxyTransport{transport: transport}
}

func (t *localProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	proxy, err := t.transport.Proxy(req)
	if err != nil {
		return nil, err
	}
	if proxy != nil && proxy.Scheme == "http" && proxy.User == nil {
		host, port, err := net.SplitHostPort(proxy.Host)
		if err == nil && (host == "localhost" || host == "127.0.0.1" || host == "::1") && (port == "7790" || port == "7890") {
			req = req.WithContext(context.WithValue(req.Context(), localProxyAddress{}, proxy.Host))
		}
	}
	return t.transport.RoundTrip(req)
}

// 回退发生在 TCP 连接建立之前，尚未发送 HTTP 请求或工具内容。
// 已建立连接后的超时、HTTP 错误不触发切换，因此不会借端口切换重发动作。
func proxyDial(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err == nil || ctx.Err() != nil || ctx.Value(localProxyAddress{}) != address {
			return conn, err
		}
		host, port, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return conn, err
		}
		switch port {
		case "7790":
			port = "7890"
		case "7890":
			port = "7790"
		default:
			return conn, err
		}
		return dial(ctx, network, net.JoinHostPort(host, port))
	}
}

var serviceTransport = newProxyTransport()
