package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestMemberRequestsKeepDeliveredHistoryAcrossActions(t *testing.T) {
	// 模拟账单上的场景：成员读长小说、发送评论并结束行动，之后才看到同伴的评论。
	// 走生产使用的 WebSocket 请求入口，核对实际序列化内容与缓存边界。
	s, _, member := fixture(t)
	peer := Member{ID: "member_peer", Name: "同伴", CreatedAt: now()}
	if err := s.AddMember(peer); err != nil {
		t.Fatal(err)
	}
	room, err := s.CreateConversation("小说群", "group", []string{member.ID, peer.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	novel := userMessage(room, "《第四排靠窗》"+strings.Repeat("窗帘盖住了第四排靠窗的座位。", 200))
	novel.AuthorID = peer.ID
	if err := s.Publish(novel, room.Revision); err != nil {
		t.Fatal(err)
	}
	// 模拟现场里也有最新正文的情况。只在确认聊天记录已有相同内容时移除副本。
	rows, err := s.Messages(room.ID, 0, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("读取小说失败: %v", err)
	}
	room.LastMessage, room.LastSeq = novel.Body, rows[0].Seq
	call := json.RawMessage(`{"type":"function_call","name":"send_message","call_id":"review_call","arguments":"{\"message\":\"我喜欢小说的开头。\"}"}`)
	finished := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"outcome\":\"已处理\"}"}]}`)
	outputs := make(chan json.RawMessage, 3)
	outputs <- call
	outputs <- finished
	outputs <- finished
	close(outputs)
	type wireRequest struct {
		Input []map[string]json.RawMessage `json:"input"`
		Tools json.RawMessage              `json:"tools"`
	}
	requests := make(chan wireRequest, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var request wireRequest
		if err := wsjson.Read(ctx, conn, &request); err != nil {
			t.Error(err)
			return
		}
		output, ok := <-outputs
		if !ok {
			t.Error("意外发起额外请求")
			return
		}
		requests <- request
		response := map[string]any{"status": "completed", "output": []json.RawMessage{output},
			"usage": responseUsage{InputTokens: 1000, OutputTokens: 50}}
		if err := wsjson.Write(ctx, conn, map[string]any{"type": "response.completed", "response": response}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.setModelEndpoint(server.URL)
	cfg.setSteering(true)
	engine := NewEngine(s, &HTTPModel{Store: s, Config: cfg, Client: server.Client()}, cfg)
	first := addWork(t, s, room, member, "chat")
	if _, err := engine.memberAction(t.Context(), room, member, first); err != nil {
		t.Fatal(err)
	}
	comment := userMessage(room, "我喜欢故事结尾，也想看看窗帘那一段。")
	comment.AuthorID, comment.ReplyTo = peer.ID, novel.ID
	if err := s.Publish(comment, room.Revision); err != nil {
		t.Fatal(err)
	}
	second := addWork(t, s, room, member, "followup")
	if _, err := engine.memberAction(t.Context(), room, member, second); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("应收到阅读、工具续接、下一轮三个请求，实际收到 %d", len(requests))
	}
	initial, afterTool, next := <-requests, <-requests, <-requests
	if string(afterTool.Tools) != string(next.Tools) {
		t.Fatal("换一轮聊天改变了可用工具")
	}

	// 对比模型实际读到的原话和工具结果。书签注解可以移动，注解内的文字保持一致。
	initialHistory, initialEnd := requestTestHistory(t, initial.Input, novel.ID)
	afterToolHistory, afterToolEnd := requestTestHistory(t, afterTool.Input, novel.ID)
	nextHistory, nextEnd := requestTestHistory(t, next.Input, comment.ID)
	if initialEnd != len(initialHistory)-1 || afterToolEnd != len(afterToolHistory)-1 || nextEnd != len(nextHistory)-1 {
		t.Fatal("缓存边界没有落在本轮说明前的历史末尾")
	}
	if len(afterToolHistory) != len(initialHistory)+2 || len(nextHistory) != len(afterToolHistory)+1 ||
		!reflect.DeepEqual(initialHistory, afterToolHistory[:len(initialHistory)]) ||
		!reflect.DeepEqual(afterToolHistory, nextHistory[:len(afterToolHistory)]) {
		t.Fatal("小说、调用或回执在续接时被重新包装、删掉或移位")
	}
	if string(afterToolHistory[afterToolEnd]["type"]) != `"function_call_output"` || string(afterToolHistory[afterToolEnd]["call_id"]) != `"review_call"` {
		t.Fatal("本轮模型没有收到原生工具回执")
	}
	if strings.Count(asJSON(next.Input), "《第四排靠窗》") != 1 || !strings.Contains(asJSON(nextHistory), comment.ID) || !strings.Contains(asJSON(nextHistory), comment.ReplyTo) {
		t.Fatal("小说正文重复，或新评价的编号、引用丢失")
	}
}

// 返回去掉传输书签后的历史。末尾两项分别是当前现场与行动来由。
func requestTestHistory(t *testing.T, items []map[string]json.RawMessage, newID string) ([]map[string]json.RawMessage, int) {
	t.Helper()
	if len(items) < 3 {
		t.Fatalf("没有完整历史和当前说明: %v", items)
	}
	var note string
	var scene struct {
		NewIDs []string        `json:"new_message_ids"`
		Latest json.RawMessage `json:"latest_messages"`
		Time   string          `json:"time"`
	}
	if err := json.Unmarshal(items[len(items)-2]["content"], &note); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(note), &scene); err != nil || !reflect.DeepEqual(scene.NewIDs, []string{newID}) || scene.Latest != nil || scene.Time == "" {
		t.Fatalf("末尾说明应点明新消息编号并保留当前时间: %v %s", err, note)
	}
	if !strings.Contains(string(items[len(items)-1]["content"]), "本次行动的来由") {
		t.Fatal("当前行动来由丢失")
	}
	history := items[:len(items)-2]
	boundary := -1
	for i, item := range history {
		for _, field := range []string{"content", "output"} {
			var block []struct {
				Type       string            `json:"type"`
				Text       string            `json:"text"`
				Breakpoint map[string]string `json:"prompt_cache_breakpoint"`
			}
			if json.Unmarshal(item[field], &block) != nil || len(block) != 1 || block[0].Breakpoint["mode"] != "explicit" {
				continue
			}
			if boundary >= 0 || block[0].Type != "input_text" {
				t.Fatal("历史应只含一个文本书签")
			}
			boundary = i
			item[field], _ = json.Marshal(block[0].Text)
		}
	}
	return history, boundary
}
