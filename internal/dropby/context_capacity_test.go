package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 旧聊天里有一段特别长的已读原文。新一轮并行读取资料、保存作品，
// 真人又补了一句。第一次原生压缩保留长原文，容量还是不足。
// 验证两种能继续的路径和一次重新启动，观察实际请求、数据库原文和写入次数。
func TestCapacityContinuesWithNewBatchAfterOversizedCompaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retry  bool
		resume bool
	}{
		{"引用腾出空间，恢复时直接续接", false, true},
		{"更小的旧窗口仍需压缩", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, room, member := fixture(t)
			work := addWork(t, s, room, member, "chat")
			old := userMessage(room, "旧活动长稿 "+strings.Repeat("archive-text ", 3500)+"旧活动末尾")
			if err := s.Publish(old, room.Revision); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow("SELECT seq FROM messages WHERE id=?", old.ID).Scan(&old.Seq); err != nil {
				t.Fatal(err)
			}
			history := appendDeliveredMessages(nil, []Message{old})
			if _, err := s.db.Exec("INSERT INTO working_contexts VALUES(?,?,?,?)", room.ID, member.ID, old.Seq, asJSON(history)); err != nil {
				t.Fatal(err)
			}
			goal := userMessage(room, "本轮只整理桌面便笺，别改原稿。")
			if err := s.Publish(goal, room.Revision); err != nil {
				t.Fatal(err)
			}
			work.SourceID = goal.ID
			source := ToolRun{ID: "desk_source", MemberID: member.ID, WorkID: work.ID, ConversationID: room.ID,
				Name: "read_page", Arguments: json.RawMessage(`{}`), Result: strings.Repeat("relevant ", 1100) + "资料尾部原文", Status: "succeeded", CreatedAt: now()}
			if err := s.RecordTool(source); err != nil {
				t.Fatal(err)
			}
			var compactions, generations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				body := string(b)
				has := func(text string) bool { return strings.Contains(body, text) }
				switch r.URL.Path {
				case "/responses/input_tokens":
					tokens := 8_000
					if has("资料尾部原文") && has("旧活动末尾") {
						tokens = 30_000
					} else if has("资料尾部原文") && tc.retry && has("已读聊天来源") {
						tokens = 27_000
					}
					fmt.Fprintf(w, `{"input_tokens":%d}`, tokens)
				case "/responses/compact":
					pass := compactions.Add(1)
					if has("资料尾部原文") || has("只整理桌面便笺") || has("只写标题和日期") || has("desk_save") {
						t.Error("压缩输入包含当前目标、待交付原话或工具批次")
					}
					if pass == 1 && !has("旧活动末尾") || pass == 2 && (!has(old.ID) || has("旧活动末尾")) {
						t.Error("第二次压缩应从有来源入口的更小历史开始")
					}
					output := []any{map[string]string{"type": "compaction", "encrypted_content": "whole_compaction"}}
					if pass == 1 {
						output = append([]any{map[string]string{"type": "message", "role": "user", "content": old.Body}}, output...)
					}
					json.NewEncoder(w).Encode(map[string]any{"output": output, "usage": map[string]int{"input_tokens": 8000, "output_tokens": 30}})
				case "/responses":
					turn := generations.Add(1)
					if turn == 1 {
						if err := s.Publish(userMessage(room, "补充一下，只写标题和日期。"), room.Revision); err != nil {
							t.Error(err)
						}
						io.WriteString(w, `{"status":"completed","output":[{"type":"function_call","name":"read_tool_result","call_id":"desk_read","arguments":"{\"id\":\"desk_source\",\"length\":18000}"},{"type":"function_call","name":"save_artifact","call_id":"desk_save","arguments":"{\"title\":\"桌面便笺\",\"content\":\"新便笺\",\"format\":\"markdown\"}"}],"usage":{"input_tokens":8000,"output_tokens":50}}`)
						return
					}
					if !has("资料尾部原文") || !has("只整理桌面便笺") || !has("只写标题和日期") || has("旧活动末尾") || has("deferred") ||
						strings.Count(body, `"call_id":"desk_read"`) != 2 || strings.Count(body, `"call_id":"desk_save"`) != 2 || !has("artifact_id") {
						t.Error("续接未完整交付新原文、工具配对、写入回执和当前目标")
					}
					if tc.retry != has("whole_compaction") {
						t.Error("仅继续使用经过复核、能容纳新批次的压缩窗口")
					}
					if tc.resume && turn == 2 {
						w.WriteHeader(400)
						return
					}
					io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"outcome\":\"已核对资料并保留便笺\"}"}]}],"usage":{"input_tokens":8000,"output_tokens":25}}`)
				default:
					t.Error(r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.setModelEndpoint(server.URL)
			cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.ContextTokenBudget = 25_000 })
			model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
			engine := NewEngine(s, model, cfg)
			_, err := engine.memberAction(context.Background(), room, member, work)
			if tc.resume {
				if err == nil {
					t.Fatal("预期模型端第一次续接失败")
				}
				cp, loadErr := s.loadAction(work.ID, member.ID)
				if loadErr != nil || !strings.Contains(asJSON(cp.Messages), "资料尾部原文") {
					t.Fatalf("断点缺少待交付资料: %v", loadErr)
				}
				engine = NewEngine(s, model, cfg)
				_, err = engine.memberAction(context.Background(), room, member, work)
			}
			if err != nil {
				t.Fatal(err)
			}
			var writes, reads int
			if err := s.db.QueryRow("SELECT count(*) FROM artifacts").Scan(&writes); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow("SELECT count(*) FROM tool_runs WHERE work_id=? AND name='read_tool_result'", work.ID).Scan(&reads); err != nil {
				t.Fatal(err)
			}
			wantPasses := int32(1)
			if tc.retry {
				wantPasses++
			}
			if writes != 1 || reads != 1 || compactions.Load() != wantPasses {
				t.Fatalf("应只读取和保存各一次，压缩 %d 次，实际 %d/%d/%d", wantPasses, reads, writes, compactions.Load())
			}
			var archived string
			if err := s.db.QueryRow("SELECT body FROM messages WHERE id=?", old.ID).Scan(&archived); err != nil || archived != old.Body {
				t.Fatal("压缩替换了底层聊天原文", err)
			}
		})
	}
}

// 新资料本身就超过预算。只把最大的同批回执标为待分段读取，失败的
// 同批结果仍照常交付。下一轮按保存的读取 ID 找到原文最后一段。
func TestCapacityPagesOversizedOrganizeSiblingWithoutLosingSmallResult(t *testing.T) {
	s, room, member := fixture(t)
	work := addWork(t, s, room, member, "chat")
	source := ToolRun{ID: "large_source", MemberID: member.ID, WorkID: work.ID, ConversationID: room.ID,
		Name: "read_page", Arguments: json.RawMessage(`{}`), Result: strings.Repeat("large_document ", 7000) + "要核对的尾注", Status: "succeeded", CreatedAt: now()}
	if err := s.RecordTool(source); err != nil {
		t.Fatal(err)
	}
	var generations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		switch r.URL.Path {
		case "/responses/input_tokens":
			count := 8000
			if strings.Contains(body, strings.Repeat("large_document ", 4)) {
				count = 85_000
			}
			fmt.Fprintf(w, `{"input_tokens":%d}`, count)
		case "/responses":
			switch generations.Add(1) {
			case 1:
				io.WriteString(w, `{"status":"completed","output":[{"type":"function_call","name":"organize_context","call_id":"empty_note","arguments":"{\"note\":\"\"}"},{"type":"function_call","name":"read_tool_result","call_id":"too_long","arguments":"{\"id\":\"large_source\",\"length\":150000}"},{"type":"function_call","name":"read_tool_result","call_id":"missing","arguments":"{\"id\":\"missing_source\"}"}],"usage":{"input_tokens":5000,"output_tokens":90}}`)
			case 2:
				var readID string
				err := s.db.QueryRow("SELECT id FROM tool_runs WHERE work_id=? AND name='read_tool_result' AND status='succeeded'", work.ID).Scan(&readID)
				if err != nil || !strings.Contains(body, readID) || !strings.Contains(body, "deferred") || !strings.Contains(body, "尚未交付") ||
					!strings.Contains(body, "当前聊天没有这条可读取的行动记录") || strings.Contains(body, "要核对的尾注") {
					t.Error("应明确延期一份过大结果，保留较小的失败回执", err)
				}
				json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{map[string]string{"type": "function_call", "name": "read_tool_result", "call_id": "page_tail", "arguments": asJSON(map[string]any{"id": readID, "query": "要核对的尾注", "length": 6})}}, "usage": map[string]int{"input_tokens": 5000, "output_tokens": 40}})
			case 3:
				if !strings.Contains(body, "要核对的尾注") || strings.Contains(body, strings.Repeat("large_document ", 4)) {
					t.Error("应按调用者给定的范围返回原文尾注")
				}
				io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"outcome\":\"已核对尾注\"}"}]}],"usage":{"input_tokens":5000,"output_tokens":20}}`)
			default:
				t.Error("意外续接")
				w.WriteHeader(400)
			}
		default:
			t.Error("只有新资料超限时无需付费压缩旧历史", r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.setModelEndpoint(server.URL)
	cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.ContextTokenBudget = 25_000 })
	engine := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: server.Client()}, cfg)
	if _, err := engine.memberAction(context.Background(), room, member, work); err != nil {
		t.Fatal(err)
	}
	if generations.Load() != 3 {
		t.Fatal("未完成分段续接")
	}
}

// 已有历史是不可拆的原生窗口。新结果单独能放下，与新压缩状态合起来
// 才超限。保留整个原生返回并给出分段入口即可继续，无需再次付费压缩。
func TestCapacityKeepsNativeStateWhenNewResultNeedsPaging(t *testing.T) {
	s, room, member := fixture(t)
	work := addWork(t, s, room, member, "chat")
	source := ToolRun{ID: "large_receipt", MemberID: member.ID, WorkID: work.ID, ConversationID: room.ID,
		Name: "read_page", Arguments: json.RawMessage(`{}`), Result: asJSON(strings.Repeat("long read ", 1800)), Status: "succeeded", CreatedAt: now()}
	if err := s.RecordTool(source); err != nil {
		t.Fatal(err)
	}
	chain := []ChatMessage{
		{Role: "system", Content: "继续核对资料"},
		{Role: "user", ResponseItems: []json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"older_state"}`)}, OutputTokens: 20_000},
		{Role: "assistant", ResponseItems: []json.RawMessage{json.RawMessage(`{"type":"function_call","name":"read_page","call_id":"page","arguments":"{}"}`)}},
		{Role: "tool", ToolCallID: "page", PendingDelivery: true, Content: asJSON(map[string]any{
			"tool_run_id": source.ID, "status": source.Status, "result": json.RawMessage(source.Result),
		})},
	}
	var compactions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		switch r.URL.Path {
		case "/responses/input_tokens":
			tokens := 8000
			if strings.Contains(body, "long read") && (strings.Contains(body, "older_state") || strings.Contains(body, "whole_new_state")) {
				tokens = 25_000
			}
			fmt.Fprintf(w, `{"input_tokens":%d}`, tokens)
		case "/responses/compact":
			compactions.Add(1)
			if !strings.Contains(body, "older_state") || strings.Contains(body, "long read") {
				t.Error("只应压缩已有原生历史")
			}
			io.WriteString(w, `{"output":[{"type":"message","role":"user","content":"保留的历史原话"},{"type":"compaction","encrypted_content":"whole_new_state"}],"usage":{"input_tokens":8000,"output_tokens":30}}`)
		case "/responses":
			if !strings.Contains(body, "whole_new_state") || !strings.Contains(body, "保留的历史原话") || !strings.Contains(body, "deferred") ||
				!strings.Contains(body, source.ID) || strings.Contains(body, "long read") {
				t.Error("应同时保留全部原生返回和新资料的分段入口")
			}
			io.WriteString(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"继续分段读取"}]}],"usage":{"input_tokens":8000,"output_tokens":20}}`)
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.setModelEndpoint(server.URL)
	cfg.editScenes(modelScenes[:], func(s *modelSettings) { s.ContextTokenBudget = 16_000 })
	model := &HTTPModel{Config: cfg, Store: s, Client: server.Client()}
	defs := []ToolDefinition{definition("read_tool_result", "分页读取", map[string]any{"id": textField("来源")}, "id")}
	if _, err := completeModel(context.Background(), model, &chain, defs, false); err != nil {
		t.Fatal(err)
	}
	if compactions.Load() != 1 {
		t.Fatalf("应只压缩一次，实际 %d", compactions.Load())
	}
}
