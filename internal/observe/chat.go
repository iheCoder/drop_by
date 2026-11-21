package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// 测光向聊天服务请求成员、会话和发出的消息，只向页面提供界面需要的字段。
// 执行记录和原始快照由观测库单独提供。
type chatMember struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Avatar     string `json:"avatar,omitempty"`
	AvatarIcon string `json:"avatar_icon,omitempty"`
}

type chatRoom struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	MemberIDs []string `json:"member_ids"`
}

type chatMessage struct {
	Seq       int64  `json:"seq,omitempty"`
	ID        string `json:"id"`
	AuthorID  string `json:"author_id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type chatReader struct {
	baseURL string
	client  *http.Client
}

func newChatReader(baseURL string) chatReader {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return chatReader{baseURL: baseURL, client: &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c chatReader) read(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errChatMissing
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("读取聊天服务失败 (%d)", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

var errChatMissing = errors.New("聊天不存在")

func (c chatReader) catalog(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Members       []chatMember `json:"members"`
		Conversations []chatRoom   `json:"conversations"`
	}
	if err := c.read(r.Context(), "/api/state", &data); err != nil {
		http.Error(w, "暂时读不到成员和会话", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (c chatReader) messages(w http.ResponseWriter, r *http.Request) {
	var data struct {
		Messages   []chatMessage `json:"messages"`
		HasEarlier bool          `json:"has_earlier"`
	}
	path := "/api/conversations/" + url.PathEscape(r.PathValue("id")) + "?activity=paged"
	if before := r.URL.Query().Get("before"); before != "" {
		path += "&before=" + url.QueryEscape(before)
	}
	if err := c.read(r.Context(), path, &data); err != nil {
		if errors.Is(err, errChatMissing) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "暂时读不到这段会话的消息", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (c chatReader) costs(w http.ResponseWriter, r *http.Request) {
	path := "/api/usage/scope"
	if q := r.URL.RawQuery; q != "" {
		path += "?" + q
	}
	var data json.RawMessage
	if err := c.read(r.Context(), path, &data); err != nil {
		http.Error(w, "暂时读不到账本，不能把已加载的调用当成完整费用", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, data)
}
