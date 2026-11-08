package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type steeringKey struct{}
type personalSinceKey struct{}
type steeringInput struct {
	Events  <-chan string
	Room    string
	Read    func() ([]Message, error)
	Applied map[string]bool
}

// 只将真人追加内容送入 steering。事件只是唤醒提示，SQLite 才是可靠来源，
// 因而连接失败或通知合并都不会消耗未读位置；同伴发言仍在普通行动边界接续。
func (e *Engine) completeMemberModel(ctx context.Context, c Conversation, seen *int64, messages *[]ChatMessage, defs []ToolDefinition) (ChatMessage, error) {
	if !e.Config.MidTurnSteering {
		return completeModel(ctx, e.Model, messages, defs, true)
	}
	events, unsubscribe := e.Hub.Subscribe()
	defer unsubscribe()
	input := &steeringInput{Applied: map[string]bool{}, Events: events, Room: c.ID, Read: func() ([]Message, error) {
		var all []Message
		var err error
		if c.ID == "" {
			since, _ := ctx.Value(personalSinceKey{}).(string)
			all, err = e.Store.personalMessages(c.MemberIDs[0], *seen, since)
		} else {
			all, err = e.Store.messagesAfter(c.ID, *seen, 0, -1)
		}
		var human []Message
		for _, msg := range all {
			if msg.AuthorID == "user" {
				human = append(human, msg)
			}
		}
		return human, err
	}}
	reply, err := completeModel(context.WithValue(ctx, steeringKey{}, input), e.Model, messages, defs, true)
	if err == nil {
		var arrived []Message
		var readErr error
		if c.ID == "" {
			since, _ := ctx.Value(personalSinceKey{}).(string)
			arrived, readErr = e.Store.personalMessages(c.MemberIDs[0], *seen, since)
		} else {
			arrived, readErr = e.Store.messagesAfter(c.ID, *seen, 0, -1)
		}
		if readErr != nil {
			return reply, readErr
		}
		// steering 可能越过尚未交付的同伴消息，按真实 Applied 集登记而不猜测连续范围。
		call, _ := ctx.Value(usageCallKey{}).(usageCall)
		var delivered []Message
		for _, msg := range arrived {
			if input.Applied[msg.ID] {
				delivered = append(delivered, msg)
			}
		}
		if call.member != "" {
			if recordErr := e.Store.recordProfileMessages(call.member, delivered); recordErr != nil {
				slog.Warn("记录后台整理来源失败", "error", recordErr)
			}
		}
		// 只能推进连续已应用的原文；夹在其中的 BOT 消息不能被人类 steering 跳过。
		for _, msg := range arrived {
			if !input.Applied[msg.ID] {
				break
			}
			*seen = msg.Seq
		}
	}
	return reply, err
}

// 一个连接处理本次响应及其自动续接。每份响应分别记账，steer.accepted 不是
// 已消费回执。若服务要求客户端工具，先返回原工具调用；未应用的补充仍在消息库，
// 下一次携工具结果的完整请求会包含它，绝不把排队状态当作模型已经读到。
func (m *HTTPModel) steeredResponse(ctx context.Context, raw []byte, price modelPrice, reserve int64, inputEstimate int, input *steeringInput, chain *[]ChatMessage) (modelResult, error) {
	if m.Client.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.Client.Timeout)
		defer cancel()
	}
	client := *m.Client
	if client.Transport == nil {
		client.Transport = serviceTransport
	}
	var conn *websocket.Conn
	// 握手尚未发送 response.create，可安全沿用现有网络重试；生成开始后不自动重放。
	err := retryRequest(ctx, func() error {
		var response *http.Response
		var dialErr error
		conn, response, dialErr = websocket.Dial(ctx, m.Config.BaseURL+"/responses", &websocket.DialOptions{HTTPClient: &client, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + m.Config.APIKey}}})
		if dialErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return &requestFailure{Stage: "建立 Responses WebSocket 失败", Status: status, Temporary: status == 0 || status == 408 || status == 429 || status >= 500}
	})
	if err != nil {
		return modelResult{}, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(-1) // 容量由模型请求边界管理，不另设截断工具正文的 WebSocket 大小限制。
	call, err := m.Store.reserveCall(ctx, m.Config, reserve, "OpenAI", m.Config.Model, "")
	if err != nil {
		return modelResult{}, &modelAccountingError{err}
	}
	var queuedCall usageReservation
	defer func() {
		m.Store.finishUsageCall(call)
		m.Store.finishUsageCall(queuedCall)
	}()
	var request map[string]any
	if err = json.Unmarshal(raw, &request); err != nil {
		return modelResult{}, err
	}
	request["type"] = "response.create"
	if err = wsjson.Write(ctx, conn, request); err != nil {
		return modelResult{}, err
	}

	type event struct {
		Type     string           `json:"type"`
		Response responseEnvelope `json:"response"`
		Error    struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	type received struct {
		Event event
		Err   error
	}
	incoming := make(chan received, 1)
	readCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		for {
			var ev event
			err := wsjson.Read(readCtx, conn, &ev)
			select {
			case incoming <- received{ev, err}:
			case <-readCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var responseID string
	var queued []Message
	sent := map[string]bool{}
	active := false
	var completed *modelResult
	var applied []Message
	// 同一响应只维护一批待应用补充。后续内容仍在数据库，续接开始后再投递，
	// 不堆叠无法确定生效顺序的请求，也不通过模型分类是否值得补充。
	steer := func() error {
		if !active || len(queued) > 0 {
			return nil
		}
		updates, err := input.Read()
		if err != nil {
			return err
		}
		batch := []Message{}
		for _, msg := range updates {
			if !sent[msg.ID] {
				batch = append(batch, msg)
			}
		}
		if len(batch) == 0 {
			return nil
		}
		// 自动续接也会计费，必须在触发它之前获得预算；额度不足时留到普通接续。
		// 续接包含上一响应及追加原文，预留不能重复使用首次短输入的金额。
		inputEstimate += m.Config.MaxOutputTokens + len(asJSON(batch))
		queuedReserve := price.reserveMicros(inputEstimate, m.Config.MaxOutputTokens)
		queuedCall, err = m.Store.reserveCall(ctx, m.Config, queuedReserve, "OpenAI", m.Config.Model, "")
		if err != nil {
			queuedCall = usageReservation{}
			return nil
		}
		queued = batch
		err = wsjson.Write(ctx, conn, map[string]any{"type": "response.steer", "previous_response_id": responseID, "input": "真人追加的消息：" + asJSON(batch)})
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
			return modelResult{}, ctx.Err()
		case room := <-input.Events:
			if input.Room == "" || room == input.Room {
				if err := steer(); err != nil {
					return modelResult{}, err
				}
			}
		case got := <-incoming:
			if got.Err != nil {
				return modelResult{}, &requestFailure{Stage: "Responses WebSocket 中断，未读消息保留", Cause: got.Err, Temporary: ctx.Err() == nil}
			}
			ev := got.Event
			switch ev.Type {
			case "response.created":
				// 新响应创建才证明排队输入进入了续接；保留原响应的全部协议项目与追加原话。
				if responseID != "" {
					if completed == nil || len(queued) == 0 {
						return modelResult{}, fmt.Errorf("服务返回无法关联的 steering 续接")
					}
					if chain != nil {
						*chain = append(*chain, completed.Message, ChatMessage{Role: "user", Content: "真人追加的消息：" + asJSON(queued)})
					}
					applied = append(applied, queued...)
					m.Store.finishUsageCall(call)
					call, queuedCall = queuedCall, usageReservation{}
					queued = nil
					completed = nil
				}
				responseID = ev.Response.ID
				active = true
				if err := steer(); err != nil {
					return modelResult{}, err
				}
			case "response.completed", "response.incomplete":
				active = false
				result, err := decodeResponse(ev.Response)
				if err != nil {
					return modelResult{}, err
				}
				result.Message.OutputTokens = result.Usage.OutputTokens
				if result.Usage.InputTokens+result.Usage.OutputTokens > 0 {
					actual, err := result.Usage.costMicros(price)
					if err != nil {
						return modelResult{}, err
					}
					if err = m.Store.settleCall(call, actual, result.Usage, result.usageOutcome(ctx)); err != nil {
						return modelResult{}, &modelAccountingError{err}
					}
				}
				completed = &result
				if input.Applied == nil {
					input.Applied = map[string]bool{}
				}
				for _, msg := range applied {
					input.Applied[msg.ID] = true
				}
				if len(queued) == 0 {
					return result, nil
				}
				// 工具调用必须交由客户端执行，不能等待一个尚缺 tool output 的自动响应。
				// 此时后续尚未启动，释放它的预留；原工具响应仍完整返回，不重演已经完成的工具。
				if len(result.Message.ToolCalls) > 0 {
					if err = m.Store.settleCall(queuedCall, 0, responseUsage{}, "released"); err != nil {
						return modelResult{}, err
					}
					return result, nil
				}
				if result.Incomplete && ev.Response.IncompleteDetails.Reason != "steered" {
					return result, fmt.Errorf("模型本次未完成输出")
				}
			case "response.steer.failed":
				if queuedCall.id != 0 {
					if err = m.Store.settleCall(queuedCall, 0, responseUsage{}, "released"); err != nil {
						return modelResult{}, err
					}
				}
				queued, queuedCall = nil, usageReservation{}
				if completed != nil {
					return *completed, nil
				}
			case "response.failed", "error":
				return modelResult{}, fmt.Errorf("Responses WebSocket 返回失败：%s", ev.Error.Code)
			}
		}
	}
}
