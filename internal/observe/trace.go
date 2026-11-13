package observe

import (
	"context"
	"sync/atomic"
)

type traceKey struct{}

// Trace 挂在一次执行的上下文上。压缩、重试和工具通过它找到同一次调用，而不是靠时间对齐。
type Trace struct {
	log                  *Log
	RunID                string
	call, model, compact atomic.Value
	tool                 atomic.Value
	member, room         atomic.Value
}

func WithTrace(ctx context.Context, log *Log, runID string) context.Context {
	if log == nil || runID == "" {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, &Trace{log: log, RunID: runID})
}

// Branch 给并行的成员各自一份调用、模型和工具槽。它们仍写入同一次执行。
func Branch(ctx context.Context) context.Context {
	tr := Current(ctx)
	if tr == nil || tr.log == nil || tr.RunID == "" {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, &Trace{log: tr.log, RunID: tr.RunID})
}

func Current(ctx context.Context) *Trace {
	if ctx == nil {
		return nil
	}
	tr, _ := ctx.Value(traceKey{}).(*Trace)
	return tr
}

func (t *Trace) End(status, note string) {
	if t == nil || t.log == nil {
		return
	}
	t.log.WriteRunEnd(t.RunID, status, note)
}

func (t *Trace) SetCall(id string) {
	if t != nil {
		t.call.Store(id)
	}
}
func (t *Trace) Call() string {
	if t == nil {
		return ""
	}
	return load(&t.call)
}
func (t *Trace) SetModel(id string) {
	if t != nil {
		t.model.Store(id)
	}
}
func (t *Trace) Model() string {
	if t == nil {
		return ""
	}
	return load(&t.model)
}
func (t *Trace) SetCompact(id string) {
	if t != nil {
		t.compact.Store(id)
	}
}
func (t *Trace) Compact() string {
	if t == nil {
		return ""
	}
	return load(&t.compact)
}
func (t *Trace) SetTool(id string) {
	if t != nil {
		t.tool.Store(id)
	}
}
func (t *Trace) Tool() string {
	if t == nil {
		return ""
	}
	return load(&t.tool)
}
func (t *Trace) SetActor(member, room string) {
	if t == nil {
		return
	}
	t.member.Store(member)
	t.room.Store(room)
}

func (t *Trace) Write(s Step) {
	if t == nil || t.log == nil {
		return
	}
	if s.RunID == "" {
		s.RunID = t.RunID
	}
	if s.MemberID == "" {
		s.MemberID = load(&t.member)
	}
	if s.ConversationID == "" {
		s.ConversationID = load(&t.room)
	}
	t.log.WriteStep(s)
}

func load(v *atomic.Value) string {
	if v.Load() == nil {
		return ""
	}
	s, _ := v.Load().(string)
	return s
}
