package dropby

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 同一个成员在群聊中仍能主动回看私聊、保留原话、形成自己的记忆；
// 场景切换不是遗忘或禁止分享。来源仍记录实际房间，不能伪造为本群发言。
func TestMemberCarriesKnownContextAcrossRooms(t *testing.T) {
	s, room, people := groupWithMembers(t, 2)
	m := people[0]
	private, _ := s.CreateConversation("本人私聊", "direct", []string{m.ID}, time.Hour)
	msg := userMessage(private, strings.Repeat("完整原文", 600)+"末尾更正")
	if err := s.Publish(msg, private.Revision); err != nil {
		t.Fatal(err)
	}
	w := addWork(t, s, room, m, "followup")
	box := NewEngine(s, nil, testConfig()).Tools
	got, err := box.perform(context.Background(), room, m, w, toolCall("read_history", map[string]any{"conversation_id": private.ID, "id": msg.ID, "length": 10000}))
	if err != nil || !strings.Contains(asJSON(got), "末尾更正") {
		t.Fatalf("跨场景原文: %v %v", got, err)
	}
	note, err := box.remember(context.Background(), room, m, w, memoryWrite{Body: "我可以自行决定如何使用这段交流", SourceIDs: []string{msg.ID}})
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.readMemory(m.ID, private.ID, note.(map[string]any)["memory_id"].(string), nil)
	if err != nil || !strings.Contains(asJSON(view), "末尾更正") {
		t.Fatalf("跨场景记忆及依据: %v %v", view, err)
	}
	// 未参与者无法凭消息 ID 读取这段交流；这不是限制知情成员的表达。
	if _, err := box.perform(context.Background(), room, people[1], w, toolCall("read_history", map[string]any{"conversation_id": private.ID, "id": msg.ID})); err == nil {
		t.Fatal("未参与者取得了原文")
	}
}
