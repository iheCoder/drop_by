package dropby

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type API struct{ engine *Engine }

func NewHandler(engine *Engine) http.Handler {
	a := &API{engine: engine}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("POST /api/member-questions", a.questions)
	mux.HandleFunc("POST /api/members", a.createMember)
	mux.HandleFunc("POST /api/conversations", a.createConversation)
	mux.HandleFunc("GET /api/conversations/{id}", a.conversation)
	mux.HandleFunc("POST /api/conversations/{id}/messages", a.sendMessage)
	mux.HandleFunc("GET /api/artifacts/{id}", a.artifact)
	mux.HandleFunc("GET /api/tool-runs/{id}", a.toolRun)
	mux.HandleFunc("GET /api/events", a.events)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	root, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServer(http.FS(root)))
	return localOnly(mux)
}

func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 回环监听外，再检查 Host 和 Origin，阻止其他网站借用户浏览器调用本机 API。
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
			http.Error(w, "仅允许本机访问", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "不允许跨站请求", http.StatusForbidden)
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "需要 JSON 请求", http.StatusUnsupportedMediaType)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func readBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 100<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		respond(w, 400, map[string]string{"error": "请求内容无效或超过大小限制"})
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		respond(w, 400, map[string]string{"error": "请求只能包含一个 JSON 对象"})
		return false
	}
	return true
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func (a *API) fail(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, ErrBudget) {
		status = http.StatusTooManyRequests
	}
	slog.Warn("请求未完成", "error", err)
	respond(w, status, map[string]string{"error": err.Error()})
}

func (a *API) state(w http.ResponseWriter, r *http.Request) {
	members, err := a.engine.Store.Members()
	if err != nil {
		a.fail(w, err)
		return
	}
	rooms, err := a.engine.Store.Conversations()
	if err != nil {
		a.fail(w, err)
		return
	}
	usage, err := a.engine.Store.Usage()
	if err != nil {
		a.fail(w, err)
		return
	}
	a.engine.mu.Lock()
	active := []string{}
	for id := range a.engine.running {
		active = append(active, id)
	}
	a.engine.mu.Unlock()
	respond(w, 200, map[string]any{"members": members, "conversations": rooms, "active_rooms": active, "usage": usage, "model_ready": a.engine.Config.APIKey != "", "search_ready": a.engine.Config.SearchKey != "", "model": a.engine.Config.Model, "daily_budget_micros": a.engine.Config.DailyBudgetMicros, "daily_calls": a.engine.Config.DailyCalls})
}
func (a *API) questions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Direction string `json:"direction"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if len([]rune(strings.TrimSpace(in.Direction))) < 2 || len([]rune(in.Direction)) > 2000 {
		respond(w, 400, map[string]string{"error": "请用 2 到 2000 个字描述成员的大方向"})
		return
	}
	questions, err := CreateQuestions(r.Context(), a.engine.Model, in.Direction)
	if err != nil {
		a.fail(w, err)
		return
	}
	respond(w, 200, map[string]any{"questions": questions})
}
func (a *API) createMember(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string     `json:"name"`
		Direction string     `json:"direction"`
		Color     string     `json:"color"`
		Questions []Question `json:"questions"`
		Answers   []string   `json:"answers"`
	}
	if !readBody(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Direction = strings.TrimSpace(in.Direction)
	if in.Name == "" || len([]rune(in.Name)) > 24 || in.Direction == "" || len([]rune(in.Direction)) > 2000 || len(in.Questions) < 2 || len(in.Questions) > 5 || len(in.Answers) != len(in.Questions) {
		respond(w, 400, map[string]string{"error": "请填写名字、大方向，并完成情境问题"})
		return
	}
	for _, answer := range in.Answers {
		if strings.TrimSpace(answer) == "" || len([]rune(answer)) > 2000 {
			respond(w, 400, map[string]string{"error": "请回答每一个情境问题（每题不超过 2000 字）"})
			return
		}
	}
	if !contains([]string{"jade", "apricot", "blue", "rose", "violet", "moss"}, in.Color) {
		in.Color = "jade"
	}
	m, err := CompleteMember(r.Context(), a.engine.Model, Member{Name: in.Name, Direction: in.Direction, Color: in.Color, Questions: in.Questions, Answers: in.Answers})
	if err != nil {
		a.fail(w, err)
		return
	}
	if err = a.engine.Store.AddMember(m); err != nil {
		a.fail(w, err)
		return
	}
	a.engine.Hub.Publish("")
	respond(w, 201, m)
}
func (a *API) createConversation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string   `json:"name"`
		Kind      string   `json:"kind"`
		MemberIDs []string `json:"member_ids"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if in.Kind != "group" && in.Kind != "direct" {
		respond(w, 400, map[string]string{"error": "聊天类型无效"})
		return
	}
	if len(in.MemberIDs) == 0 || len(in.MemberIDs) > 8 || (in.Kind == "direct" && len(in.MemberIDs) != 1) {
		respond(w, 400, map[string]string{"error": "请为群选择 1 到 8 位成员，私聊只选择一位"})
		return
	}
	seen := map[string]bool{}
	for _, id := range in.MemberIDs {
		if seen[id] {
			respond(w, 400, map[string]string{"error": "成员不能重复"})
			return
		}
		seen[id] = true
		m, err := a.engine.Store.Member(id)
		if err != nil {
			respond(w, 400, map[string]string{"error": "所选成员不存在"})
			return
		}
		if in.Kind == "direct" {
			in.Name = m.Name
		}
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = "随便聊聊"
	}
	if len([]rune(in.Name)) > 80 {
		respond(w, 400, map[string]string{"error": "聊天名称太长"})
		return
	}
	c, err := a.engine.Store.CreateConversation(in.Name, in.Kind, in.MemberIDs, a.engine.Config.ExploreInterval)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.engine.Hub.Publish(c.ID)
	respond(w, 201, c)
}
func (a *API) conversation(w http.ResponseWriter, r *http.Request) {
	c, err := a.engine.Store.Conversation(r.PathValue("id"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "聊天不存在"})
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	messages, err := a.engine.Store.Messages(c.ID, before, 60)
	if err != nil {
		a.fail(w, err)
		return
	}
	jobs, err := a.engine.Store.Works(c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	artifacts, err := a.engine.Store.Artifacts(c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	respond(w, 200, map[string]any{"conversation": c, "messages": messages, "work": jobs, "artifacts": artifacts, "has_earlier": len(messages) == 60})
}
func (a *API) sendMessage(w http.ResponseWriter, r *http.Request) {
	c, err := a.engine.Store.Conversation(r.PathValue("id"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "聊天不存在"})
		return
	}
	var in struct {
		Body     string   `json:"body"`
		ReplyTo  string   `json:"reply_to"`
		Mentions []string `json:"mentions"`
	}
	if !readBody(w, r, &in) {
		return
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" || len([]rune(in.Body)) > 8000 {
		respond(w, 400, map[string]string{"error": "消息需要 1 到 8000 个字"})
		return
	}
	for _, id := range in.Mentions {
		if !contains(c.MemberIDs, id) {
			respond(w, 400, map[string]string{"error": "只能 @ 当前聊天中的成员"})
			return
		}
	}
	m := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: "user", Body: in.Body, ReplyTo: in.ReplyTo, Mentions: in.Mentions, CreatedAt: now()}
	if err = a.engine.Submit(m); err != nil {
		a.fail(w, err)
		return
	}
	respond(w, 201, m)
}
func (a *API) artifact(w http.ResponseWriter, r *http.Request) {
	versions, err := a.engine.Store.ArtifactVersions(r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	if len(versions) == 0 {
		respond(w, 404, map[string]string{"error": "成果不存在"})
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+versions[0].ID+`.md"`)
		io.WriteString(w, versions[0].Content)
		return
	}
	respond(w, 200, map[string]any{"versions": versions})
}
func (a *API) toolRun(w http.ResponseWriter, r *http.Request) {
	var run ToolRun
	var args string
	err := a.engine.Store.db.QueryRow(`SELECT id,work_id,conversation_id,member_id,name,arguments,result,status,created_at FROM tool_runs WHERE id=?`, r.PathValue("id")).Scan(&run.ID, &run.WorkID, &run.ConversationID, &run.MemberID, &run.Name, &args, &run.Result, &run.Status, &run.CreatedAt)
	if err != nil {
		respond(w, 404, map[string]string{"error": "行动记录不存在"})
		return
	}
	run.Arguments = json.RawMessage(args)
	respond(w, 200, run)
}
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "无法建立消息连接", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	stream, closeStream := a.engine.Hub.Subscribe()
	defer closeStream()
	io.WriteString(w, "retry: 2000\nevent: ready\ndata: {}\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case room := <-stream:
			fmt.Fprintf(w, "event: change\ndata: %s\n\n", asJSON(map[string]string{"conversation_id": room}))
			flusher.Flush()
		case <-heartbeat.C:
			io.WriteString(w, ": connected\n\n")
			flusher.Flush()
		}
	}
}
