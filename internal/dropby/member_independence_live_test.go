package dropby

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 在正式数据库的独立副本里接上用户实际失败的交流。保留人物、记忆和历史，
// 不替成员写台词，也不向正式群发送验收消息。副本须由调用者先用 SQLite backup 创建。
func TestLiveResumeActualConversation(t *testing.T) {
	dir := os.Getenv("DROPBY_INDEPENDENCE_LIVE_DIR")
	if dir == "" {
		t.Skip("需要独立验收数据库副本")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ModelReady() {
		t.Fatal("缺少真实模型配置")
	}
	cfg.DataDir = dir
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	room := "room_f670059fd0c6defd3fe45523"
	w, err := s.ReadWork(room, "work_29875a2d6511d232881df28e")
	if err != nil {
		t.Fatal(err)
	}
	w.Kind = "followup"
	before, _ := s.Messages(room, 0, 100)
	e := NewEngine(s, &HTTPModel{Store: s, Config: cfg, Client: &http.Client{Timeout: 90 * time.Second, Transport: independenceTrace{t}}}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runErr := e.process(ctx, w)
	after, _ := s.Messages(room, 0, 100)
	records, _ := s.Participations(room)
	usage, _ := s.Usage()
	result := map[string]any{"messages_before": before, "messages_after": after, "participation": records, "usage": usage}
	if runErr != nil {
		result["error"] = runErr.Error()
	}
	raw, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "report-"+time.Now().Format("150405")+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, m := range after {
		if len(before) == 0 || m.Seq > before[len(before)-1].Seq {
			t.Log(m.AuthorID, m.Body)
		}
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(after) <= len(before) {
		t.Fatal("没有接上原来的交流")
	}
	// 不把发言人数写成角色规则；这里只检查已保存的裴船正文没有被丢弃或重复。
	var count int
	for _, m := range after {
		if m.AuthorID == "member_3409520e0944d29270165a0f" && len(m.Body) > 0 {
			for _, old := range before {
				if m.ID == old.ID {
					goto oldMessage
				}
			}
			count++
		}
	oldMessage:
	}
	if count == 0 {
		t.Fatal("裴船原本的发言未恢复")
	}
}

// 仅报告验收请求的错误说明，不输出凭据或请求正文。
type independenceTrace struct{ t *testing.T }

func (tr independenceTrace) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := serviceTransport.RoundTrip(r)
	if err == nil && response.StatusCode >= 400 {
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(raw))
		var diagnostic struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &diagnostic) == nil {
			tr.t.Log("service", response.StatusCode, diagnostic.Error.Message)
		}
	}
	return response, err
}

// 使用同一组成员的另一个群，不携带旧群的自动方向和公共话术。
// 它用于观察新输出，不能替代原群恢复验收，也不预设每个人必须回答。
func TestLiveFreshConversationWithExistingMembers(t *testing.T) {
	dir := os.Getenv("DROPBY_INDEPENDENCE_LIVE_DIR")
	if dir == "" {
		t.Skip("需要独立验收数据库副本")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.CreateConversation("同一成员的新交流验收", "group", []string{"member_3409520e0944d29270165a0f", "member_91bbc0aaa87bdc53f31ae52f"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	msg := userMessage(c, "我们聊《后会友期》的纯稚，也就是白小纯和青稚。你们觉得这个有什么磕点呢？从各自的角度去说一说，他们为什么这次能这么火？一个不是艺人的也能够这么火呢？")
	if err := s.AddUserMessage(msg, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, c.ID)
	e := NewEngine(s, &HTTPModel{Store: s, Config: cfg, Client: &http.Client{Timeout: 90 * time.Second, Transport: independenceTrace{t}}}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runErr := e.process(ctx, w)
	messages, _ := s.Messages(c.ID, 0, 100)
	records, _ := s.Participations(c.ID)
	room, _ := s.Conversation(c.ID)
	report := map[string]any{"conversation": room, "messages": messages, "participation": records}
	if runErr != nil {
		report["error"] = runErr.Error()
	}
	raw, _ := json.MarshalIndent(report, "", "  ")
	os.WriteFile(filepath.Join(dir, "fresh-"+time.Now().Format("150405")+".json"), raw, 0600)
	for _, m := range messages {
		if m.AuthorID != "user" {
			t.Log(m.AuthorID, m.Body)
		}
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
}
