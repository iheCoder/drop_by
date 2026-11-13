package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDomainProfilesShareCreationAndEditVersions(t *testing.T) {
	// 两种创建输入共用一个响应夹具。只有提供设定的请求可以保存该领域。
	s, _, _ := fixture(t)
	calls := 0
	const profile = "我看地图爱追没标名字的小路，找人帮忙却很干脆。"
	domains := []DomainProfile{{Domain: "地图", Profile: "偏爱把绕路的原因也画进去。"}}
	e := NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if reply, ok := replyIfWakeAdjust(messages); ok {
			return reply, nil
		}
		calls++
		if strings.Contains(messages[0].Content, "自主唤醒") {
			return jsonReply(map[string]any{"windows": []any{}}), nil
		}
		if len(messages) != 2 || !strings.Contains(messages[0].Content, "domain_profiles") {
			t.Error("创建调用缺少同时生成正文和提取领域的提示")
		}
		return jsonReply(map[string]any{"profile": profile, "domain_profiles": domains}), nil
	}), testConfig())
	h := NewHandler(e)
	create := func(direction string) Member {
		t.Helper()
		response := preferenceRequest(h, "POST", "/api/members", map[string]string{"name": "小尤", "direction": direction})
		var m Member
		if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &m) != nil {
			t.Fatal(response.Code, response.Body.String())
		}
		return m
	}
	bare := create("")
	m := create("给他一个地图画像，他偏爱把绕路的原因也画进去。")
	if calls != 4 || bare.Profile != profile || len(bare.DomainProfiles) != 0 || !slices.Equal(m.DomainProfiles, domains) {
		t.Fatal("创建正文或领域画像没有按输入保存", calls, bare, m)
	}

	// 资料编辑只改画像，共用旧版本检查，并能恢复完整快照。
	path := "/api/members/" + m.ID
	updated := []DomainProfile{{Domain: "地图", Profile: "偏爱用全角符号标出原路。"}}
	response := preferenceRequest(h, "PATCH", path, map[string]any{"domain_profiles": updated, "profile_revision": m.ProfileRevision})
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	response = preferenceRequest(h, "PATCH", path, map[string]any{"domain_profiles": []DomainProfile{}, "profile_revision": m.ProfileRevision})
	if response.Code != http.StatusConflict {
		t.Fatal("旧草稿可以覆盖新画像", response.Code)
	}
	current, err := s.Member(m.ID)
	if err != nil || current.Profile != profile || !slices.Equal(current.DomainProfiles, updated) {
		t.Fatal("单独改画像影响了人物正文或没有保存", current, err)
	}
	response = preferenceRequest(h, "PATCH", path, map[string]any{"domain_profiles": []DomainProfile{}, "profile_revision": current.ProfileRevision})
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	versions, err := s.profileHistory(m.ID)
	if err != nil || len(versions) != 3 || len(versions[0].DomainProfiles) != 0 || !slices.Equal(versions[1].DomainProfiles, updated) || !slices.Equal(versions[2].DomainProfiles, domains) {
		t.Fatal("完整画像版本历史丢失", versions, err)
	}
}

func TestDomainProfileToolReadsActualTextAndRefreshesAfterExplicitEdit(t *testing.T) {
	s, room, m := fixture(t)
	old := strings.Repeat("保留每次改变路线的理由。", 520) + " 原文尾部。"
	other := DomainProfile{Domain: "手绘", Profile: "愿意把不顺手的笔划也留着。"}
	profile := DomainProfile{Domain: "地图", Profile: old}
	h := NewHandler(NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if reply, ok := replyIfWakeAdjust(ms); ok {
			return reply, nil
		}
		t.Fatal("资料编辑以外的模型调用")
		return ChatMessage{}, nil
	}), testConfig()))
	response := preferenceRequest(h, "PATCH", "/api/members/"+m.ID, map[string]any{"domain_profiles": []DomainProfile{profile, other}})
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &m) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	var err error
	room, err = s.Conversation(room.ID)
	if err != nil {
		t.Fatal(err)
	}
	source := userMessage(room, "请把地图画像改为只标亲自走过的路。")
	if err = s.AddUserMessage(source, time.Hour); err != nil {
		t.Fatal(err)
	}
	w := latestChatWork(t, s, room.ID)
	const updated = "只标亲自走过的路。"
	readBefore := toolCall("read_domain_profile", map[string]string{"domain": "地图"})
	readAfter := toolCall("read_domain_profile", map[string]string{"domain": "地图"})
	calls := 0
	e := NewEngine(s, modelFunc(func(_ context.Context, messages []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		if reply, ok := replyIfWakeAdjust(messages); ok {
			return reply, nil
		}
		calls++
		if calls == 1 {
			// 现场公开本人可用的领域名。全文只在请求读取后交给本次行动。
			input := asJSON(messages)
			if !strings.Contains(input, `\"domain_names\":[\"地图\",\"手绘\"]`) || strings.Contains(input, old) {
				t.Error("首轮上下文的领域名称或全文交付范围不符")
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{readBefore,
				toolCall("apply_instruction", map[string]any{"source_id": source.ID, "preference_updates": []PreferenceUpdate{{
					MemberID: m.ID, SourceID: source.ID, ExpectedRevision: &m.ProfileRevision,
					DomainProfileUpdates: []DomainProfileUpdate{{Domain: "地图", Profile: updated}},
				}}}), readAfter,
			}}, nil
		}
		// 同批写入后，第二次读取必须拿到新正文和版本，旧读取仍可按原结果复核。
		want := map[string]struct {
			Profile  string
			Revision int
		}{readBefore.ID: {old, m.ProfileRevision}, readAfter.ID: {updated, m.ProfileRevision + 1}}
		for _, message := range messages {
			expected, ok := want[message.ToolCallID]
			if !ok {
				continue
			}
			var receipt struct {
				Status string `json:"status"`
				Result struct {
					Profile  string `json:"profile"`
					Revision int    `json:"profile_revision"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(message.Content), &receipt); err != nil || receipt.Status != "succeeded" || receipt.Result.Profile != expected.Profile || receipt.Result.Revision != expected.Revision {
				t.Error("实际工具回读没有完整交付对应版本的正文", err, receipt.Status, receipt.Result.Revision)
			}
			delete(want, message.ToolCallID)
		}
		if len(want) != 0 || !strings.Contains(asJSON(messages), `\"profile_revision\":2`) {
			t.Error("新现场或同批回读未交付")
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	if _, err := e.act(context.Background(), room, m, w, "继续交流"); err != nil || calls != 2 {
		t.Fatal("对话没有按原行动模型完成", calls, err)
	}
	saved, err := s.Member(m.ID)
	if err != nil || saved.Profile != m.Profile || saved.ProfileRevision != m.ProfileRevision+1 || !slices.Equal(saved.DomainProfiles, []DomainProfile{{Domain: "地图", Profile: updated}, other}) {
		t.Fatal("领域修改影响了未指定的设置", saved, err)
	}
	// 个人活动读的是同一个成员的名称索引，换场景也能主动找到画像。
	personal, err := e.personalContext(saved, Work{Kind: "explore"})
	if err != nil || !strings.Contains(asJSON(personal["self"]), `"domain_names":["地图","手绘"]`) || strings.Contains(asJSON(personal), updated) {
		t.Fatal("个人现场没有正确的领域索引", personal, err)
	}
}
