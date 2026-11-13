package dropby

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"drop_by/internal/observe"
)

// 观测写入全部走内存队列。这里的函数在没有观测库时直接返回，不改变业务结果。

func note(ctx context.Context, s observe.Step) {
	if tr := observe.Current(ctx); tr != nil {
		tr.Write(s)
	}
}

func scrub(text, secret string) string {
	if text == "" || secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "[credential]")
}

func billing(err error, settled, released bool, actual, reserved int64) (status string, cost, hold *int64) {
	if settled {
		v := actual
		return "settled", &v, nil
	}
	if released {
		zero := int64(0)
		return "released", &zero, nil
	}
	var failure *requestFailure
	if errors.As(err, &failure) && failure.NotSent {
		return "none", nil, nil
	}
	if reserved > 0 {
		v := reserved
		return "unknown", nil, &v
	}
	if err != nil {
		return "unknown", nil, nil
	}
	return "none", nil, nil
}

func heldMicros(call usageReservation, reserved int64) int64 {
	if call.id == 0 {
		return 0
	}
	return reserved
}

func callRef(call usageReservation) string {
	if call.id == 0 {
		return ""
	}
	return strconv.FormatInt(call.id, 10)
}

func branchObservation(ctx context.Context) context.Context {
	return observe.Branch(ctx)
}

func noteActor(ctx context.Context, member, room string) {
	if tr := observe.Current(ctx); tr != nil {
		tr.SetActor(member, room)
	}
}

func beginCall(ctx context.Context) {
	if tr := observe.Current(ctx); tr != nil {
		tr.SetCall(observe.NewID("call"))
		tr.SetCompact("")
	}
}

func beginModelAttempt(ctx context.Context, modelName, input string, attempt int, retryOf, parent string) string {
	tr := observe.Current(ctx)
	if tr == nil {
		return ""
	}
	if parent == "" {
		parent = tr.Compact()
	}
	id := observe.NewID("step")
	tr.SetModel(id)
	tr.Write(observe.Step{
		ID: id, CallID: tr.Call(), ParentID: parent, RetryOf: retryOf,
		Kind: "model", Name: modelName, Attempt: attempt, Status: "started",
		Input: input, Summary: modelName,
	})
	return id
}

func finishModelAttempt(ctx context.Context, id string, msg ChatMessage, raw string, err error, settled bool, actual, reserved int64, call usageReservation, secret string) {
	if id == "" {
		return
	}
	status := "succeeded"
	if err != nil {
		status = "failed"
	}
	output := strings.TrimSpace(raw)
	if output == "" {
		output = modelObserved(msg, err)
	} else if err != nil {
		output += "\n" + err.Error()
	}
	costStatus, cost, hold := billing(err, settled, false, actual, heldMicros(call, reserved))
	note(ctx, observe.Step{
		ID: id, Status: status, Summary: status, Output: scrub(output, secret),
		CostStatus: costStatus, CostMicros: cost, ReservedMicros: hold, BusinessID: callRef(call),
	})
}

func modelObserved(msg ChatMessage, err error) string {
	if err != nil && msg.Content == "" && len(msg.ToolCalls) == 0 && len(msg.ResponseItems) == 0 {
		return err.Error()
	}
	body := map[string]any{"content": msg.Content, "tool_calls": msg.ToolCalls, "output": msg.ResponseItems}
	if err != nil {
		body["error"] = err.Error()
	}
	return asJSON(body)
}

func beginCompact(ctx context.Context, input string) string {
	tr := observe.Current(ctx)
	if tr == nil {
		return ""
	}
	id := observe.NewID("step")
	tr.SetCompact(id)
	tr.Write(observe.Step{
		ID: id, CallID: tr.Call(), Kind: "compact", Name: "compact", Status: "started",
		Input: input, Summary: "压缩了窗口",
	})
	return id
}

func noteInputCount(ctx context.Context, attempt int, retryOf, input, output string, err error) string {
	tr := observe.Current(ctx)
	if tr == nil {
		return ""
	}
	id := observe.NewID("step")
	status := "succeeded"
	if err != nil {
		status = "failed"
		if output == "" {
			output = err.Error()
		}
	}
	tr.Write(observe.Step{
		ID: id, CallID: tr.Call(), RetryOf: retryOf, Kind: "request", Name: "input_tokens",
		Attempt: attempt, Status: status, Input: input, Output: output, Summary: "输入计数",
	})
	return id
}

func finishCompact(ctx context.Context, id, output string, err error, settled bool, actual, reserved int64, call usageReservation, secret string) {
	if id == "" {
		return
	}
	status := "succeeded"
	if err != nil {
		status = "failed"
		if strings.TrimSpace(output) == "" {
			output = err.Error()
		} else {
			output += "\n" + err.Error()
		}
	}
	costStatus, cost, hold := billing(err, settled, false, actual, heldMicros(call, reserved))
	note(ctx, observe.Step{
		ID: id, Status: status, Summary: status, Output: scrub(output, secret),
		CostStatus: costStatus, CostMicros: cost, ReservedMicros: hold, BusinessID: callRef(call),
	})
}

func noteConnection(ctx context.Context, attempt int, retryOf string, err error, secret string) string {
	tr := observe.Current(ctx)
	if tr == nil {
		return ""
	}
	id := observe.NewID("step")
	status := "succeeded"
	output := ""
	if err != nil {
		status = "failed"
		output = scrub(err.Error(), secret)
	}
	parent := tr.Compact()
	tr.Write(observe.Step{
		ID: id, CallID: tr.Call(), ParentID: parent, RetryOf: retryOf,
		Kind: "request", Name: "websocket", Attempt: attempt, Status: status,
		Output: output, Summary: "建立连接",
	})
	return id
}

func finishSteering(ctx context.Context, id, status, output string) {
	if id == "" {
		return
	}
	note(ctx, observe.Step{ID: id, Status: status, Output: output, Summary: status})
}

func noteSteering(ctx context.Context, input, status, output string) string {
	tr := observe.Current(ctx)
	if tr == nil {
		return ""
	}
	id := observe.NewID("step")
	tr.Write(observe.Step{
		ID: id, CallID: tr.Call(), ParentID: tr.Model(), Kind: "steering", Name: "steer",
		Status: status, Input: input, Output: output, Summary: "中途追加",
	})
	return id
}

func noteParticipation(ctx context.Context, member, action, full string) {
	note(ctx, observe.Step{
		ID: observe.NewID("step"), Kind: "participation", Name: member, Status: action,
		Summary: full, MemberID: member,
	})
}

func noteNotCalled(ctx context.Context, member string) {
	note(ctx, observe.Step{
		ID: observe.NewID("step"), Kind: "participation", Name: member, Status: "not_called",
		Summary: "这次没有叫到", MemberID: member,
	})
}

func noteSentMessage(ctx context.Context, author, source, target, messageID string, reused bool) {
	status, summary := "sent", "已写入"
	if reused {
		status, summary = "reused", "这条消息之前已经写入，没有再发一次"
	}
	note(ctx, observe.Step{
		ID: observe.NewID("step"), Kind: "message", Name: "send_message", Status: status,
		Summary: summary, BusinessID: messageID, MemberID: author,
		ConversationID: source, TargetID: target, ParentID: modelParent(ctx),
	})
}

func modelParent(ctx context.Context) string {
	if tr := observe.Current(ctx); tr != nil {
		return tr.Model()
	}
	return ""
}

func noteReusedTool(ctx context.Context, businessID, name, args, result string) {
	tr := observe.Current(ctx)
	if tr == nil {
		return
	}
	tr.Write(observe.Step{
		ID: observe.NewID("step"), CallID: tr.Call(), ParentID: tr.Model(),
		Kind: "tool", Name: name, Status: "reused", BusinessID: businessID,
		Input: args, Output: result, Summary: "复用已有结果，没有再执行",
	})
}

func noteBlockedTool(ctx context.Context, name, args, summary string) {
	tr := observe.Current(ctx)
	if tr == nil {
		return
	}
	tr.Write(observe.Step{
		ID: observe.NewID("step"), CallID: tr.Call(), ParentID: tr.Model(),
		Kind: "tool", Name: name, Status: "blocked", Input: args, Summary: summary,
	})
}

func beginTool(ctx context.Context, businessID, name, args string, resuming bool) string {
	tr := observe.Current(ctx)
	if tr == nil {
		return ""
	}
	id := observe.NewID("step")
	tr.SetTool(id)
	summary := name
	status := "started"
	if resuming {
		summary = "接着上次未完成的执行"
		status = "resume"
	}
	tr.Write(observe.Step{
		ID: id, CallID: tr.Call(), ParentID: tr.Model(), Kind: "tool", Name: name,
		Status: status, BusinessID: businessID, Input: args, Summary: summary,
	})
	return id
}

func finishTool(ctx context.Context, id, status, output string) {
	if id == "" {
		return
	}
	note(ctx, observe.Step{ID: id, Status: status, Output: output, Summary: status})
}

func noteWebTry(ctx context.Context, name string, attempt int, retryOf, input, output string, err error, settled, released bool, actual, reserved int64, call usageReservation, secret string) string {
	tr := observe.Current(ctx)
	if tr == nil {
		return ""
	}
	id := observe.NewID("step")
	status := "succeeded"
	if err != nil {
		status = "failed"
		output = err.Error()
	}
	costStatus, cost, hold := billing(err, settled, released, actual, heldMicros(call, reserved))
	parent := tr.Tool()
	tr.Write(observe.Step{
		ID: id, CallID: tr.Call(), ParentID: parent, RetryOf: retryOf,
		Kind: "request", Name: name, Attempt: attempt, Status: status,
		Input: input, Output: scrub(output, secret), Summary: name,
		CostStatus: costStatus, CostMicros: cost, ReservedMicros: hold, BusinessID: callRef(call),
	})
	return id
}
