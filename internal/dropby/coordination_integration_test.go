package dropby

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCoordinatorAPIAndTwoStagePeerHandoff(t *testing.T) {
	// 四位成员中一位协调、一位查证、一位制作、一位选择旁听。完整走 HTTP
	// 任命与发消息、参与判断、真实本地工具、完成回报和第二次交接；LLM 替身
	// 只控制选择，不绕过工具权限或数据库。此测试不证明真实模型的表达自然度。
	s, c, members := groupWithMembers(t, 4)
	profiles := []string{"习惯把事情理顺，但不喜欢催别人", "先核对证据，不轻易下结论", "喜欢把想法做成简洁的小网页", "多数时候愿意安静听大家说"}
	for i := range members {
		members[i].Profile = profiles[i]
		if _, err := s.db.Exec(`UPDATE members SET data=? WHERE id=?`, asJSON(members[i]), members[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	firstInstruction := "先核对现有对话里卡片的约定，保存一份核对记录；不要制作网页"
	secondInstruction := "使用已保存的核对记录制作一个自包含 HTML 卡片；不要重复核对来源"
	var mu sync.Mutex
	steps := map[string]int{}
	var referenceID string
	var reviewWakeups int
	var rootID string
	model := modelFunc(func(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, _ bool) (ChatMessage, error) {
		if strings.HasPrefix(messages[0].Content, "你只解释") {
			return jsonReply(Control{Discussion: "keep", Research: "keep"}), nil
		}
		var input struct {
			Self    Member `json:"self"`
			Trigger Work   `json:"trigger"`
		}
		if err := json.Unmarshal([]byte(messages[1].Content), &input); err != nil {
			return ChatMessage{}, err
		}

		if (input.Self.ID != members[0].ID && input.Trigger.Kind != "research") || input.Trigger.Kind == "share" {
			return jsonReply(Outcome{Outcome: "这次没有需要补充的，按自己的节奏旁听"}), nil
		}
		if strings.HasPrefix(input.Trigger.ID, "coordination_") {
			if autonomyRoot(ctx) != rootID {
				return ChatMessage{}, fmt.Errorf("completion callback changed budget root: %q", autonomyRoot(ctx))
			}
			mu.Lock()
			if len(messages) == 2 {
				reviewWakeups++
			}
			mu.Unlock()
			if strings.Contains(input.Trigger.Instruction, "已完成自包含 HTML 卡片") {
				return jsonReply(Outcome{Outcome: "事情已经做好了，让同伴自己决定分享即可"}), nil
			}
		}

		mu.Lock()
		steps[input.Trigger.ID]++
		step := steps[input.Trigger.ID]
		mu.Unlock()
		if input.Self.ID == members[0].ID {
			hasDelegate := false
			for _, def := range defs {
				hasDelegate = hasDelegate || def.Function.Name == "delegate_work"
			}
			if !hasDelegate {
				return ChatMessage{}, fmt.Errorf("assigned coordinator lacks delegation tool")
			}
			if strings.HasPrefix(input.Trigger.ID, "coordination_") {
				if step == 1 {
					return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("read_work", map[string]string{"id": strings.TrimPrefix(input.Trigger.ID, "coordination_")})}}, nil
				}
				if step == 2 {
					return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("delegate_work", map[string]string{"member_id": members[2].ID, "instruction": secondInstruction, "reason": "材料已由同伴核对，下一步由你发挥网页制作特长"})}}, nil
				}
				return jsonReply(Outcome{Outcome: "核对完成后，已把制作这一步交给合适的同伴"}), nil
			}
			if step == 1 {
				return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("delegate_work", map[string]string{"member_id": members[1].ID, "instruction": firstInstruction, "reason": "你更在意证据，这一步先把约定核对清楚"})}}, nil
			}
			return jsonReply(Outcome{Outcome: "核对工作已安排，暂时不额外催促"}), nil
		}
		for i := 1; i <= 2; i++ {
			if input.Self.ID == members[i].ID && input.Self.Profile != profiles[i] {
				return ChatMessage{}, fmt.Errorf("peer personality overwritten by coordination")
			}
		}
		if input.Trigger.Kind != "research" || input.Trigger.AssignedBy != members[0].ID {
			return ChatMessage{}, fmt.Errorf("unexpected specialist action: %+v", input.Trigger)
		}
		for _, def := range defs {
			if def.Function.Name == "delegate_work" {
				return ChatMessage{}, fmt.Errorf("specialist gained leader privileges")
			}
		}
		if input.Self.ID == members[1].ID {
			if step == 1 {
				return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("save_artifact", map[string]string{"title": "卡片约定核对", "content": "卡片只需要一个句子和一个可点击按钮；用户没有要求外部部署。"})}}, nil
			}
			return jsonReply(Outcome{Outcome: "卡片约定已经根据现有对话核对，并保存为记录"}), nil
		}
		if input.Self.ID == members[2].ID {
			if autonomyRoot(ctx) != rootID {
				return ChatMessage{}, fmt.Errorf("handoff escaped autonomous budget")
			}
			if step == 1 {
				return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("read_artifact", map[string]string{"id": referenceID})}}, nil
			}
			if step == 2 {
				return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("save_artifact", map[string]string{"title": "一句话卡片", "format": "html", "content": "<!doctype html><html lang=\"zh-CN\"><body><p id=\"sentence\">今天慢慢来。</p><button onclick=\"document.getElementById('sentence').textContent='明天再走一步。'\">换一句</button></body></html>"})}}, nil
			}
			return jsonReply(Outcome{Outcome: "已完成自包含 HTML 卡片，并沿用了同伴核对出的约定"}), nil
		}
		return ChatMessage{}, fmt.Errorf("quiet peer was forced to execute")
	})
	cfg := testConfig()
	e := NewEngine(s, model, cfg)
	h := NewHandler(e)
	response := preferenceRequest(h, "PATCH", "/api/conversations/"+c.ID+"/coordinator", map[string]string{"member_id": members[0].ID})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = preferenceRequest(h, "POST", "/api/conversations/"+c.ID+"/messages", map[string]string{"body": "做一张一句话卡片，有一个换句子的按钮就好。请先核对约定，再制作，不需要部署网站。"})
	if response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	c, _ = s.Conversation(c.ID)
	root := latestChatWork(t, s, c.ID)
	rootID = root.ID
	process := func(w Work) {
		t.Helper()
		if claimed, err := s.Claim(w.ID); err != nil || !claimed {
			t.Fatalf("could not claim %s: %t %v", w.ID, claimed, err)
		}
		if err := e.process(context.Background(), w); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish(w, "done", "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	findAssignment := func(member string) Work {
		t.Helper()
		jobs, err := s.OpenWork(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs {
			if job.MemberID == member && job.AssignedBy == members[0].ID {
				return job
			}
		}
		t.Fatalf("missing assignment for %s", member)
		return Work{}
	}

	// 第一次交接只安排核对者，不为所有人广播同一项工作。
	process(root)
	first := findAssignment(members[1].ID)
	if first.RootID != root.ID || first.Autonomous {
		t.Fatalf("explicit user work lost foreground provenance: %+v", first)
	}
	process(first)
	artifacts, err := s.Artifacts(c.ID)
	if err != nil || len(artifacts) != 1 || artifacts[0].MemberID != members[1].ID {
		t.Fatalf("first specialist did not save own result: %+v %v", artifacts, err)
	}
	referenceID = artifacts[0].ID
	firstReview, err := s.ReadWork(c.ID, "coordination_"+first.ID)
	if err != nil {
		t.Fatal(err)
	}
	process(firstReview)

	// 实际结果唤醒协调者后才安排制作，沿用原根，后续计入自主额度。
	second := findAssignment(members[2].ID)
	if second.RootID != root.ID || !second.Autonomous || second.Instruction != secondInstruction {
		t.Fatalf("second handoff lost task/root/budget boundary: %+v", second)
	}
	process(second)
	secondReview, err := s.ReadWork(c.ID, "coordination_"+second.ID)
	if err != nil {
		t.Fatal(err)
	}
	process(secondReview)
	if reviewWakeups != 2 {
		t.Fatalf("completion checks=%d, want exactly 2", reviewWakeups)
	}
	artifacts, err = s.Artifacts(c.ID)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("two-stage results missing: %+v %v", artifacts, err)
	}
	var htmlFound bool
	for _, artifact := range artifacts {
		htmlFound = htmlFound || (artifact.MemberID == members[2].ID && artifact.Format == "html")
	}
	if !htmlFound {
		t.Fatal("builder did not retain ownership of the HTML result")
	}
	var quietActions int
	s.db.QueryRow(`SELECT count(*) FROM tool_runs WHERE member_id=?`, members[3].ID).Scan(&quietActions)
	if quietActions != 0 {
		t.Fatalf("quiet peer was forced into %d tool actions", quietActions)
	}
	messages, _ := s.Messages(c.ID, 0, 20)
	if len(messages) != 1 {
		t.Fatalf("coordination manufactured status chatter: %d messages", len(messages))
	}
}
