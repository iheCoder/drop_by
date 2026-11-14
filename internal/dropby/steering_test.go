package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"drop_by/internal/dropby/model"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestSteeringProtocolAccountingAndPendingTools(t *testing.T) {
	// 用真实 WebSocket 帧验证已接受/已应用的区别，包含自动续接、工具等待与拒绝。
	// 每个响应独立结算；尚未启动的续接释放预留，断线后的未知预留不能冒充免费。
	for _, mode := range []string{"completed", "incomplete", "tool_pending", "rejected", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := fixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var request map[string]any
				if err = wsjson.Read(ctx, conn, &request); err != nil {
					t.Error(err)
					return
				}
				if request["type"] != "response.create" || request["model"] != "gpt-6.1-terra" || request["store"] != false {
					t.Errorf("错误请求：%v", request)
				}
				write := func(v any) {
					if err := wsjson.Write(ctx, conn, v); err != nil {
						t.Error(err)
					}
				}
				write(map[string]any{"type": "response.created", "response": map[string]any{"id": "first"}})
				var steer map[string]any
				if err = wsjson.Read(ctx, conn, &steer); err != nil {
					t.Error(err)
					return
				}
				if steer["type"] != "response.steer" || steer["previous_response_id"] != "first" || !strings.Contains(asJSON(steer), "新要求") {
					t.Errorf("错误 steering：%v", steer)
				}
				if mode == "disconnect" {
					return
				}
				if mode == "rejected" {
					write(map[string]any{"type": "response.steer.failed"})
				} else {
					write(map[string]any{"type": "response.steer.accepted"})
				}
				output := []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "original"}}}}
				if mode == "tool_pending" {
					output = []any{map[string]string{"type": "function_call", "call_id": "call_one", "name": "read_history", "arguments": "{}"}}
				}
				status := "completed"
				if mode == "incomplete" {
					status = "incomplete"
				}
				write(map[string]any{"type": "response." + status, "response": map[string]any{"id": "first", "status": status, "incomplete_details": map[string]string{"reason": "steered"}, "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2,
					"input_tokens_details": map[string]any{"cached_tokens": 2, "cache_write_tokens": 4,
						"cache_write_tokens_details": map[string]int{"ttl_30m_tokens": 2, "ttl_12h_tokens": 2}}}}})
				if mode == "rejected" || mode == "tool_pending" {
					return
				}
				write(map[string]any{"type": "response.created", "response": map[string]any{"id": "second"}})
				write(map[string]any{"type": "response.completed", "response": map[string]any{"id": "second", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "updated"}}}}, "usage": map[string]int{"input_tokens": 20, "output_tokens": 3}}})
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.setModelEndpoint(server.URL)
			cfg.setSteering(true)
			client := &HTTPModel{Store: s, Config: cfg, Client: server.Client()}
			input := &steeringInput{Read: func() ([]model.Update, error) {
				return []model.Update{{ID: "human_new", Data: json.RawMessage(asJSON(Message{ID: "human_new", AuthorID: "user", Body: "新要求"}))}}, nil
			}}
			ctx := context.WithValue(context.Background(), steeringKey{}, input)
			chain := []ChatMessage{{Role: "user", Content: "original task"}}
			result, err := completeModel(ctx, client, &chain, nil, false)
			usage, _ := s.Usage()
			if mode == "disconnect" {
				if err == nil || usage.UnsettledMicros == 0 {
					t.Fatal("断线没有保留未知预留")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if usage.UnsettledMicros != 0 {
				t.Fatalf("已知结果仍留预留：%+v", usage)
			}
			// 第一笔 Terra 的输入拆成 4 普通、2 读、2 短写、2 长写，另计 2 输出。
			// 每笔分别向上取整，第一笔 22 微美元；若收到续接，再加 35 微美元。
			wantCost := int64(22)
			if mode == "completed" || mode == "incomplete" {
				wantCost += 35
			}
			if usage.CostMicros != wantCost {
				t.Fatalf("WebSocket 缓存写入细分结算错误: %d, 预期 %d", usage.CostMicros, wantCost)
			}
			if mode == "tool_pending" {
				if len(result.ToolCalls) != 1 || len(chain) != 1 || usage.InputTokens != 10 {
					t.Fatal("工具等待被误当成补充已经应用")
				}
				return
			}
			if mode == "rejected" {
				if result.Content != "original" || len(chain) != 1 {
					t.Fatal("拒绝 steering 破坏原响应")
				}
				return
			}
			if result.Content != "updated" || len(chain) != 3 || !strings.Contains(chain[2].Content, "human_new") || usage.InputTokens != 30 || usage.OutputTokens != 5 {
				t.Fatalf("续接或分别记账错误：%+v %+v", result, usage)
			}
		})
	}
}

func TestSteeringReadsOnlyNewHumanMessages(t *testing.T) {
	s, c, m := fixture(t)
	s.Publish(Message{ID: "peer", ConversationID: c.ID, AuthorID: m.ID, Body: "BOT 的建议", CreatedAt: now()}, c.Revision)
	s.Publish(userMessage(c, "真人要求"), c.Revision)
	cfg := testConfig()
	cfg.setSteering(true)
	engine := NewEngine(s, modelFunc(func(ctx context.Context, _ []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		source := ctx.Value(steeringKey{}).(*steeringInput)
		messages, err := source.Read()
		if err != nil {
			return ChatMessage{}, err
		}
		if len(messages) != 1 || !strings.Contains(string(messages[0].Data), `"author_id":"user"`) {
			t.Fatal("把 BOT 发言升级为真人 steering")
		}
		source.Applied[messages[0].ID] = true // 模拟该真人补充的自动续接已经完成。
		return jsonReply(Outcome{}), nil
	}), cfg)
	chain := []ChatMessage{{Role: "user", Content: "现场"}}
	seen := int64(0)
	if _, err := engine.completeMemberModel(context.Background(), c, &seen, &chain, nil); err != nil {
		t.Fatal(err)
	}
	if seen != 0 {
		t.Fatal("Steering 跳过了前面的未读 BOT 消息")
	}
	// 已经读过前面的 BOT 原话时，人类补充才构成连续已交付位置。
	seen = 1
	if _, err := engine.completeMemberModel(context.Background(), c, &seen, &chain, nil); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatal("已完成的连续 steering 输入没有推进交付位置")
	}
}

func TestLiveGPT6Steering(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_STEERING") != "1" {
		t.Skip("显式开启实际 GPT-6 steering 验证")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gpt-6.1-terra", "gpt-6.1-sol"} {
		t.Run(name, func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg.editScenes([]modelScene{sceneMemberSettings}, func(s *modelSettings) { s.Model = name; s.setOutputLimit(128) })
			cfg.setSteering(true)
			client := &HTTPModel{Store: s, Config: cfg, Client: &http.Client{Timeout: 45 * time.Second}}
			source := &steeringInput{Read: func() ([]model.Update, error) {
				return []model.Update{{ID: "update", Data: json.RawMessage(asJSON(Message{ID: "update", AuthorID: "user", Body: "Instead reply with the single word UPDATED."}))}}, nil
			}}
			ctx := context.WithValue(context.Background(), steeringKey{}, source)
			chain := []ChatMessage{{Role: "user", Content: "Reply with the single word ORIGINAL."}}
			reply, err := completeModel(ctx, client, &chain, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(reply.Content, "UPDATED") || len(chain) < 3 {
				t.Fatalf("未收到实际自动续接：%s", reply.Content)
			}
			usage, _ := s.Usage()
			if usage.Calls != 2 || usage.UnsettledMicros != 0 {
				t.Fatalf("实际用量未完整结算：%+v", usage)
			}
			t.Log(fmt.Sprintf("自动续接通过；%d 次响应，输入 %d / 输出 %d tokens，估算 $%.6f", usage.Calls, usage.InputTokens, usage.OutputTokens, float64(usage.CostMicros)/1e6))
			// 确认保存的链保持可序列化，不能只有终端文本而丢失原生输出项目。
			if _, err := json.Marshal(chain); err != nil {
				t.Fatal(err)
			}
		})
	}
}
