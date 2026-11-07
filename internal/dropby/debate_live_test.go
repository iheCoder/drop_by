package dropby

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 仅保留失败响应和请求正文用于验收诊断；不保存 URL、请求头或凭据。
type debateTransport struct{ dir string }

func (d debateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	response, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		raw, e := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(raw))
		if e == nil {
			b, _ := json.MarshalIndent(map[string]any{"request": json.RawMessage(body), "response": string(raw)}, "", "  ")
			os.WriteFile(filepath.Join(d.dir, newID("request-error")+".json"), b, 0600)
		}
	}
	return response, nil
}

// 使用真实成员资料和原始请求验收，不给角色预写台词或指定谁必须第一个推动。
// 明确开启才收费；独立数据库保留所有尝试、费用与模型判断，绝不写正式群聊。
func TestLiveDebateParticipation(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_DEBATE") != "1" {
		t.Skip("显式开启真实辩论验收")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("DROPBY_DEBATE_DIR")
	if dir == "" || cfg.APIKey == "" {
		t.Fatal("需要独立目录和模型凭据")
	}
	raw, err := os.ReadFile(os.Getenv("DROPBY_DEBATE_MEMBERS"))
	if err != nil {
		t.Fatal(err)
	}
	var originals []Member
	if err = json.Unmarshal(raw, &originals); err != nil {
		t.Fatal(err)
	}
	cfg.WeeklyBudgetMicros = 1500000
	cfg.AutonomousBudgetMicros = 1500000
	// 口头辩论不生成大作品，缩小单请求预留；实际累计账本不清零。
	cfg.MaxOutputTokens, cfg.DeepMaxOutputTokens = 2500, 7000
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	model := &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 3 * time.Minute, Transport: debateTransport{dir}}}
	scenarios := []string{"original", "already_open", "reserved_participants"}
	if name := os.Getenv("DROPBY_DEBATE_CASE"); name != "" {
		scenarios = []string{name}
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			people := map[string]Member{}
			var c Conversation
			var w Work
			resume := os.Getenv("DROPBY_DEBATE_RESUME")
			if resume != "" {
				// 预留额度不足时续接原工作，保留已发出的论点与累计账本。
				var room string
				if err = s.db.QueryRow(`SELECT conversation_id FROM work WHERE id=?`, resume).Scan(&room); err != nil {
					t.Fatal(err)
				}
				c, err = s.Conversation(room)
				if err != nil {
					t.Fatal(err)
				}
				for _, id := range c.MemberIDs {
					m, e := s.Member(id)
					if e != nil {
						t.Fatal(e)
					}
					people[m.Name] = m
				}
				w, err = s.ReadWork(room, resume)
				if err != nil {
					t.Fatal(err)
				}
				if w.Status != "failed" && w.Status != "waiting_budget" {
					t.Fatal("只续接未完成的试验工作")
				}
				if _, err = s.db.Exec(`UPDATE work SET status='pending' WHERE id=?`, w.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				ids := []string{}
				for _, original := range originals {
					m := original
					m.ID = newID("debate_member") // 不同试次不共享试验记忆。
					if scenario == "reserved_participants" && (m.Name == "裴船" || m.Name == "石粒") {
						m.Profile = "慢热，讨论时习惯先听别人，不太主动开场。对技术话题有自己的想法，愿意认真参与朋友组织的讨论。"
					}
					if err := s.AddMember(m); err != nil {
						t.Fatal(err)
					}
					people[m.Name] = m
					ids = append(ids, m.ID)
				}
				c, err = s.CreateConversation("辩论验收 · "+scenario, "group", ids, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				messages, _ := s.Messages(c.ID, 0, 100)
				work, _ := s.Works(c.ID)
				decisions, _ := s.Participations(c.ID)
				runs, _ := s.ToolRuns(c.ID, "")
				usage, _ := s.Usage()
				report := map[string]any{"scenario": scenario, "room": c.ID, "people": people, "messages": messages, "work": work, "participation": decisions, "tool_runs": runs, "cumulative_usage": usage}
				b, _ := json.MarshalIndent(report, "", "  ")
				suffix := ""
				if resume != "" {
					suffix = "-resumed-" + newID("attempt")
				}
				path := filepath.Join(dir, c.ID+suffix+".json")
				os.WriteFile(path, b, 0600)
				t.Logf("report=%s cumulative=$%.6f calls=%d", path, float64(usage.CostMicros)/1e6, usage.Calls)
				for _, m := range messages {
					t.Logf("%s: %s", m.AuthorID, m.Body)
				}
			}()
			e := NewEngine(s, model, cfg)
			if resume == "" {
				msg := userMessage(c, "@青雉 你来主持举行一下小型的辩论赛，让裴船和石粒进行辩论。主题是 AI 的发展是利大于弊程序员还是弊大于利呢？爱情导师不用参与")
				msg.Mentions = []string{people["青雉"].ID}
				if err = e.Submit(msg); err != nil {
					t.Fatal(err)
				}
				works, _ := s.DueWorks()
				for _, candidate := range works {
					if candidate.ConversationID == c.ID {
						w = candidate
						break
					}
				}
				if scenario == "already_open" {
					c, _ = s.Conversation(c.ID)
					// 重建用户遇到的开场现场；保留原话，没有“请不要沉默”等补救指令。
					if err = e.applyControl(ctx, c, w); err != nil {
						t.Fatal(err)
					}
					c, _ = s.Conversation(c.ID)
					opening := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: people["青雉"].ID, Body: "辩论赛开场！题目是：**AI 的发展对程序员而言，是利大于弊，还是弊大于利？**\n\n@裴船 @石粒，请你们先各自选一个想辩护的立场；如果恰好选了同一方，我们再协调。立场确定后，每人简短陈词，再互相回应，最后各自总结。", CreatedAt: now()}
					if err = s.Publish(opening, c.Revision); err != nil {
						t.Fatal(err)
					}
					w.Kind = "followup"
				}
			}
			for step := 0; step < 3; step++ {
				if ok, err := s.Claim(w.ID); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
				if err = e.process(ctx, w); err != nil {
					s.Finish(w, "failed", err.Error(), time.Now())
					t.Fatal(err)
				}
				if err = s.Finish(w, "done", "", time.Now()); err != nil {
					t.Fatal(err)
				}
				jobs, _ := s.Works(c.ID)
				next := Work{}
				for _, job := range jobs {
					if job.Status == "pending" && (job.Kind == "revisit" || job.Kind == "followup") {
						next = job
						break
					}
				}
				if next.ID == "" {
					break
				}
				// 仅在隔离实验中推进已经真实预约的时刻；不凭空制造复看机会。
				t.Logf("advance existing followup %s from %s", next.ID, time.UnixMilli(next.DueAt))
				if _, err = s.db.Exec(`UPDATE work SET due_at=? WHERE id=?`, time.Now().UnixMilli(), next.ID); err != nil {
					t.Fatal(err)
				}
				w = next
			}
			messages, _ := s.Messages(c.ID, 0, 100)
			counts := map[string]int{}
			for _, m := range messages {
				counts[m.AuthorID]++
			}
			// 检查参与任务有没有发生，不限定立场、句式、引用、先后和谁带动。
			for _, name := range []string{"裴船", "石粒"} {
				if counts[people[name].ID] == 0 {
					t.Errorf("%s 尚未实际加入辩论", name)
				}
			}
			if counts[people["爱情导师"].ID] != 0 {
				t.Error("明确退出的成员仍发言")
			}
			var assignments int
			s.db.QueryRow(`SELECT count(*) FROM work WHERE conversation_id=? AND kind='research'`, c.ID).Scan(&assignments)
			if assignments > 0 {
				t.Error("这次口头辩论被转换成后台研究")
			}
			t.Log(fmt.Sprintf("member message counts: %v", counts))
		})
	}
}
