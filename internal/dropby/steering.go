package dropby

import (
	"context"
	"encoding/json"

	"drop_by/internal/dropby/model"
)

type steeringKey struct{}
type personalSinceKey struct{}
type steeringInput = model.SteeringInput

// 只将真人追加内容送入 steering。事件只是唤醒提示，SQLite 才是可靠来源，
// 因而连接失败或通知合并都不会消耗未读位置；同伴发言仍在普通行动边界接续。
func (e *Engine) completeMemberModel(ctx context.Context, c Conversation, seen *int64, messages *[]ChatMessage, defs []ToolDefinition) (ChatMessage, error) {
	if !e.Config.Models.route(sceneFromContext(ctx)).steeringEnabled() {
		return completeModel(ctx, e.Model, messages, defs, true)
	}
	events, unsubscribe := e.Hub.Subscribe()
	defer unsubscribe()
	input := &steeringInput{Applied: map[string]bool{}, Events: events, Room: c.ID, Read: func() ([]model.Update, error) {
		var all []Message
		var err error
		if c.ID == "" {
			since, _ := ctx.Value(personalSinceKey{}).(string)
			all, err = e.Store.personalMessages(c.MemberIDs[0], *seen, since)
		} else {
			all, err = e.Store.messagesAfter(c.ID, *seen, 0, -1)
		}
		var human []model.Update
		for _, msg := range all {
			if msg.AuthorID == "user" {
				human = append(human, model.Update{ID: msg.ID, Data: json.RawMessage(asJSON(msg))})
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

// 连接如何续接由供应商决定。每一次可能计费的响应仍由应用预留、结算
// 和记录，供应商包只接收这四种操作，不依赖 SQLite 或观测库。
func (m *modelCall) steeringHooks(ctx context.Context) model.SteeringHooks {
	var previous string
	return model.SteeringHooks{
		Reserve: func(inputTokens int) (model.SteeringCall, error) {
			reserve := m.reserveMicros(inputTokens, m.maxOutputTokens)
			call, err := m.Store.reserveCall(ctx, m.Config, reserve, m.Provider, m.Model, "")
			if err != nil {
				return nil, &modelAccountingError{err}
			}
			return &steeringBill{model: m, ctx: ctx, call: call, reserve: reserve}, nil
		},
		Connection: func(attempt int, err error) {
			previous = noteConnection(ctx, attempt, previous, err, m.apiKey)
		},
		SendUpdate: func(body string, err error) string {
			if err != nil {
				return noteSteering(ctx, body, "failed", scrub(err.Error(), m.apiKey))
			}
			return noteSteering(ctx, body, "started", "")
		},
		FinishUpdate: func(id string, accepted bool) {
			if accepted {
				finishSteering(ctx, id, "accepted", "服务器已接下这次追加")
				return
			}
			finishSteering(ctx, id, "failed", "服务器没有接受这次追加")
		},
	}
}

type steeringBill struct {
	model   *modelCall
	ctx     context.Context
	call    usageReservation
	step    string
	reserve int64
}

func (b *steeringBill) Start(body, parent string) {
	b.step = beginModelAttempt(b.ctx, b.model.Model, body, 1, "", parent)
}

func (b *steeringBill) Complete(result model.Result, failure error) (err error) {
	var actual int64
	settled := false
	defer func() {
		if err != nil {
			failure = err
		}
		finishModelAttempt(b.ctx, b.step, result.Message, result.Raw, failure, settled, actual, b.reserve, b.call, b.model.apiKey)
	}()
	if result.Usage.InputTokens+result.Usage.OutputTokens == 0 {
		return nil
	}
	actual, err = result.CostMicros, result.CostError
	if err != nil {
		return err
	}
	if err := b.model.Store.settleCall(b.call, actual, result.Usage, usageOutcome(b.ctx, result)); err != nil {
		return &modelAccountingError{err}
	}
	settled = true
	return nil
}

func (b *steeringBill) Release() error {
	return b.model.Store.settleCall(b.call, 0, responseUsage{}, "released")
}

func (b *steeringBill) Close() {
	b.model.Store.finishUsageCall(b.call)
}
