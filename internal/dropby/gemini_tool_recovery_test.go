package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"drop_by/internal/dropby/model/gemini"
)

func TestGeminiChatSearchWithoutCallIDs(t *testing.T) {
	// 真实接入点可能在同一回复里提出多个同名搜索，连 responseId 也省略。
	// 在正常执行和只完成第一项后重启两种情况下，下一轮都应收到对应的搜索结果。
	const first = `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"functionCall":{"name":"search","args":{"query":"资料甲"}},"thoughtSignature":"c2ln"},{"functionCall":{"name":"search","args":{"query":"资料乙"}}}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"totalTokenCount":120}}`
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%v", resume), func(t *testing.T) {
			checkGeminiSearchReply(t, first, []string{"资料甲", "资料乙"}, resume)
		})
	}
}

func checkGeminiSearchReply(t *testing.T, first string, queries []string, resume bool) {
	t.Helper()
	s, room, member := fixture(t)
	if err := s.AddUserMessage(userMessage(room, "请核对这些资料"), time.Hour); err != nil {
		t.Fatal(err)
	}
	work := latestChatWork(t, s, room.ID)
	var searchMu sync.Mutex
	var searched []string
	var generations atomic.Int32
	const reply = "核对完了。"
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			var request struct{ Query string }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			searchMu.Lock()
			searched = append(searched, request.Query)
			searchMu.Unlock()
			respond(w, 200, map[string]any{"results": []map[string]string{{"url": "https://example.test/source", "title": "来源：" + request.Query}}})
		case "/v1beta/models/gemini-4-hydrogen:generateContent":
			if generations.Add(1) == 1 {
				io.WriteString(w, first)
				return
			}
			// 检查真正发到模型端的 JSON：同轮回执相邻且按原顺序排列，
			// 每项包含自己的搜索结果，供应商协议里没有虚构的调用 ID。
			var request struct {
				Contents []struct {
					Parts []struct {
						FunctionResponse *struct {
							ID       json.RawMessage
							Name     string
							Response struct {
								Status string
								Result json.RawMessage
							}
						}
					}
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			matched := 0
			for _, turn := range request.Contents {
				for i, part := range turn.Parts {
					receipt := part.FunctionResponse
					if receipt == nil {
						continue
					}
					if len(turn.Parts) != len(queries) || i >= len(queries) || receipt.Name != "search" || receipt.ID != nil || receipt.Response.Status != "succeeded" || !strings.Contains(string(receipt.Response.Result), "来源："+queries[i]) {
						t.Errorf("第 %d 个原生回执与原调用不匹配：%+v", i, receipt)
					}
					matched++
				}
			}
			if matched != len(queries) {
				t.Errorf("收到 %d 个原生回执，应为 %d", matched, len(queries))
			}
			fmt.Fprintf(w, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":%s}]}}],"usageMetadata":{"promptTokenCount":150,"candidatesTokenCount":10,"totalTokenCount":160}}`, asJSON(asJSON(Outcome{Message: reply})))
		default:
			t.Errorf("意外请求：%s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer service.Close()
	cfg := testConfig()
	cfg.SearchURL = service.URL
	cfg.Models.Providers["native"] = modelProvider{Protocol: gemini.Protocol, BaseURL: service.URL, apiKey: "test", TokenCounting: "local"}
	cfg.Models.Scenes[sceneChat] = modelSettings{Provider: "native", Model: gemini.Hydrogen, ContextTokenBudget: 160000, maxOutputTokens: 1024}
	client := &HTTPModel{Config: cfg, Store: s, Client: service.Client()}
	if resume {
		// 构造进程在第一次搜索已落库、第二次未开始时留下的真实 SQLite 断点。
		// 新引擎没有上一进程的搜索缓存；重复执行会多出一次对 /search 的请求。
		engine := NewEngine(s, client, cfg)
		ctx := context.WithValue(t.Context(), modelSceneKey{}, sceneChat)
		messages := []ChatMessage{{Role: "user", Content: work.Instruction}}
		turn, err := client.Complete(ctx, messages, engine.Tools.Definitions(member, room, work), true)
		if err != nil || len(turn.ToolCalls) != len(queries) {
			t.Fatalf("首轮工具：%+v %v", turn, err)
		}
		var seen int64
		if err := s.db.QueryRow("SELECT max(seq) FROM messages WHERE conversation_id=?", room.ID).Scan(&seen); err != nil {
			t.Fatal(err)
		}
		if err := s.saveAction(actionCheckpoint{ID: newID("msg"), WorkID: work.ID, MemberID: member.ID, Seen: seen, Revision: room.Revision, ProfileRevision: member.ProfileRevision, Messages: append(messages, turn)}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Tools.Execute(ctx, room, member, work, turn.ToolCalls[0]); err != nil {
			t.Fatal(err)
		}
	}
	if err := NewEngine(s, client, cfg).process(t.Context(), work); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(room.ID, 0, 10)
	if err != nil || len(messages) != 2 || messages[1].AuthorID != member.ID || messages[1].Body != reply {
		t.Fatalf("群聊未收到成员的最终回复：%+v %v", messages, err)
	}
	runs, err := s.ToolRuns(room.ID, work.ID)
	searchMu.Lock()
	defer searchMu.Unlock()
	if err != nil || len(runs) != len(queries) || !slices.Equal(searched, queries) || generations.Load() != 2 {
		t.Fatalf("工具记录 %d，实际搜索 %v，模型调用 %d：%v", len(runs), searched, generations.Load(), err)
	}
}
