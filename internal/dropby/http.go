package dropby

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
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
	mux.HandleFunc("PATCH /api/profile", a.userProfile)
	mux.HandleFunc("PATCH /api/conversations/{id}", a.updateGroup)
	mux.HandleFunc("POST /api/member-questions", a.questions)
	mux.HandleFunc("POST /api/members", a.createMember)
	mux.HandleFunc("PATCH /api/members/{id}", a.updateMember)
	mux.HandleFunc("POST /api/members/{id}/question", a.memberQuestion)
	mux.HandleFunc("POST /api/members/{id}/preferences", a.preferences)
	mux.HandleFunc("POST /api/conversations", a.createConversation)
	mux.HandleFunc("GET /api/conversations/{id}", a.conversation)
	mux.HandleFunc("PATCH /api/conversations/{id}/coordinator", a.coordinator)
	mux.HandleFunc("POST /api/conversations/{id}/messages", a.sendMessage)
	mux.HandleFunc("GET /api/artifacts/{id}", a.artifact)
	mux.HandleFunc("GET /api/artifacts/{id}/preview", a.artifactPreview)
	mux.HandleFunc("GET /api/artifacts/{id}/download", a.artifactDownload)
	mux.HandleFunc("GET /api/tool-runs/{id}", a.toolRun)
	mux.HandleFunc("GET /api/events", a.events)
	mux.HandleFunc("POST /api/work/{id}/resume", a.resumeWork)
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
	activities := []Activity{}
	for id := range a.engine.running {
		if !strings.HasPrefix(id, "member:") {
			active = append(active, id)
		}
	}
	for _, activity := range a.engine.activities {
		activities = append(activities, activity)
	}
	a.engine.mu.Unlock()
	profile, err := a.engine.Store.UserProfile()
	if err != nil {
		a.fail(w, err)
		return
	}
	focus, err := a.engine.Store.personalFocuses()
	if err != nil {
		a.fail(w, err)
		return
	}
	respond(w, 200, map[string]any{"profile": profile, "personal_focus": focus, "budget_blocked": a.engine.Store.budgetBlocked(a.engine.Config, usage), "members": members, "conversations": rooms, "active_rooms": active, "activities": activities, "usage": usage, "model_ready": a.engine.Config.APIKey != "", "search_ready": a.engine.Config.SearchKey != "", "model": a.engine.Config.Model, "chat_model": a.engine.Config.ChatModel, "weekly_budget_micros": a.engine.Config.weeklyBudgetAt(time.Now()), "explore_interval_minutes": int(a.engine.Config.ExploreInterval.Minutes())})
}
func (a *API) questions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Direction string `json:"direction"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if len([]rune(in.Direction)) > 2000 {
		respond(w, 400, map[string]string{"error": "偏好描述不超过 2000 字"})
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
		Name       string     `json:"name"`
		Direction  string     `json:"direction"`
		Color      string     `json:"color"`
		Avatar     string     `json:"avatar"`
		AvatarIcon string     `json:"avatar_icon"`
		Questions  []Question `json:"questions"`
		Answers    []string   `json:"answers"`
	}
	if !readBody(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Direction = strings.TrimSpace(in.Direction)
	if in.Name == "" || len([]rune(in.Name)) > 24 || len([]rune(in.Direction)) > 2000 || len(in.Questions) > 5 || len(in.Answers) != len(in.Questions) {
		respond(w, 400, map[string]string{"error": "请填写 1 到 24 个字的名字，其他信息可以稍后再补充"})
		return
	}
	if err := validateAvatar(in.Avatar); err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if in.AvatarIcon != "" && !contains(avatarIcons, in.AvatarIcon) {
		respond(w, 400, map[string]string{"error": "请选择已有头像图案"})
		return
	}
	if in.AvatarIcon == "" {
		in.AvatarIcon = "cat"
	}
	for _, answer := range in.Answers {
		if len([]rune(answer)) > 2000 {
			respond(w, 400, map[string]string{"error": "每个回答不超过 2000 字，也可以跳过"})
			return
		}
	}
	if !contains([]string{"jade", "apricot", "blue", "rose", "violet", "moss"}, in.Color) {
		in.Color = "jade"
	}
	// 创建身份不需要等待模型、回答问卷或消耗预算。未知偏好保持未知，
	// 用户在聊天或自愿补充偏好时再逐步认识；默认提供产品现有的安全工具。
	m := Member{ID: newID("member"), Name: in.Name, Direction: in.Direction, Color: in.Color, Avatar: in.Avatar, AvatarIcon: in.AvatarIcon, CreatedAt: now(), Questions: in.Questions, Answers: in.Answers, Tools: []string{"search", "read_page", "save_artifact"}, Profile: "性格和兴趣尚未设定，可在交流中逐步形成。"}
	if in.Direction != "" {
		m.Profile = "用户希望的起点：" + in.Direction + "\n具体判断可随真实经历变化。"
	}
	if len(in.Answers) > 0 {
		m.Profile += "\n用户自愿提供的情境偏好：" + asJSON(map[string]any{"questions": in.Questions, "answers": in.Answers})
	}
	if err := a.engine.Store.AddMember(m); err != nil {
		a.fail(w, err)
		return
	}
	a.engine.Hub.Publish("")
	respond(w, 201, m)
}

// 头像只允许小型本地栅格图片，避免远程追踪地址或可执行的 SVG 混入聊天。
func validateAvatar(raw string) error {
	if raw == "" {
		return nil
	}
	prefix, encoded, ok := strings.Cut(raw, ",")
	if !ok || (prefix != "data:image/png;base64" && prefix != "data:image/jpeg;base64") {
		return fmt.Errorf("头像请使用 PNG 或 JPEG 图片")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) > 64<<10 {
		return fmt.Errorf("头像图片过大或无效")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width > 512 || cfg.Height > 512 {
		return fmt.Errorf("头像需要是有效的小尺寸图片")
	}
	return nil
}

func (a *API) preferences(w http.ResponseWriter, r *http.Request) {
	m, err := a.engine.Store.Member(r.PathValue("id"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "成员不存在"})
		return
	}
	var in struct {
		Question Question `json:"question"`
		Answer   string   `json:"answer"`
	}
	if !readBody(w, r, &in) {
		return
	}
	in.Answer = strings.TrimSpace(in.Answer)
	if in.Answer == "" {
		respond(w, 200, m)
		return
	} // 跳过不修改身份，不触发模型。
	if len([]rune(in.Answer)) > 2000 || len([]rune(in.Question.Text)) > 500 || len(in.Question.Options) > 6 {
		respond(w, 400, map[string]string{"error": "偏好内容过长"})
		return
	}
	m.Questions = []Question{in.Question}
	m.Answers = []string{in.Answer}
	previousProfile := m.Profile
	// 旧 profile 一并交给模型，只合并这次明确提供的偏好，保留同一身份及经历。
	m, err = CompleteMember(r.Context(), a.engine.Model, m)
	if err != nil {
		a.fail(w, err)
		return
	}
	// 兼容旧界面的接口也不能整份覆盖等待期间的新名字和头像。人物说明已被显式
	// 修改时拒绝迟到的合并结果，避免旧问题答案悄悄撤销刚保存的新设定。
	tx, err := a.engine.Store.db.Begin()
	if err != nil {
		a.fail(w, err)
		return
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRow(`SELECT data FROM members WHERE id=?`, m.ID).Scan(&raw); err != nil {
		a.fail(w, err)
		return
	}
	var current Member
	if err = json.Unmarshal([]byte(raw), &current); err != nil {
		a.fail(w, err)
		return
	}
	if current.Profile != previousProfile {
		respond(w, http.StatusConflict, map[string]string{"error": "人物说明刚刚已修改，请基于最新说明继续聊偏好"})
		return
	}
	current.Profile, current.Tools, current.Questions, current.Answers = m.Profile, m.Tools, m.Questions, m.Answers
	if _, err = tx.Exec(`UPDATE members SET data=? WHERE id=?`, asJSON(current), current.ID); err != nil {
		a.fail(w, err)
		return
	}
	if _, err = tx.Exec(`UPDATE conversations SET revision=revision+1 WHERE id IN (SELECT conversation_id FROM participants WHERE member_id=?)`, current.ID); err != nil {
		a.fail(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		a.fail(w, err)
		return
	}
	a.engine.Hub.Publish("")
	respond(w, 200, current)
}

func (a *API) createConversation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string   `json:"name"`
		Avatar    string   `json:"avatar"`
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
	if len(in.MemberIDs) == 0 || len(in.MemberIDs) > 12 || (in.Kind == "direct" && len(in.MemberIDs) != 1) {
		respond(w, 400, map[string]string{"error": "请为群选择 1 到 12 位成员，私聊只选择一位"})
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
	if len([]rune(in.Name)) > 120 {
		respond(w, 400, map[string]string{"error": "聊天名称太长"})
		return
	}
	if err := validateAvatar(in.Avatar); err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	c, err := a.engine.Store.CreateConversation(in.Name, in.Kind, in.MemberIDs, a.engine.Config.ExploreInterval, in.Avatar)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err = a.engine.Store.EnsureWelcome(c); err != nil && !errors.Is(err, ErrStale) {
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
	decisions, err := a.engine.Store.Participations(c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	runs, err := a.engine.Store.ToolRuns(c.ID, "")
	if err != nil {
		a.fail(w, err)
		return
	}
	personalJobs, err := a.engine.Store.personalWorks(c.MemberIDs)
	if err != nil {
		a.fail(w, err)
		return
	}
	jobs = append(jobs, personalJobs...)
	for _, id := range c.MemberIDs {
		personalRuns, err := a.engine.Store.personalToolRuns(id)
		if err != nil {
			a.fail(w, err)
			return
		}
		runs = append(runs, personalRuns...)
	}
	// 展示文案从真实记录派生；执行指令和工具成功不能直接当作用户进度。
	// 完整结果仍按 ID 打开，避免 SSE 每次刷新重传网页或作品正文。
	for i := range jobs {
		display, err := a.engine.Store.workSummary(jobs[i])
		if err != nil {
			a.fail(w, err)
			return
		}
		jobs[i].Display = &display
	}
	for i := range runs {
		display, err := a.engine.Store.actionSummary(runs[i])
		if err != nil {
			a.fail(w, err)
			return
		}
		runs[i].Display = &display
		runs[i].Result = ""
	}
	memberStates, err := a.engine.Store.MemberStates(c.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	respond(w, 200, map[string]any{"conversation": c, "messages": messages, "work": jobs, "artifacts": artifacts, "participation": decisions, "tool_runs": runs, "member_states": memberStates, "has_earlier": len(messages) == 60})
}

func (a *API) coordinator(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MemberID string `json:"member_id"`
	}
	if !readBody(w, r, &in) {
		return
	}
	c, err := a.engine.Store.Conversation(r.PathValue("id"))
	if err != nil {
		respond(w, 404, map[string]string{"error": "聊天不存在"})
		return
	}
	// 修订号会让旧职责的副作用失效，并由现有 ErrStale 分支重读上下文。
	// 不以普通取消中断聊天，否则会把尚未回答的真人问题误标为已被新消息替代。
	updated, err := a.engine.Store.SetCoordinator(c, in.MemberID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.engine.Hub.Publish(c.ID)
	respond(w, 200, updated)
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
		// 保留旧下载地址；类型和版本选择使用统一的下载逻辑。
		a.artifactDownload(w, r)
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
	display, err := a.engine.Store.actionSummary(run)
	if err != nil {
		a.fail(w, err)
		return
	}
	run.Display = &display
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

// resumeWork 恢复原任务与已有行动现场，不用伪造一条用户聊天来“再催一次”。
// 只能恢复真实失败/暂停的工作；已完成、已取消或已被新消息替代的不重放。
func (a *API) resumeWork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var room, status, kind string
	if err := a.engine.Store.db.QueryRow("SELECT coalesce(conversation_id,''),status,kind FROM work WHERE id=?", id).Scan(&room, &status, &kind); err != nil {
		respond(w, 404, map[string]string{"error": "找不到这项工作"})
		return
	}
	if status != "failed" && status != "paused" {
		respond(w, 409, map[string]string{"error": "这项工作当前不需要恢复"})
		return
	}
	var c Conversation
	var err error
	if room != "" {
		c, err = a.engine.Store.Conversation(room)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if ((kind == "research" || kind == "revisit") && c.ResearchPaused) || (kind != "research" && c.DiscussionPaused) {
		respond(w, 409, map[string]string{"error": "请先恢复这个聊天中的相关活动"})
		return
	}
	result, err := a.engine.Store.db.Exec("UPDATE work SET status='pending',kind=CASE WHEN kind='chat' THEN 'followup' ELSE kind END,due_at=? WHERE id=? AND status IN ('failed','paused')", time.Now().UnixMilli(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	n, err := result.RowsAffected()
	if err != nil {
		a.fail(w, err)
		return
	}
	if n != 1 {
		respond(w, 409, map[string]string{"error": "工作状态已经改变"})
		return
	}
	a.engine.Hub.Publish(room)
	respond(w, 200, map[string]string{"status": "pending"})
}
