package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 实验只移除新增提示形成基线；模型、人物、输入和真实工具保持一致。
// 不要求模型解释思考过程，观察真实调用和已发布消息。单次样本不证明统计效果。
const explorationTrialPrompt = "交流与行动中，依据你的人格特质，结合当前情境和对方的意思，自主决定是否进一步探索、探索什么、深入到什么程度。对于值得探索的事情，使用可用工具去了解、尝试、验证或创造，把实际发现和结果带回后续的判断与行动。\n"

type explorationTrialModel struct {
	base     Model
	baseline bool
	calls    int
	dir      string
}

func (m *explorationTrialModel) Complete(ctx context.Context, input []ChatMessage, defs []ToolDefinition, jsonOutput bool) (ChatMessage, error) {
	// 上限仅限制这次收费实验，不注入成员提示，也不修改产品行动循环。
	if m.calls >= 6 {
		return ChatMessage{}, fmt.Errorf("experiment call limit")
	}
	m.calls++
	messages := append([]ChatMessage(nil), input...)
	if m.baseline {
		for i := range messages {
			if messages[i].Role == "system" {
				messages[i].Content = strings.ReplaceAll(messages[i].Content, explorationTrialPrompt, "")
			}
		}
	}
	b, _ := json.MarshalIndent(messages, "", "  ")
	if err := os.WriteFile(filepath.Join(m.dir, fmt.Sprintf("input-%02d.json", m.calls)), b, 0600); err != nil {
		return ChatMessage{}, err
	}
	out, err := m.base.Complete(ctx, messages, defs, jsonOutput)
	b, _ = json.MarshalIndent(out, "", "  ")
	_ = os.WriteFile(filepath.Join(m.dir, fmt.Sprintf("output-%02d.json", m.calls)), b, 0600)
	return out, err
}

// 显式开启才调用真实模型；每组独立数据库，无正式聊天、资料或预算写入。
// 四对场景分别观察日常克制、人格允许的深究、开放发现和实际成果制作。
func TestLiveExplorationPrompt(t *testing.T) {
	dir := os.Getenv("DROPBY_EXPLORATION_TRIAL_DIR")
	if dir == "" {
		t.Skip("需要独立目录显式开启收费实验")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey == "" || cfg.SearchKey == "" {
		t.Fatal("需要模型与搜索配置")
	}
	cfg.WeeklyBudgetMicros = 300_000
	cfg.TemporaryBudgetMicros = 0
	cases := []struct{ name, profile, message string }{
		{"casual", "随性，喜欢轻松接梗，日常相处重在陪伴，遇到认真问题也愿意动脑。", "猫又把我的杯子碰倒了，今天第三次，服了😂"},
		{"curious", "对日常细节也有强烈的研究欲，喜欢主动追究原因、找资料弄明白，再用具体发现和朋友聊天。", "猫又把我的杯子碰倒了，今天第三次，服了😂"},
		{"discover", "喜欢人物的小心思、日常关系里的错位和幽默，选故事看重自己的阅读感受。", "最近想找个短篇读读，有点好玩、人物有意思就好。你有什么想分享的？"},
		{"create", "喜欢亲手做东西理解问题，重视交互中的实际感受，偏爱简单直观的小作品。", "我在琢磨一个呼吸节奏的小页面，圆圈变大变小也许比倒计时舒服，但光这么说好像感受不出来。"},
	}
	for _, tc := range cases {
		for _, baseline := range []bool{true, false} {
			name := tc.name + "-new"
			if baseline {
				name = tc.name + "-baseline"
			}
			t.Run(name, func(t *testing.T) {
				d := filepath.Join(dir, name)
				if _, err := os.Stat(d); err == nil {
					t.Fatal("不复用旧实验目录")
				}
				s, err := OpenStore(d)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				m := Member{ID: "trial", Name: "小岚", Profile: tc.profile, Tools: []string{"search", "read_page"}, CreatedAt: now()}
				if err = s.AddMember(m); err != nil {
					t.Fatal(err)
				}
				c, err := s.CreateConversation("探索实验", "direct", []string{m.ID}, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				msg := userMessage(c, tc.message)
				if err = s.Publish(msg, c.Revision); err != nil {
					t.Fatal(err)
				}
				model := &explorationTrialModel{base: &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 2 * time.Minute}}, baseline: baseline, dir: d}
				e := NewEngine(s, model, cfg)
				w := Work{ID: newID("work"), ConversationID: c.ID, MemberID: m.ID, Kind: "chat", SourceID: msg.ID, CreatedAt: now()}
				if err := s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error { return insertWork(tx, w) }); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				_, actErr := e.act(ctx, c, m, w, "")
				messages, _ := s.Messages(c.ID, 0, 100)
				runs, _ := s.ToolRuns(c.ID, "")
				usage, _ := s.Usage()
				after, _ := s.Member(m.ID)
				report := map[string]any{"case": map[string]string{"name": tc.name, "profile": tc.profile, "message": tc.message}, "baseline": baseline, "calls": model.calls, "messages": messages, "tools": runs, "usage": usage, "error": fmt.Sprint(actErr), "profile_unchanged": after.Profile == m.Profile}
				b, _ := json.MarshalIndent(report, "", "  ")
				if err = os.WriteFile(filepath.Join(d, "report.json"), b, 0600); err != nil {
					t.Fatal(err)
				}
				if actErr != nil {
					t.Error(actErr)
				}
				if after.Profile != m.Profile {
					t.Error("人格被改写")
				}
				t.Logf("calls=%d tools=%d cost=$%.6f", model.calls, len(runs), float64(usage.CostMicros)/1e6)
			})
		}
	}
}
