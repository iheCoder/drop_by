package dropby

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"drop_by/internal/observe"
)

func TestChatAndObservationKeepResponsesUsage(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			// 从聊天 API 发消息，执行一次真实本地工具，再查聊天和观测 API。
			// 两步都带混合 TTL，确保结算不会因为账本只收汇总而退回短缓存价格。
			s, room, member := fixture(t)
			if kind == "direct" {
				var err error
				room, err = s.CreateConversation(member.Name, "direct", []string{member.ID}, testConfig().ExploreInterval)
				if err != nil {
					t.Fatal(err)
				}
			}
			const usage = `"usage":{"input_tokens":1000,"output_tokens":100,"input_tokens_details":{"cached_tokens":400,"cache_write_tokens":200,"cache_write_tokens_details":{"ttl_30m_tokens":100,"ttl_12h_tokens":100}},"output_tokens_details":{"reasoning_tokens":60}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				body := string(raw)
				if r.URL.Path != "/responses" || !strings.Contains(body, "缓存结算验收原话") {
					t.Errorf("实际请求缺少聊天原话或路径错误: %s", r.URL.Path)
				}
				if strings.Contains(body, "function_call_output") {
					if !strings.Contains(body, "call_cache_chat") || !strings.Contains(body, "cache-chat-opaque") {
						t.Error("工具续接缺少调用 ID 或原生推理")
					}
					io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"message\":\"收到，已核对本地历史\"}"}]}],`+usage+`}`)
					return
				}
				io.WriteString(w, `{"status":"completed","output":[{"type":"reasoning","encrypted_content":"cache-chat-opaque"},{"type":"function_call","name":"read_history","call_id":"call_cache_chat","arguments":"{}"}],`+usage+`}`)
			}))
			defer server.Close()

			cfg := testConfig()
			cfg.setModelEndpoint(server.URL)
			engine := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: server.Client()}, cfg)
			path := filepath.Join(t.TempDir(), "observe.sqlite")
			log, err := observe.OpenLog(path)
			if err != nil {
				t.Fatal(err)
			}
			go log.Serve()
			engine.Observe = log
			t.Cleanup(func() {
				if engine.Observe != nil {
					log.Close()
				}
			})

			chatAPI := NewHandler(engine)
			chatPath := "/api/conversations/" + room.ID
			if w := preferenceRequest(chatAPI, "POST", chatPath+"/messages", map[string]string{"body": "缓存结算验收原话"}); w.Code != 201 {
				t.Fatalf("聊天提交失败: %d %s", w.Code, w.Body)
			}
			engine.dispatch(t.Context())
			engine.wg.Wait()
			// 关闭写入器会清空待写队列。观测端随后通过独立的只读入口读库。
			log.Close()
			engine.Observe = nil
			if w := preferenceRequest(chatAPI, "GET", chatPath, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "收到，已核对本地历史") {
				t.Fatalf("无法查回成员答复: %d %s", w.Code, w.Body)
			}
			calls, err := s.Usage()
			if err != nil || calls.Calls != 2 || calls.CostMicros != 2590 || calls.UnsettledMicros != 0 {
				t.Fatalf("聊天和工具续接应各按 1295 微美元结算: %+v %v", calls, err)
			}
			tools, err := s.ToolRuns(room.ID, "")
			if err != nil || len(tools) != 1 || tools[0].Name != "read_history" || tools[0].Status != "succeeded" {
				t.Fatalf("本地工具未成功执行一次: %+v %v", tools, err)
			}

			t.Setenv("OBSERVE_DB", path)
			obsAPI := observe.NewHandler()
			var runs struct{ Runs []observe.RunView }
			if w := preferenceRequest(obsAPI, "GET", "/api/runs", nil); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &runs) != nil ||
				len(runs.Runs) != 1 || runs.Runs[0].Status != "done" || runs.Runs[0].ConversationID != room.ID {
				t.Fatalf("观测未找到完成的聊天: %d %s", w.Code, w.Body)
			}
			var steps struct{ Steps []observe.StepView }
			if w := preferenceRequest(obsAPI, "GET", "/api/runs/"+runs.Runs[0].ID, nil); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &steps) != nil {
				t.Fatalf("无法读取观测步骤: %d %s", w.Code, w.Body)
			}
			var observed int
			for _, step := range steps.Steps {
				if step.Kind != "model" {
					continue
				}
				observed++
				if step.CostStatus != "settled" || step.CostMicros == nil || *step.CostMicros != 1295 || step.BusinessID == "" {
					t.Errorf("观测步骤需引用账本金额: %+v", step)
				}
				var snapshot struct{ Input, Output string }
				if w := preferenceRequest(obsAPI, "GET", "/api/steps/"+step.ID, nil); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &snapshot) != nil ||
					!strings.Contains(snapshot.Input, "缓存结算验收原话") || !strings.Contains(snapshot.Output, usage) {
					t.Errorf("观测快照丢失请求或供应商原始用量: %d %s", w.Code, w.Body)
				}
			}
			if observed != 2 {
				t.Fatalf("应观测到两次实际模型调用，得到 %d", observed)
			}
		})
	}
}
