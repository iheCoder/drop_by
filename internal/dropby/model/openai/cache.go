package openai

import (
	"crypto/sha256"
	"encoding/json"
	"time"

	"drop_by/internal/dropby/model"
)

const (
	shortCacheLifetime = 30 * time.Minute
	longCacheLifetime  = 12 * time.Hour
)

// 仅保留请求位置的摘要，不保存另一份聊天正文。重启后重新观察两次独立探索。
type cacheState struct {
	workID     string
	first      [sha256.Size]byte
	lastAt     time.Time
	long       [sha256.Size]byte
	longUntil  time.Time
	source     string
	responseID string
}

type cacheRequest struct {
	next         cacheState
	workID       string
	at           time.Time
	longWrite    bool
	writeDetails *cacheWriteDetails
}

// 按模型实际收到的顺序比较。工具定义、模型和推理设置也参与比较；缓存标记
// 及末尾的当前现场不参与。上一轮历史若仍原样出现在这轮，便可复用它的旧位置。
func (r *cacheRequest) longBoundary(settings map[string]any, input []any, shortEnd int) (int, error) {
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

// 只在同一接入点和模型上比较缓存前缀与响应 ID。
func (m *Client) prepareCache(state *cacheState, workID string, at time.Time) *cacheRequest {
	req := &cacheRequest{workID: workID, at: at}
	if state.source == m.Source() {
		req.next = *state
	}
	return req
}

func (r *cacheRequest) record(state *cacheState, source string, response model.Result) model.CacheObservation {
	gap := r.at.Sub(r.next.lastAt)
	r.next.source, r.next.responseID = source, response.ID
	r.next.lastAt = r.at
	*state = r.next

	// 诊断解释与 TTL 用量字段都是 Responses 的约定。
	var diagnostic struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
		Missed int    `json:"cache_missed_tokens"`
	}
	u := response.Usage.InputDetails
	if gap < shortCacheLifetime && u.CacheWriteTokens > u.CachedTokens &&
		json.Unmarshal(response.CacheDiagnostic, &diagnostic) == nil && diagnostic.Type == "cache_miss" {
		return model.CacheObservation{Message: "模型缓存对照未命中",
			Fields: []any{"reason", diagnostic.Reason, "missed_tokens", diagnostic.Missed, "cached_tokens", u.CachedTokens}}
	}
	if d := r.writeDetails; r.longWrite && d != nil && d.TwelveHours > 0 {
		return model.CacheObservation{Message: "个人探索写入长缓存",
			Fields: []any{"cached_tokens", u.CachedTokens, "write_12h_tokens", d.TwelveHours, "write_30m_tokens", d.ThirtyMinutes}}
	}
	return model.CacheObservation{}
}

func (m *Client) BeginCache(previous any, workID string, at time.Time) {
	state, _ := previous.(cacheState)
	m.cache = m.prepareCache(&state, workID, at)
}

func (m *Client) FinishCache(result model.Result) (state any, observation model.CacheObservation) {
	if m.cache == nil {
		return nil, observation
	}
	var current cacheState
	observation = m.cache.record(&current, m.Source(), result)
	return current, observation
}
