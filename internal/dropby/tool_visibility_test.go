package dropby

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPrivateMemoryCannotLeakThroughSharedToolHistory(t *testing.T) {
	// A 与 B 在同一群里，但自己的经历仍分别拥有。A 的记忆读取不能因为留下
	// 工具日志，就出现在 B 的自动上下文或被 B 用 read_tool_result 绕过权限读走。
	s, c, members := groupWithMembers(t, 2)
	a, b := members[0], members[1]
	wa := addWork(t, s, c, a, "research")
	wb := addWork(t, s, c, b, "research")
	e := NewEngine(s, nil, testConfig())
	secret := "只有阿甲的经历记录应携带这段标记"
	for _, name := range []string{"remember", "read_memory", "search_memory"} {
		run := ToolRun{ID: "private_" + name, WorkID: wa.ID, ConversationID: c.ID, MemberID: a.ID, Name: name,
			Arguments: json.RawMessage(`{}`), Result: asJSON(map[string]string{"body": secret}), Status: "succeeded", CreatedAt: now()}
		if err := s.RecordTool(run); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Tools.perform(context.Background(), c, b, wb, toolCall("read_tool_result", map[string]string{"id": run.ID})); err == nil {
			t.Fatalf("同群成员借工具日志读到了 %s", name)
		}
	}

	// 第二层读取同样需要保护。否则 A 自己读过私有结果后，B 能读到这次复制日志。
	copyRaw, err := e.Tools.Execute(context.Background(), c, a, wa, toolCall("read_tool_result", map[string]string{"id": "private_read_memory"}))
	if err != nil || !strings.Contains(copyRaw, secret) {
		t.Fatalf("本人应能回读自己的经历结果: %s %v", copyRaw, err)
	}
	var copyRecord struct {
		ID string `json:"tool_run_id"`
	}
	if err := json.Unmarshal([]byte(copyRaw), &copyRecord); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Tools.perform(context.Background(), c, b, wb, toolCall("read_tool_result", map[string]string{"id": copyRecord.ID})); err == nil {
		t.Fatal("经过 read_tool_result 复制后绕过了私有记忆权限")
	}

	// 搜索、网页、成果和工作依然是同群共享的实际行动；权限修复不能把协作切断。
	for _, name := range []string{"search", "read_page", "save_artifact", "read_work"} {
		run := ToolRun{ID: "shared_" + name, WorkID: wa.ID, ConversationID: c.ID, MemberID: a.ID, Name: name,
			Arguments: json.RawMessage(`{}`), Result: asJSON(map[string]string{"body": "可共享的实际行动"}), Status: "succeeded", CreatedAt: now()}
		if err := s.RecordTool(run); err != nil {
			t.Fatal(err)
		}
		result, err := e.Tools.perform(context.Background(), c, b, wb, toolCall("read_tool_result", map[string]string{"id": run.ID}))
		if err != nil || !strings.Contains(asJSON(result), "可共享的实际行动") {
			t.Fatalf("非记忆行动被错误隔离: %s %v", name, err)
		}
	}

	inputA, err := e.contextFor(c, a, Work{Kind: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	inputB, err := e.contextFor(c, b, Work{Kind: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(asJSON(inputA["tool_runs"]), secret) || !strings.Contains(asJSON(inputA["tool_runs"]), copyRecord.ID) {
		t.Fatal("自己的历史回忆只应保留回查入口，不能自动重放可能已经修正的旧笔记")
	}
	if strings.Contains(asJSON(inputB["tool_runs"]), secret) || !strings.Contains(asJSON(inputB["tool_runs"]), "shared_search") {
		t.Fatal("自动上下文泄露同伴经历，或丢失共享行动")
	}
}
