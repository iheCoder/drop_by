package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"drop_by/internal/dropby/model"
	"drop_by/internal/dropby/upstream"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// 握手可以重试。response.create 发出后，任何重连都必须由应用依据断点决定。
func (m *Client) Steer(ctx context.Context, raw []byte, inputEstimate int, input *model.SteeringInput, chain *[]model.ChatMessage, hooks model.SteeringHooks) (out model.Result, err error) {
	if m.HTTP.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.HTTP.Timeout)
		defer cancel()
	}
	var conn *websocket.Conn
	attempt := 0
	err = upstream.Retry(ctx, func() error {
		attempt++
		var response *http.Response
		var failure error
		conn, response, failure = websocket.Dial(ctx, m.BaseURL+"/responses", &websocket.DialOptions{
			HTTPClient: m.HTTP, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + m.APIKey}},
		})
		if failure != nil {
			if ctx.Err() != nil {
				failure = ctx.Err()
			} else {
				status := 0
				if response != nil {
					status = response.StatusCode
				}
				failure = &upstream.Failure{Stage: "建立 Responses WebSocket 失败", Status: status, Temporary: status == 0 || status == 408 || status == 429 || status >= 500}
			}
		}
		hooks.Connection(attempt, failure)
		return failure
	})
	if err != nil {
		return out, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(-1)

	var request map[string]json.RawMessage
	if err = json.Unmarshal(raw, &request); err != nil {
		return out, err
	}
	request["type"] = json.RawMessage(`"response.create"`)
	sentBody, _ := json.Marshal(request)
	call, err := hooks.Reserve(inputEstimate)
	if err != nil {
		return out, err
	}
	var queuedCall model.SteeringCall
	var transcript []string
	var completed *model.Result
	var steerStep string
	defer func() {
		if completed == nil {
			out.Raw = strings.Join(transcript, "\n")
			_ = call.Complete(out, err)
		}
		call.Close()
		if queuedCall != nil {
			queuedCall.Close()
		}
	}()
	if err = wsjson.Write(ctx, conn, request); err != nil {
		return out, err
	}
	call.Start(string(sentBody), "")

	type event struct {
		Type     string                `json:"type"`
		Response responseEnvelope      `json:"response"`
		Error    struct{ Code string } `json:"error"`
	}
	type received struct {
		Event event
		Raw   string
		Err   error
	}
	incoming := make(chan received, 1)
	readCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		for {
			var ev event
			_, data, err := conn.Read(readCtx)
			if err == nil {
				err = json.Unmarshal(data, &ev)
			}
			select {
			case incoming <- received{ev, string(data), err}:
			case <-readCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var responseID string
	var queued, applied []model.Update
	sent := map[string]bool{}
	active := false
	steer := func() error {
		if !active || len(queued) > 0 {
			return nil
		}
		updates, err := input.Read()
		if err != nil {
			return err
		}
		var batch []model.Update
		for _, msg := range updates {
			if !sent[msg.ID] {
				batch = append(batch, msg)
			}
		}
		if len(batch) == 0 {
			return nil
		}
		inputEstimate += m.MaxOutputTokens + len(asJSON(batch))
		queuedCall, err = hooks.Reserve(inputEstimate)
		if err != nil {
			// 额度不足的新增输入仍由应用在普通边界交付。
			return nil
		}
		queued = batch
		body := map[string]any{"type": "response.steer", "previous_response_id": responseID, "input": "真人追加的消息：" + asJSON(batch)}
		err = wsjson.Write(ctx, conn, body)
		steerStep = hooks.SendUpdate(asJSON(body), err)
		if err == nil {
			for _, msg := range batch {
				sent[msg.ID] = true
			}
		}
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case room := <-input.Events:
			if input.Room == "" || room == input.Room {
				if err := steer(); err != nil {
					return out, err
				}
			}
		case got := <-incoming:
			if got.Raw != "" {
				transcript = append(transcript, got.Raw)
			}
			if got.Err != nil {
				return out, &upstream.Failure{Stage: "Responses WebSocket 中断，未读消息保留", Cause: got.Err, Temporary: ctx.Err() == nil}
			}
			ev := got.Event
			switch ev.Type {
			case "response.created":
				if responseID != "" {
					if completed == nil || queuedCall == nil {
						return out, fmt.Errorf("服务返回无法关联的 steering 续接")
					}
					// 新响应创建才证明追加输入进入了续接。
					content := "真人追加的消息：" + asJSON(queued)
					if chain != nil {
						*chain = append(*chain, completed.Message, model.ChatMessage{Role: "user", Content: content})
					}
					applied = append(applied, queued...)
					hooks.FinishUpdate(steerStep, true)
					call.Close()
					call, queuedCall = queuedCall, nil
					call.Start(content, steerStep)
					transcript = []string{got.Raw}
					steerStep, queued, completed = "", nil, nil
				}
				responseID, active = ev.Response.ID, true
				if err := steer(); err != nil {
					return out, err
				}
			case "response.completed", "response.incomplete":
				active = false
				result, err := m.decodeResponse(ev.Response)
				if err != nil {
					return out, err
				}
				result.Raw = strings.Join(transcript, "\n")
				invalid := result.Incomplete && ev.Response.IncompleteDetails.Reason != "steered" && len(queued) > 0 && len(result.Message.ToolCalls) == 0
				var resultErr error
				if invalid {
					resultErr = fmt.Errorf("模型本次未完成输出")
				}
				completed = &result
				if err := call.Complete(result, resultErr); err != nil {
					return out, err
				}
				if input.Applied == nil {
					input.Applied = map[string]bool{}
				}
				for _, msg := range applied {
					input.Applied[msg.ID] = true
				}
				if resultErr != nil || queuedCall == nil {
					return result, resultErr
				}
				// 尚未启动的续接可释放预留。工具结果随后由应用按原调用配对。
				if len(result.Message.ToolCalls) > 0 {
					return result, queuedCall.Release()
				}
			case "response.steer.failed":
				hooks.FinishUpdate(steerStep, false)
				if queuedCall != nil {
					if err := queuedCall.Release(); err != nil {
						return out, err
					}
					queuedCall.Close()
				}
				queued, queuedCall = nil, nil
				if completed != nil {
					return *completed, nil
				}
			case "response.failed", "error":
				return out, fmt.Errorf("Responses WebSocket 返回失败：%s", ev.Error.Code)
			}
		}
	}
}
