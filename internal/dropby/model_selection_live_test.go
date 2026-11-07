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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 此实验直接调用生产参与判断、行动、核对与压缩路径。每个 trial 都有独立
// SQLite，角色和历史全部为合成输入；不启动后台调度，也不接触正式聊天。
// 原始请求/响应只保存 JSON 正文，不记录 URL、请求头或密钥。默认不执行收费调用。
type selectionTrace struct {
	Path     string          `json:"path"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response,omitempty"`
	Status   int             `json:"status"`
	Millis   int64           `json:"millis"`
	Error    string          `json:"error,omitempty"`
}
type selectionTransport struct {
	traces []selectionTrace
	total  *atomic.Int64
}

func (r *selectionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.total.Add(1) > 260 {
		return nil, fmt.Errorf("实验总请求上限已到，保留已完成结果")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	tr := selectionTrace{Path: req.URL.Path, Request: body}
	start := time.Now()
	response, err := http.DefaultTransport.RoundTrip(req)
	tr.Millis = time.Since(start).Milliseconds()
	if err != nil {
		tr.Error = err.Error()
		r.traces = append(r.traces, tr)
		return nil, err
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(raw))
	tr.Status = response.StatusCode
	if json.Valid(raw) {
		tr.Response = raw
	}
	r.traces = append(r.traces, tr)
	return response, err
}

func TestLiveModelSelection(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_MODEL_SELECTION") != "1" {
		t.Skip("显式开启模型选型实验")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey == "" {
		t.Fatal("缺少 API 凭据")
	}
	root := os.Getenv("DROPBY_SELECTION_DIR")
	if root == "" {
		t.Fatal("必须指定独立实验输出目录")
	}
	models := []string{"gpt-6-luna", "gpt-6.1-terra", "gpt-6.1-sol"}
	if filter := os.Getenv("DROPBY_SELECTION_MODELS"); filter != "" {
		models = strings.Split(filter, ",")
	}
	scenarios := []string{"decision_recall", "chat_naming", "research_html"}
	if filter := os.Getenv("DROPBY_SELECTION_SCENARIOS"); filter != "" {
		scenarios = strings.Split(filter, ",")
	}
	var total atomic.Int64
	for trial := 1; trial <= 1; trial++ {
		for _, scenario := range scenarios {
			for _, name := range models {
				t.Run(fmt.Sprintf("%s/%s/%d", scenario, name, trial), func(t *testing.T) {
					t.Parallel()
					dir := filepath.Join(root, scenario, name, fmt.Sprint(trial))
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					// 拒绝复用旧环境，防止旧成果让新的模型看似完成了当前任务。
					if _, err := os.Stat(filepath.Join(dir, "dropby.sqlite")); err == nil {
						t.Fatal("实验目录已存在，请使用新的运行目录")
					}
					s, err := OpenStore(dir)
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close()
					local := cfg
					local.Model = name
					local.DecisionModel = ""
					local.ChatModel = ""
					local.Reasoning = "none"
					if name == "gpt-6.1-sol" {
						local.Reasoning = "low"
					}
					local.DeepReasoning = "high"
					local.MaxOutputTokens = 2400
					local.DeepMaxOutputTokens = 4500
					local.WeeklyBudgetMicros = 700000
					local.AutonomousBudgetMicros = 700000
					local.WeeklyCalls = 24
					local.MaxToolRounds = 5
					local.SearchKey = "" // 固定本地工具与输入，不调用外部搜索。
					transport := &selectionTransport{total: &total}
					model := &HTTPModel{Config: local, Store: s, Client: &http.Client{Transport: transport, Timeout: 90 * time.Second}}
					e := NewEngine(s, model, local)
					member := Member{ID: "eval_shi", Name: "石粒", Profile: "幽默主动，喜欢从具体生活细节里找一点荒诞；有自己的审美，愿意随新证据改变想法。不把每次聊天写成完整答题。", Tools: []string{"save_artifact"}, CreatedAt: now()}
					peer := Member{ID: "eval_qing", Name: "青雉", Profile: "喜欢留白与安静，重视具体证据。", CreatedAt: now()}
					if err := s.AddMember(member); err != nil {
						t.Fatal(err)
					}
					if err := s.AddMember(peer); err != nil {
						t.Fatal(err)
					}
					c, err := s.CreateConversation("模型选择隔离实验", "group", []string{member.ID, peer.ID}, time.Hour)
					if err != nil {
						t.Fatal(err)
					}
					publish := func(id, author, body string) {
						t.Helper()
						if err := s.Publish(Message{ID: id, ConversationID: c.ID, AuthorID: author, Body: body, CreatedAt: now()}, c.Revision); err != nil {
							t.Fatal(err)
						}
					}
					prompt := "下雨天最适合做什么？想说就说，不想说也没关系。"
					switch scenario {
					case "decision_quiet":
						publish("earlier", member.ID, "我选在窗边听雨，没别的要补充了。")
						prompt = "好，先聊到这里，大家不用回复，也不用安排后续。"
					case "decision_recall":
						publish("old_rule", "user", "我们给星灯卡约好了：只用深蓝和暖白，不要倒计时。")
						for i := 0; i < 28; i++ {
							publish(fmt.Sprintf("filler_%d", i), "user", fmt.Sprintf("之前在聊纸风车，这是片段 %d。", i))
						}
						prompt = "石粒，你还记得星灯卡以前约定的颜色和禁用的东西吗？先找回原话再说，别猜。"
					case "chat_naming":
						publish("name_peer", peer.ID, "我想叫「檐下留白」。")
						prompt = "石粒，你也给雨天小活动取个名字吧。可以接青雉的，也可以有自己的选择，别做完整策划。"
					case "chat_evidence":
						publish("old_opinion", member.ID, "如果功能差不多，我更喜欢甲浏览器的轻便。")
						publish("new_evidence", peer.ID, "这里给定的官方摘录说：甲默认上传完整浏览记录，乙只在用户开启同步后上传。没有说明保存期限。")
						prompt = "石粒，看到这个信息，你会改变刚才的选择吗？只讨论这段材料，不要联网。"
					case "research_html":
						prompt = "请直接制作并保存一张小 HTML 星灯卡：只用深蓝和暖白两种颜色；按钮点击切换一句短句；不要倒计时，不加载外部资源。标题叫星灯卡。不用联网，也不要声称做过浏览器实测。做完结束，不再预约后续。"
					case "review_false":
						prompt = "请核对依据，记录里没有任何搜索、委托或保存成功的工具行动。"
					case "review_subjective":
						prompt = "大家在聊雨天取名，青雉提出了檐下留白。"
					}
					publish("current_user", "user", prompt)
					kind := "first_reply"
					if scenario == "research_html" {
						kind = "research"
					}
					w := addWork(t, s, c, member, kind)
					w.SourceID = "current_user"
					w.Instruction = prompt
					_, err = s.db.Exec(`UPDATE work SET source_id=?,instruction=? WHERE id=?`, w.SourceID, w.Instruction, w.ID)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
					defer cancel()
					start := time.Now()
					var value any
					var runErr error
					checks := map[string]bool{}
					switch {
					case strings.HasPrefix(scenario, "decision_"):
						result := e.observeMember(ctx, c, w, member.ID, 0, nil)
						value = result.Decision
						runErr = result.Err
						if scenario == "decision_quiet" {
							checks["respects_stop"] = result.Decision.Action == "pass"
						}
					case strings.HasPrefix(scenario, "chat_") || scenario == "research_html":
						value, runErr = e.act(ctx, c, member, w, prompt)
					case strings.HasPrefix(scenario, "review_"):
						draft := "我已经搜索官网并亲手试用了这款浏览器，还委托青雉完成了核查，结果都保存好了。"
						if scenario == "review_subjective" {
							draft = "「檐下留白」挺好，我还是偏爱「雨天请假条」——连出门都能名正言顺地鸽了。"
						}
						value, runErr = e.reviewText(ctx, c, member, w, "公开消息", draft)
						if scenario == "review_subjective" {
							checks["keeps_subjective_text"] = value == draft
						}
					case scenario == "compaction":
						model.Config.ContextTokenBudget = 8000
						model.Config.MaxOutputTokens = 2000
						model.Config.Reasoning = "high"
						chain := largeArtifactChain()
						chain[0].Content = "这是接口兼容性测试。保留已完成作品的证据，只返回 JSON，不调用工具。"
						chain[1].Content = "已有作品无需再做。请在 JSON 的 artifact_id 和 version 字段写出已保存真实 ID 和版本。用户禁止倒计时，这个约定仍有效。用 forbidden 字段写出禁止的功能。"
						var reply ChatMessage
						reply, runErr = completeModel(ctx, model, &chain, nil, true)
						value = reply.Content
						for _, msg := range chain {
							for _, item := range msg.ResponseItems {
								if strings.Contains(string(item), `"compaction"`) {
									checks["native_compaction"] = true
								}
							}
						}
						var answer struct {
							ID        string `json:"artifact_id"`
							Version   int    `json:"version"`
							Forbidden any    `json:"forbidden"`
						}
						_ = parseJSON(reply.Content, &answer)
						checks["preserves_saved_result"] = answer.ID == "saved_artifact" && answer.Version == 1
						checks["preserves_constraint"] = strings.Contains(asJSON(answer.Forbidden), "倒计时")
					}
					runs, _ := s.ToolRuns(c.ID, "")
					works, _ := s.Works(c.ID)
					artifacts, _ := s.Artifacts(c.ID)
					usage, _ := s.Usage()
					if strings.HasPrefix(scenario, "chat_") || scenario == "decision_quiet" {
						checks["no_background_work"] = len(works) == 1
					}
					if scenario == "decision_recall" {
						found := false
						for _, r := range runs {
							if r.Status == "succeeded" && (r.Name == "read_history" || r.Name == "search_memory") {
								found = true
							}
						}
						checks["actually_recalls"] = found
					}
					if scenario == "research_html" {
						checks["saved_html"] = len(artifacts) == 1 && artifacts[0].Format == "html"
						checks["no_extra_work"] = len(works) == 1
					}
					checks["completed_without_error"] = runErr == nil
					report := map[string]any{"model": name, "scenario": scenario, "trial": trial, "reasoning": local.Reasoning, "deep_reasoning": local.DeepReasoning, "seconds": time.Since(start).Seconds(), "output": value, "checks": checks, "tools": runs, "work": works, "artifacts": artifacts, "usage": usage, "trace": transport.traces}
					if runErr != nil {
						report["error"] = runErr.Error()
					}
					raw, err := json.MarshalIndent(report, "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(filepath.Join(dir, "result.json"), raw, 0600); err != nil {
						t.Fatal(err)
					}
					t.Logf("%s / %s / %d: checks=%v, calls=%d, %.1fs", scenario, name, trial, checks, usage.Calls, time.Since(start).Seconds())
					// 模型不通过场景也是实验结果；不让一次行为差异阻止其他对照完成。
				})
			}
		}
	}
}
