package dropby

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
)

func TestProxyFallbackOnlyBeforeConnection(t *testing.T) {
	// 模拟 TCP 拒绝连接：只有已确认的本机代理才允许换端口。
	for _, tc := range []struct {
		name, address, marked string
		want                  []string
	}{
		{"old to current", "127.0.0.1:7790", "127.0.0.1:7790", []string{"127.0.0.1:7790", "127.0.0.1:7890"}},
		{"current to old", "127.0.0.1:7890", "127.0.0.1:7890", []string{"127.0.0.1:7890", "127.0.0.1:7790"}},
		{"direct target unchanged", "127.0.0.1:7790", "", []string{"127.0.0.1:7790"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			dial := proxyDial(func(_ context.Context, _, address string) (net.Conn, error) {
				calls = append(calls, address)
				return nil, errors.New("refused")
			})
			ctx := context.WithValue(context.Background(), localProxyAddress{}, tc.marked)
			_, _ = dial(ctx, "tcp", tc.address)
			if !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("calls %v, want %v", calls, tc.want)
			}
		})
	}
	// 连接成功不探测备用端口；取消的请求也不再建立连接。
	for _, cancelled := range []bool{false, true} {
		calls := 0
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), localProxyAddress{}, "127.0.0.1:7790"))
		if cancelled {
			cancel()
		}
		dial := proxyDial(func(context.Context, string, string) (net.Conn, error) {
			calls++
			if cancelled {
				return nil, context.Canceled
			}
			return nil, nil
		})
		_, _ = dial(ctx, "tcp", "127.0.0.1:7790")
		cancel()
		if calls != 1 {
			t.Fatalf("unexpected fallback: %d", calls)
		}
	}
}
