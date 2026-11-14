package dropby

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// 通过真实创建和私聊行动验证领域画像能被成员自己读到并用于写作。
// 相同名字、题目和运行设置下保留两种对照，作品原文留给人判断。
func TestLivePersonalityCreativeWriting(t *testing.T) {
	if os.Getenv("DROPBY_LIVE_PERSONALITY_CREATIVE") != "1" {
		t.Skip("显式开启人格创作验收")
	}
	cfg, err := LoadConfig()
	if err != nil || !cfg.ModelReady() {
		t.Fatal("真实模型配置不可用", err)
	}
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg.WeeklyBudgetMicros = min(cfg.WeeklyBudgetMicros, 2_000_000)
	cfg.TemporaryBudgetMicros = 0
	cfg.SearchKey = ""
	cfg.setSteering(false)
	e := NewEngine(s, &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 3 * time.Minute}}, cfg)

	direction := `梁野很容易听出一本正经的话里藏着的荒唐。越是大家觉得规矩就该如此，他越手痒，想试出这规矩究竟管什么。他有点散漫，爱抄近路，愿意自嘲，也肯为好奇惹麻烦。他不服头衔，遇到真有本事的人却会主动请教。熟了会互相拆台，也会偏袒朋友。
给他一个叫「写作」的领域画像。他爱从平常处找一个好笑的矛盾，让人物真的动手，把后果一层层闹大，最后在一个回扣前文、能笑到自己身上的反转处结束。评故事时他看重这个观察能不能生出后面的事。`
	prompt := "写一篇虚构的第一人称小短文，讲一件发生在周末跳蚤市场的事。六百字左右。写好后存成 Markdown 作品，分享给我。"
	type sample struct {
		Mode    string    `json:"mode"`
		Stories []string  `json:"stories"`
		Replies []string  `json:"replies"`
		Tools   []ToolRun `json:"tools"`
		Error   string    `json:"error,omitempty"`
	}
	var member Member
	var samples []sample
	dir := os.Getenv("DROPBY_PERSONALITY_DATA_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	defer func() {
		usage, _ := s.Usage()
		report := map[string]any{"creation_model": cfg.Models.Scenes[sceneMemberSettings].Model, "chat_model": cfg.ChatModelName(), "direction": direction, "prompt": prompt, "member": member, "samples": samples, "usage": usage}
		raw, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			path := filepath.Join(dir, newID("personality_creative")+".json")
			err = os.WriteFile(path, raw, 0600)
			t.Log("创作验收原始记录", path)
		}
		if err != nil {
			t.Error(err)
		}
	}()

	h := NewHandler(e)
	created := preferenceRequest(h, "POST", "/api/members", map[string]any{"name": "梁野", "direction": direction})
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	if err = json.Unmarshal(created.Body.Bytes(), &member); err != nil {
		t.Fatal(err)
	}
	if member.Profile == "" || len(member.DomainProfiles) != 1 || member.DomainProfiles[0].Domain != "写作" {
		t.Fatalf("创建未保存预期的人格和写作画像：%+v", member.DomainProfiles)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	for _, mode := range []string{"人格与画像", "仅人格", "空白"} {
		current := member
		if mode != "人格与画像" {
			current.ID, current.DomainProfiles = newID("member"), nil
			if mode == "空白" {
				current.Profile = ""
			}
			if err := s.AddMember(current); err != nil {
				t.Fatal(err)
			}
		}
		// 三组从同一个题目开始。成员都通过正常私聊和实际工具交付作品。
		room, err := s.CreateConversation(current.Name, "direct", []string{current.ID}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddUserMessage(userMessage(room, prompt), time.Hour); err != nil {
			t.Fatal(err)
		}
		work := latestChatWork(t, s, room.ID)
		if claimed, err := s.Claim(work.ID); err != nil || !claimed {
			t.Fatal("无法启动私聊工作", err)
		}
		actionErr := e.process(ctx, work)
		messages, err := s.Messages(room.ID, 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		runs, err := s.ToolRuns(room.ID, work.ID)
		if err != nil {
			t.Fatal(err)
		}
		result := sample{Mode: mode, Tools: runs}
		if actionErr != nil {
			result.Error = actionErr.Error()
		}
		for _, message := range messages {
			if message.AuthorID == current.ID {
				result.Replies = append(result.Replies, message.Body)
			}
		}
		// 作品原文从正常下载入口回读，工具回执只用来找到实际保存的成果。
		for _, run := range runs {
			if run.Name != "save_artifact" || run.Status != "succeeded" {
				continue
			}
			var receipt struct {
				Download string `json:"download_url"`
			}
			if err := json.Unmarshal([]byte(run.Result), &receipt); err != nil || receipt.Download == "" {
				t.Fatal("找不到短文下载入口", err)
			}
			download := preferenceRequest(h, "GET", receipt.Download, nil)
			if download.Code != http.StatusOK {
				t.Fatal(download.Body.String())
			}
			result.Stories = append(result.Stories, download.Body.String())
		}
		samples = append(samples, result)
		if actionErr != nil || len(result.Stories) == 0 || len(result.Replies) == 0 {
			t.Errorf("%s未交付短文：%v", mode, actionErr)
			continue
		}
		if mode == "人格与画像" && !slices.ContainsFunc(runs, func(run ToolRun) bool {
			var read struct {
				DomainProfile
				Revision int `json:"profile_revision"`
			}
			return run.Name == "read_domain_profile" && run.Status == "succeeded" &&
				json.Unmarshal([]byte(run.Result), &read) == nil && read.DomainProfile == member.DomainProfiles[0] && read.Revision == member.ProfileRevision
		}) {
			t.Error("成员写作时没有读到已保存的画像全文和版本")
		}
		saved, err := s.Member(current.ID)
		if err != nil || saved.ProfileRevision != current.ProfileRevision {
			t.Fatalf("普通写作改动了人格版本：%v", err)
		}
	}
}
