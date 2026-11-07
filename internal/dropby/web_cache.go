package dropby

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"
)

const webCacheTTL = 5 * time.Minute

// 只缓存公开网页工具的成功结果。每个成员仍经过 Execute 留下自己的行动记录；
// 这里复用的是服务返回的材料，不是成员的分析，也不缓存本地状态或写操作。
type webCacheEntry struct {
	done      chan struct{}
	completed time.Time
	result    map[string]any
	err       error
}
type webResultCache struct {
	mu      sync.Mutex
	entries map[[32]byte]*webCacheEntry
}

func (t *Toolbox) cachedWeb(ctx context.Context, room, path string, body any, fetch func() (map[string]any, error)) (any, error) {
	// JSON 对象键顺序不影响身份；字符串、URL 查询参数原样保留。凭据只参与哈希，
	// 防止换服务或换账户后复用旧结果，缓存不跨聊天共享。
	key := sha256.Sum256([]byte(asJSON([]any{room, t.Config.SearchURL, t.Config.SearchKey, path, body})))
	cache := &t.webCache
	cache.mu.Lock()
	if cache.entries == nil {
		cache.entries = make(map[[32]byte]*webCacheEntry)
	}
	for k, entry := range cache.entries {
		if !entry.completed.IsZero() && time.Since(entry.completed) >= webCacheTTL {
			delete(cache.entries, k)
		}
	}
	if entry := cache.entries[key]; entry != nil {
		cache.mu.Unlock()
		// 等待者取消只结束自己的等待，不取消发起者的请求。发起者失败时所有等待者
		// 都收到该次错误，错误不会留下缓存，之后的新调用可以重新尝试。
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-entry.done:
			return cachedWebResult(entry, true)
		}
	}
	entry := &webCacheEntry{done: make(chan struct{})}
	cache.entries[key] = entry
	cache.mu.Unlock()

	// 只有真正发出请求的路径进入预算预留与结算，命中及并发等待都不重复收费。
	result, err := fetch()
	cache.mu.Lock()
	entry.result, entry.err, entry.completed = result, err, time.Now()
	if err != nil {
		delete(cache.entries, key)
	}
	close(entry.done)
	cache.mu.Unlock()
	return cachedWebResult(entry, false)
}

func cachedWebResult(entry *webCacheEntry, reused bool) (any, error) {
	if entry.err != nil {
		return nil, entry.err
	}
	// 独立包装本次调用的元信息，不能修改其他成员共享的原结果。
	out := make(map[string]any, len(entry.result)+2)
	for k, v := range entry.result {
		out[k] = v
	}
	out["retrieved_at"] = entry.completed.UTC().Format(time.RFC3339Nano)
	out["reused"] = reused
	return out, nil
}
