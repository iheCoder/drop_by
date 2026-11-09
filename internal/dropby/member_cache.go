package dropby

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"time"
)

const (
	shortCacheLifetime = 30 * time.Minute
	longCacheLifetime  = 12 * time.Hour
)

type memberCacheKey struct{}

type memberCacheScope struct {
	state  *memberCacheState
	workID string // 只有定时个人探索有长缓存；私聊只请求诊断。
}

// 仅保留请求位置的摘要，不保存另一份聊天正文。重启后重新观察两次独立探索。
type memberCacheState struct {
	workID     string
	first      [sha256.Size]byte
	lastAt     time.Time
	long       [sha256.Size]byte
	longUntil  time.Time
	model      string
	responseID string
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

type memberCacheRequest struct {
	next      memberCacheState
	workID    string
	at        time.Time
	longWrite bool
}

// 按模型实际收到的顺序比较。工具定义、模型和推理设置也参与比较；缓存标记
// 及末尾的当前现场不参与。上一轮历史若仍原样出现在这轮，便可复用它的旧位置。
func (r *memberCacheRequest) longBoundary(settings map[string]any, input []any, shortEnd int) (int, error) {
	if r.workID == "" {
		return -1, nil
	}
	state := &r.next
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	if err := encoder.Encode(settings); err != nil {
		return -1, err
	}
	var first [sha256.Size]byte
	previousEnd, longEnd := -1, -1
	for i := 0; i <= shortEnd; i++ {
		if err := encoder.Encode(input[i]); err != nil {
			return -1, err
		}
		// 摘要只会记录已有缓存线的位置。原生推理和压缩记录完整参与
		// 比较，但本身不可加标记，也不拆开它们另选位置。
		item, ok := input[i].(map[string]any)
		if !ok || (item["role"] != "user" && item["type"] != "function_call_output") {
			continue
		}
		var digest [sha256.Size]byte
		hash.Sum(digest[:0])
		if digest == state.first {
			previousEnd = i
		}
		if digest == state.long && r.at.Before(state.longUntil) {
			longEnd = i
		}
		if i == shortEnd {
			first = digest
		}
	}

	// 窗口整理或压缩改变开头时，旧标记随之失效。仍然匹配时，固定
	// 长缓存线直到这一段保留期结束，后续新工具结果沿用 30 分钟。
	if longEnd < 0 {
		state.long = [sha256.Size]byte{}
		state.longUntil = time.Time{}
	}
	if state.workID != r.workID {
		gap := r.at.Sub(state.lastAt)
		if longEnd < 0 && previousEnd >= 0 && gap >= shortCacheLifetime && gap < longCacheLifetime {
			longEnd = previousEnd
			state.long, state.longUntil = state.first, r.at.Add(longCacheLifetime)
		}
		state.workID, state.first = r.workID, first
	}
	r.longWrite = longEnd >= 0
	return longEnd, nil
}

type cacheDiagnostic struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Missed int    `json:"cache_missed_tokens"`
}

// 仅观察正常响应，诊断无额外生成。记录原因和数字即可，不保存请求正文。
func (r *memberCacheRequest) record(ctx context.Context, model string, response modelResult) {
	gap := r.at.Sub(r.next.lastAt)
	r.next.model, r.next.responseID = model, response.ID
	r.next.lastAt = r.at
	call, _ := ctx.Value(usageCallKey{}).(usageCall)
	u := response.Usage.InputDetails
	// 末尾变动本来就常见。只记录短时间内写入多于读回的失败对照。
	if d := response.CacheDiagnostic; d != nil && d.Type == "cache_miss" &&
		gap < shortCacheLifetime && u.CacheWriteTokens > u.CachedTokens {
		slog.Info("模型缓存对照未命中", "member", call.member, "room", call.room, "work", call.root,
			"model", model, "reason", d.Reason, "missed_tokens", d.Missed, "cached_tokens", u.CachedTokens)
	}
	if r.longWrite && u.WriteDetails != nil && u.WriteDetails.TwelveHours > 0 {
		slog.Info("个人探索写入长缓存", "member", call.member, "work", call.root,
			"cached_tokens", u.CachedTokens, "write_12h_tokens", u.WriteDetails.TwelveHours, "write_30m_tokens", u.WriteDetails.ThirtyMinutes)
	}
}
