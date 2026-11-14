package dropby

import (
	"context"
	"log/slog"

	"drop_by/internal/dropby/model"
)

type memberCacheState struct{ value any }

type memberCacheKey struct{}

type memberCacheScope struct {
	state  *memberCacheState
	workID string // 只有定时个人探索有长缓存；私聊只请求诊断。
}

func (e *Engine) withMemberCache(ctx context.Context, member, room, workID string) context.Context {
	gate := e.gate(member)
	if gate.requestCaches == nil {
		gate.requestCaches = make(map[string]*memberCacheState)
	}
	state := gate.requestCaches[room]
	if state == nil {
		state = &memberCacheState{}
		gate.requestCaches[room] = state
	}
	return context.WithValue(ctx, memberCacheKey{}, memberCacheScope{state: state, workID: workID})
}

// 仅观察正常响应，诊断无额外生成。记录原因和数字即可，不保存请求正文。
func recordCache(ctx context.Context, observation model.CacheObservation, modelName string) {
	if observation.Message == "" {
		return
	}
	call, _ := ctx.Value(usageCallKey{}).(usageCall)
	fields := []any{"member", call.member, "room", call.room, "work", call.root, "model", modelName}
	slog.Info(observation.Message, append(fields, observation.Fields...)...)
}
