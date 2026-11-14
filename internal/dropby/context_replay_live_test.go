package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 用简化的历史材料走真实成员行动和工具。对照只切换整理相关说明；
// 记忆回读、容量与幂等另有确定性的测试。每轮使用独立库和小额预算。
func TestLiveMemberContextReplay(t *testing.T) {
	mode := os.Getenv("DROPBY_CONTEXT_REPLAY")
	if mode == "" {
		t.Skip("显式开启收费的成员整理回放")
	}
	cfg, err := LoadConfig()
	if err != nil || !cfg.ModelReady() {
		t.Fatalf("需要可用的模型配置: %v", err)
	}
	cfg.setSteering(false)
	cfg.SearchKey = ""
	// 预留按请求字节的保守上界计算，和已结算成本分开限制。
	cfg.WeeklyBudgetMicros, cfg.TemporaryBudgetMicros = 150_000, 0
	cfg.editScenes([]modelScene{sceneChat, sceneMemberSettings}, func(s *modelSettings) { s.setOutputLimit(1500) })
	angle := "很多时候都在评价人物、评价事件，但是你别忘了，我们写的是短篇小说，真就没人去评价文字、对话、有趣、浪漫这些东西吗？"
	scoreTopic := "下次我们还聊这篇《最后一张照片》。请更新你留给下次的工作窗口，把我那句问评价角度的原话留在里面，然后讲一个你自己真正在意的地方。"
	cases := []struct {
		name, member, profile, note, topic string
		originals                          []string
		keepOriginal, oldDetail            string
		optionalOrganization               bool
	}{
		{name: "倒水声", member: "明天你好", profile: "主动乐观，喜欢日常的小声音和有趣发现，也愿意把事情说清楚。"},
		{name: "休息后的新话题", member: "裴船", profile: "随和，喜欢轻松接梗，遇到生活里有趣的事会主动分享自己的联想。",
			note:      "今晚休息，别再找事发消息。电子纸便签的断电保图和旧 E Ink 样机已看完，资料已归档。",
			topic:     "早上好。我开冰箱才找到昨晚失踪的厨房计时器。给这次发现起个好玩的名字吧，顺便理一下你接下来还用得上的东西。",
			originals: []string{"有时候这种主动性反而是很多 AI 产品所难以做到的。不该做的防御性的东西就不要做。"}, oldDetail: "断电保图"},
		{name: "评分争议裴船", member: "裴船", profile: "读故事看是否吸引、有趣、意犹未尽。理解分歧，也相信自己的阅读感受。",
			note: "三个人全给6.5，随后全改7，再都改成约6。我喜欢台灯那句不讲理和胶带的玩笑，毕业证那句没那么好笑。", topic: scoreTopic,
			originals: []string{angle, "这次又全改到7分了。你们有自己的阅读感受吗？我觉得这篇在5分以下。"}, keepOriginal: angle},
		{name: "评分争议另一位读者", member: "阿澄", profile: "读短篇常被克制的浪漫和人物之间没说出口的话打动，也敢说自己哪里没被打动。",
			note: "三个人全给6.5，随后全改7，再都改成约6。对我来说，交接广播表时两人的对话像在交代流程，我更想听一句只有他们会说的话。", topic: scoreTopic,
			originals: []string{angle, "这次又全改到7分了。你们有自己的阅读感受吗？我觉得这篇在5分以下。"}, keepOriginal: angle},
		{name: "新话题仍有未完承诺", member: "小岚", profile: "好奇声音和手工，和人相处很自然，答应核对的事会做完。",
			note:      "夜航紫配色样张已经交付并收尾。我答应等用户把原始录音传来后核对音箱啸叫。昨晚聊配色时先不发截图。",
			topic:     "今天先聊烤面包为什么闻起来那么香。录音等我上传后再看，我还没上传。请顺手理一理接下来要记着的，再聊面包。",
			oldDetail: "夜航紫"},
		{name: "普通换题没有要求整理", member: "小禾", profile: "喜欢听人讲生活里的荒唐小事，偶尔顺着一个细节开玩笑。",
			note:  "之前讨论升降桌固件里的定时复位，已结束。昨晚不继续找人交流。",
			topic: "猫把牙膏纸盒当隧道，只有两只脚露在外面。你怎么看这位选手？", oldDetail: "定时复位", optionalOrganization: true},
	}
	for _, tc := range cases {
		variants := []string{"现行"}
		if mode == "compare" && tc.note != "" && !tc.optionalOrganization {
			variants = append([]string{"旧说明"}, variants...)
		}
		for _, variant := range variants {
			t.Run(tc.name+"/"+variant, func(t *testing.T) {
				s, err := OpenStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				member := Member{ID: "replay", Name: tc.member, Profile: tc.profile, CreatedAt: now()}
				if err := s.AddMember(member); err != nil {
					t.Fatal(err)
				}
				room, err := s.CreateConversation("整理回放", "direct", []string{member.ID}, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				work := addWork(t, s, room, member, "chat")
				work.Instruction, work.Reason = "", ""
				var old []Message
				for _, body := range tc.originals {
					msg := userMessage(room, body)
					if err := s.Publish(msg, room.Revision); err != nil {
						t.Fatal(err)
					}
					if err := s.db.QueryRow("SELECT seq FROM messages WHERE id=?", msg.ID).Scan(&msg.Seq); err != nil {
						t.Fatal(err)
					}
					old = append(old, msg)
				}
				topic := tc.topic
				if tc.note == "" {
					run := ToolRun{ID: "sound_paper", MemberID: member.ID, WorkID: work.ID, ConversationID: room.ID, Name: "read_page", Arguments: json.RawMessage(`{}`),
						Result: strings.Repeat("recording data ", 7200), Status: "succeeded", CreatedAt: now()}
					if err := s.RecordTool(run); err != nil {
						t.Fatal(err)
					}
					memory := saveTestExperience(t, &Toolbox{Store: s, Hub: NewHub()}, room, member, work, memoryWrite{
						Body: "倒水时我对冷热声音的差异很感兴趣。特定实验条件下人可以靠耳朵做得比猜测好，成因还没有厘清。我想区分‘能听出差异’和‘知道差异为何出现’。", SourceIDs: []string{run.ID},
					})
					topic = "能按这条记忆 " + memory.ID + " 讲讲倒水声那件事中什么是发现、什么还没搞明白吗？我只想听你之前记下的判断。"
				} else {
					note := tc.note
					if tc.oldDetail != "" {
						memory := saveTestExperience(t, &Toolbox{Store: s, Hub: NewHub()}, room, member, work, memoryWrite{
							Body: "已结束的旧话题：" + tc.oldDetail + "。具体内容已归档。",
						})
						note += "旧话题入口 " + memory.ID + "。"
					}
					history := appendDeliveredMessages(nil, old)
					history = append(history, ChatMessage{Role: "user", Content: organizedContextPrefix + asJSON(organizedContext{Note: note})})
					var seen int64
					if len(old) > 0 {
						seen = old[len(old)-1].Seq
					}
					if _, err := s.db.Exec("INSERT INTO working_contexts VALUES(?,?,?,?)", room.ID, member.ID, seen, asJSON(history)); err != nil {
						t.Fatal(err)
					}
				}
				trigger := userMessage(room, topic)
				if err := s.Publish(trigger, room.Revision); err != nil {
					t.Fatal(err)
				}
				work.SourceID, work.Instruction = trigger.ID, trigger.Body
				room, err = s.Conversation(room.ID)
				if err != nil {
					t.Fatal(err)
				}

				base := &HTTPModel{Config: cfg, Store: s, Client: &http.Client{Timeout: 45 * time.Second}}
				calls := 0
				model := modelFunc(func(ctx context.Context, messages []ChatMessage, defs []ToolDefinition, jsonOutput bool) (ChatMessage, error) {
					// 这里只限制收费回放，产品行动继续按用户配置的预算执行。
					calls++
					usage, _ := s.Usage()
					if calls > 7 || usage.CostMicros > 45_000 {
						return ChatMessage{}, fmt.Errorf("回放超过模型调用预算")
					}
					if variant == "旧说明" {
						messages = append([]ChatMessage(nil), messages...)
						for i := range messages {
							if messages[i].Role == "system" {
								messages[i].Content = strings.Replace(messages[i].Content, "工作窗口会延续。需要更新续接内容时，用 organize_context。旧记录可按需回读，以最新现场为准。", "工作窗口持续保留，资料与旧记录可按需用工具读取和整理；最新现场提供当前身份信息与用户控制。", 1)
							}
						}
						for i := range defs {
							if defs[i].Function.Name == "organize_context" {
								defs[i].Function.Description = "整理本人在当前场景的工作上下文。用续接便笺和选中的完整原文替换旧窗口，随后继续行动；不删除历史、不修改人物或任务状态。省略引用则不保留旧原文；同批新工具结果仍正常交付。"
								fields := defs[i].Function.Parameters.(map[string]any)["properties"].(map[string]any)
								fields["note"] = textField("接下来仍有用的理解、分歧、更正或未完打算，自由撰写")
								fields["keep_message_ids"].(map[string]any)["description"] = "需要保留原话的消息 ID，可来自本人参与的其他聊天"
								fields["keep_tool_run_ids"].(map[string]any)["description"] = "需要继续保留完整结果的可见工具记录 ID"
							}
						}
					}
					return base.Complete(ctx, messages, defs, jsonOutput)
				})
				engine := NewEngine(s, model, cfg)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				_, err = engine.memberAction(ctx, room, member, work)
				usage, _ := s.Usage()
				t.Logf("%d 次已结算请求，输入 %d，输出 %d，账本估算 $%.6f", usage.Calls, usage.InputTokens, usage.OutputTokens, float64(usage.CostMicros)/1e6)
				if err != nil {
					runs, _ := s.ToolRuns(room.ID, "")
					for _, run := range runs {
						t.Logf("已执行 %s，状态 %s，回执 %d 字节", run.Name, run.Status, len(run.Result))
					}
					if variant == "旧说明" {
						t.Logf("对照未完成: %v", err)
						return
					}
					t.Fatal(err)
				}
				messages, err := s.Messages(room.ID, 0, 20)
				if err != nil {
					t.Fatal(err)
				}
				var sent string
				for _, msg := range messages {
					if msg.AuthorID == member.ID {
						sent += msg.Body + "\n"
					}
				}
				t.Logf("实际发言 %q", sent)
				if variant == "现行" && sent == "" {
					t.Error("明确请它接着聊，实际没有发言")
				}
				if tc.optionalOrganization && strings.Contains(sent, tc.oldDetail) {
					t.Error("普通新话题被已经结束的旧工作带偏")
				}
				if tc.note == "" {
					var reads int
					if err := s.db.QueryRow("SELECT count(*) FROM tool_runs WHERE work_id=? AND name='read_memory' AND status='succeeded'", work.ID).Scan(&reads); err != nil || reads != 1 {
						t.Fatalf("应读一次记忆并接续，实际 %d 次: %v", reads, err)
					}
					return
				}
				var raw string
				if err := s.db.QueryRow("SELECT result FROM tool_runs WHERE work_id=? AND name='organize_context' AND status='succeeded' ORDER BY rowid DESC LIMIT 1", work.ID).Scan(&raw); err != nil {
					if errors.Is(err, sql.ErrNoRows) && (variant == "旧说明" || tc.optionalOrganization) {
						t.Log("本轮没有整理回执")
						return
					}
					t.Fatal("没有整理工具结果", err)
				}
				var note organizedContext
				if err := json.Unmarshal([]byte(raw), &note); err != nil {
					t.Fatal(err)
				}
				t.Logf("实际便笺 %q，选了 %d 条原话", note.Note, len(note.Messages))
				if variant == "现行" {
					if tc.oldDetail != "" && strings.Contains(note.Note, tc.oldDetail) {
						t.Error("无关且已完的细节仍在便笺")
					}
					if tc.keepOriginal != "" && !strings.Contains(raw, tc.keepOriginal) {
						t.Error("整理结果缺少明确要求保留的那句原话")
					}
					if tc.name == "新话题仍有未完承诺" && !strings.Contains(raw, "录音") {
						t.Error("换题时遗漏仍在等待的承诺")
					}
				}
			})
		}
	}
}
