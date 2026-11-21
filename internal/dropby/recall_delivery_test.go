package dropby

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 一次模型回合可以查多条 SQL。预览只改变交付，完整结果仍可在原记录中定位，
// 中断恢复不能把大结果突然全塞回去。显式全文同时验证超过旧 5000 字默认长度的尾部。
func TestRecallBatchDeliveryAndRecovery(t *testing.T) {
	s, room, member := fixture(t)
	work := addWork(t, s, room, member, "explore")
	body := strings.Repeat("静水流深。", 1600) + "终稿尾注"
	if err := s.SystemMessage(room.ID, "recall_long_message", body); err != nil {
		t.Fatal(err)
	}
	box := NewEngine(s, nil, testConfig()).Tools
	query := map[string]any{"sql": "SELECT body FROM records WHERE id='recall_long_message'"}
	calls := []ToolCall{toolCall("recall", query), toolCall("recall", query), toolCall("recall", map[string]any{"id": "recall_long_message", "full": true})}
	var outputs []ChatMessage
	for _, call := range calls {
		raw, err := box.Execute(t.Context(), room, member, work, call)
		if err != nil || !strings.Contains(raw, body) {
			t.Fatal("未保存完整结果", err)
		}
		outputs = append(outputs, ChatMessage{Role: "tool", ToolCallID: call.ID, Content: raw})
	}
	presentRecallBatch(calls, outputs)
	for _, message := range outputs[:2] {
		var response struct {
			ID     string `json:"tool_run_id"`
			Result struct {
				Rows    [][]string
				Preview bool
			}
		}
		if err := json.Unmarshal([]byte(message.Content), &response); err != nil {
			t.Fatal(err)
		}
		rows := response.Result.Rows
		if !response.Result.Preview || len(rows) != 1 || len(rows[0]) != 1 || len([]rune(rows[0][0])) > recallPreviewChars/2 || !strings.HasPrefix(body, rows[0][0]) || strings.Contains(message.Content, "终稿尾注") {
			t.Fatal("同批长单元格预览错误")
		}
		got, err := box.perform(t.Context(), room, member, work, toolCall("read_tool_result", map[string]any{"id": response.ID, "query": "终稿尾注", "length": 4}))
		if err != nil || got.(map[string]any)["content"] != "终稿尾注" {
			t.Fatal("预览遗漏部分无法定位", err)
		}
	}
	if !strings.Contains(outputs[2].Content, body) {
		t.Fatal("全文尾部未交付")
	}
	recovered, err := NewEngine(s, nil, testConfig()).resumeReceipts([]ChatMessage{{Role: "assistant", ToolCalls: calls}}, work, member)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range outputs {
		var previous, resumed any
		if json.Unmarshal([]byte(m.Content), &previous) != nil || json.Unmarshal([]byte(recovered[i+1].Content), &resumed) != nil || !reflect.DeepEqual(previous, resumed) {
			t.Fatal("恢复改变了回执", i)
		}
	}
}

// SQL 只能读调用者这一刻的资料。分享关系连到具体版次；自定义关联、聚合和 LIKE
// 都由 SQLite 执行，失败查询不会改变后续调用者的数据或权限。
func TestRecallSQLScopeAndShares(t *testing.T) {
	s, room, people := groupWithMembers(t, 2)
	author, peer := people[0], people[1]
	personal, err := s.personalConversation(author.ID)
	if err != nil {
		t.Fatal(err)
	}
	work := addWork(t, s, personal, author, "explore")
	box := NewEngine(s, nil, testConfig()).Tools
	var saved struct {
		ID     string `json:"tool_run_id"`
		Result struct {
			ID string `json:"artifact_id"`
		}
	}
	raw, err := box.Execute(t.Context(), personal, author, work, toolCall("save_artifact", map[string]any{"title": "电影随笔", "content": "单纯胖到飞不起来。"}))
	if err != nil || json.Unmarshal([]byte(raw), &saved) != nil || saved.Result.ID == "" {
		t.Fatal(raw, err)
	}
	_, err = box.perform(t.Context(), personal, author, work, toolCall("send_message", map[string]any{"conversation_id": room.ID, "message": "第一版", "evidence": []string{saved.ID}}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = box.perform(t.Context(), personal, author, work, toolCall("save_artifact", map[string]any{"id": saved.Result.ID, "title": "电影随笔", "content": "私下更新的第二版"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO memories VALUES(?,?,?,?,?,?)", "private_note", author.ID, "", "私有笔记", "", now()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"SELECT * FROM tool_runs", "UPDATE records SET title='改名'", "ATTACH DATABASE ':memory:' AS another"} {
		if _, err := s.queryRecallSQL(t.Context(), peer.ID, bad); err == nil {
			t.Fatal("非只读成员查询成功", bad)
		}
	}
	table, err := s.queryRecallSQL(t.Context(), peer.ID, `WITH birds AS (SELECT * FROM records WHERE body LIKE '%飞%起来%')
	SELECT b.id,sh.version,sh.conversation_name,sh.save_run_id FROM birds b JOIN shares sh ON sh.artifact_id=b.id AND b.version=sh.version`)
	if err != nil || len(table.Rows) != 1 || table.Rows[0][0] != saved.Result.ID || table.Rows[0][1] != int64(1) || table.Rows[0][2] != room.Name || table.Rows[0][3] != saved.ID {
		t.Fatal(table, err)
	}
	table, err = s.queryRecallSQL(t.Context(), peer.ID, "SELECT count(*) FROM records WHERE body IN ('私下更新的第二版','私有笔记')")
	if err != nil || table.Rows[0][0] != int64(0) {
		t.Fatal(table, err)
	}
	table, err = s.queryRecallSQL(t.Context(), author.ID, "SELECT count(*) FROM records WHERE mine AND body IN ('私下更新的第二版','私有笔记')")
	if err != nil || table.Rows[0][0] != int64(2) {
		t.Fatal(table, err)
	}
}
