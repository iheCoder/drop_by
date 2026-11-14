package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func PostJSON(ctx context.Context, client *http.Client, endpoint, key string, body []byte, out any, raw *string) error {
	return postJSON(ctx, client, endpoint, "Authorization", "Bearer "+key, key, body, out, raw)
}

// Gemini 原生端点使用自己的密钥请求头。HTTP 客户端默认会在跳转时复制
// 这种自定义头，因而只向配置明确指定的 endpoint 发出请求。
func PostJSONWithAPIKey(ctx context.Context, client *http.Client, endpoint, header, key string, body []byte, out any, raw *string) error {
	local := *client
	local.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return postJSON(ctx, &local, endpoint, header, key, key, body, out, raw)
}

func postJSON(ctx context.Context, client *http.Client, endpoint, header, credential, key string, body []byte, out any, raw *string) error {
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set(header, credential)
	req.Header.Set("Content-Type", "application/json")
	if ctx.Err() != nil {
		return &Failure{Stage: ctx.Err().Error(), Cause: ctx.Err(), NotSent: true}
	}
	var wroteHeaders atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WroteHeaders: func() { wroteHeaders.Store(true) }}))
	r, e := client.Do(req)
	if e != nil {
		var op *net.OpError
		var dns *net.DNSError
		notSent := !wroteHeaders.Load() && ((errors.As(e, &op) && op.Op == "dial") || errors.As(e, &dns))
		var ne net.Error
		stage := "服务连接失败"
		if errors.As(e, &ne) && ne.Timeout() {
			stage = "等待服务响应超时"
		}
		// 取消和超时仍交由上层按原语义恢复，NotSent 只决定本笔预留如何结算。
		cause := e
		if ctx.Err() != nil {
			cause, stage = ctx.Err(), ctx.Err().Error()
		}
		return &Failure{Stage: stage, Cause: cause, Temporary: ctx.Err() == nil, NotSent: notSent}
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		wait := time.Duration(0)
		if seconds, err := strconv.Atoi(r.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(seconds) * time.Second
		} else if date, err := http.ParseTime(r.Header.Get("Retry-After")); err == nil {
			wait = time.Until(date)
		}
		var detail struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		rawBody, _ := io.ReadAll(r.Body)
		if raw != nil {
			*raw = string(rawBody)
		}
		_ = json.NewDecoder(bytes.NewReader(rawBody)).Decode(&detail)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		message := detail.Error.Message
		if key != "" {
			message = strings.ReplaceAll(message, key, "[credential]")
		}
		return &Failure{Detail: message, Stage: "服务请求失败", Status: r.StatusCode, RetryAfter: wait, Temporary: r.StatusCode == 429 || r.StatusCode == 408 || r.StatusCode >= 500}
	}
	b, e := io.ReadAll(r.Body)
	if raw != nil {
		*raw = string(b)
	}
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Failure{Stage: "读取服务结果中断", Temporary: true}
	}
	if e = json.Unmarshal(b, out); e != nil {
		return fmt.Errorf("服务返回了无法识别的 JSON")
	}
	return nil
}
