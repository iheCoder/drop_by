package dropby

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"unicode/utf8"

	"drop_by/internal/dropby/model/gemini"

	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestIsolatedLiangRecall(t *testing.T) {
	snapshot := os.Getenv("DROPBY_RECALL_TEST_SNAPSHOT")
	if snapshot == "" {
		t.Skip("只在指定实验目录运行")
	}
	// 唯一可写的业务库是临时目录的副本；不启动引擎调度，不读取真实模型配置。
	data, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err = os.WriteFile(filepath.Join(tmp, "dropby.sqlite"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mid string
	if err = s.db.QueryRow("select id from members where json_extract(data,'$.name')='梁野'").Scan(&mid); err != nil {
		t.Fatal(err)
	}
	m, err := s.Member(mid)
	if err != nil {
		t.Fatal(err)
	}
	scene, err := s.personalConversation(mid)
	if err != nil {
		t.Fatal(err)
	}
	w := addWork(t, s, scene, m, "explore")
	box := NewEngine(s, nil, testConfig()).Tools
	names := map[string]bool{}
	for _, d := range box.Definitions(m, scene, w) {
		names[d.Function.Name] = true
	}
	if !names["recall"] || names["read_artifact"] || names["read_history"] || names["search_memory"] || names["read_memory"] {
		t.Fatal("工具入口未合并")
	}
	run := func(name string, args any, out any) {
		t.Helper()
		raw, e := box.Execute(ctx, scene, m, w, toolCall(name, args))
		if e != nil {
			t.Fatal(e)
		}
		var r struct {
			Status string
			Result json.RawMessage
		}
		if e = json.Unmarshal([]byte(raw), &r); e != nil || r.Status != "succeeded" {
			t.Fatalf("%s %s %v", name, r.Result, e)
		}
		if e = json.Unmarshal(r.Result, out); e != nil {
			t.Fatal(e)
		}
	}
	var page struct{ Records []recallRecord }
	var hits []recallRecord
	query := "替客人指点吉凶却躲不开自身横祸，天生翱翔的猎手又因嘴馋失却腾空本领。我曾把这两桩糗事放一块儿调侃。"
	corpus, err := s.recallRows(ctx, mid, recallRequest{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, record := range corpus {
		counts[record.Kind]++
	}
	t.Logf("实际候选范围 %v", counts)
	start := time.Now()
	run("recall", map[string]any{"query": query}, &page)
	t.Logf("首次跨类型召回（含建索引） %v", time.Since(start))
	hits = page.Records
	if len(hits) == 0 || hits[0].ID != "artifact_6cecae5ca1f341d7c626b9dd" || hits[0].Version != 1 || hits[0].Evidence == "" {
		t.Fatalf("首项未命中晨稿 v1: %+v", hits)
	}
	h := hits[0]
	verifyLiangRecallRequests(t, s, scene, m, h, query)
	// 对照同题竞争稿、合写线索和其他题材。后两题在本次打分前写定。
	// 计时包括工具回执落库与检索；正式数据库和已有工作不参与写入。
	queries := []string{query}
	for _, test := range []struct {
		query, id string
		maxRank   int
	}{
		{"有位号称能参透天机的仁兄竟然阴沟翻船，羽族中的饭桶把看家本领给吃没了。哪篇把这俩倒霉蛋凑成一对？", h.ID, 3},
		{"替旁人指点迷津的行家，竟难料本人会栽跟头，回头还想叫别人赔一大笔。", "artifact_fa0ef2512931bca91c4dbfa9", 3},
		{"一边装可怜讨怜悯，一厢真行善却惹出麻烦。我曾把两间食肆的际遇相提并论。", "artifact_022677abf67b169af5786cfe", 3},
		{"肚腹闹腾，替皇家看管巨册的人竟叫稀世之物中途失踪。我曾写过哪篇？", "artifact_4a6b3f3ff20e82c09b3cd5ef", 5},
		{"守门人隔着好多赛季两回把比分送给敌方，我在哪次随笔拿他开过玩笑？", "artifact_5091e8720a88caaed6457b0a", 5},
	} {
		queries = append(queries, test.query)
		start = time.Now()
		run("recall", map[string]any{"query": test.query}, &page)
		rank := 0
		for i, r := range page.Records {
			if r.ID == test.id {
				rank = i + 1
				break
			}
		}
		t.Logf("意译 %q：目标第 %d，耗时 %v", test.query, rank, time.Since(start))
		if rank == 0 || rank > test.maxRank {
			t.Error("预定目标未进入候选前列")
		}
	}
	var warm []time.Duration
	for range 4 {
		for _, q := range queries {
			start = time.Now()
			run("recall", map[string]any{"query": q}, &page)
			warm = append(warm, time.Since(start))
		}
	}
	slices.Sort(warm)
	t.Logf("六种已预热查询，经真实工具往返 %d 次，中位数 %v，P95 %v", len(warm), (warm[len(warm)/2-1]+warm[len(warm)/2])/2, warm[(len(warm)*95+99)/100-1])
	open := func(v int) Artifact {
		var r struct{ Artifact Artifact }
		run("recall", map[string]any{"id": h.ID, "version": v, "length": 100000}, &r)
		return r.Artifact
	}
	orig := open(h.Version)
	digest := sha256.Sum256([]byte(orig.Content))
	var expected string
	if err = s.db.QueryRow("select content from artifacts where id=? and version=?", h.ID, h.Version).Scan(&expected); err != nil || orig.Content != expected {
		t.Fatal("原文读取不完整")
	}
	edit := orig.Content + "\n\n隔离实验修订：冰川深处封存着一封密信。"
	var saved struct{ Version int }
	run("save_artifact", map[string]string{"id": h.ID, "title": h.Title, "content": edit}, &saved)
	if saved.Version != 2 || open(2).Content != edit || open(1).Content != orig.Content {
		t.Fatal("改稿没有正确保留 v1")
	}
	run("recall", map[string]any{"query": "冰川深处封存着一封密信", "types": []string{"artifact"}}, &page)
	if len(page.Records) == 0 || page.Records[0].ID != h.ID || page.Records[0].Version != 2 {
		t.Fatal("新版本未进入语义检索")
	}
	var groupID string
	if err = s.db.QueryRow("select id from conversations where name='八卦闲聊群'").Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		ID string `json:"message_id"`
	}
	run("send_message", map[string]any{"conversation_id": groupID, "message": "隔离引用测试", "evidence": []string{h.Evidence}}, &sent)
	msgs, err := s.Messages(groupID, 0, 1)
	if err != nil || len(msgs) != 1 || msgs[0].ID != sent.ID || len(msgs[0].Artifacts) != 1 || msgs[0].Artifacts[0].ID != h.ID || msgs[0].Artifacts[0].Version != 1 {
		t.Fatal("消息没有固定到原版本", err)
	}
	var openedMessage struct{ Message Message }
	run("recall", map[string]any{"id": sent.ID}, &openedMessage)
	if len(openedMessage.Message.Artifacts) != 1 || openedMessage.Message.Artifacts[0].Version != 1 {
		t.Fatal("从回忆中打开消息遗失作品卡片")
	}
	// 从分享房间中另一人的身份打开 v1，再验证作者未分享的 v2 不可见。
	var peer string
	if err = s.db.QueryRow("select member_id from participants where conversation_id=? and member_id<>? limit 1", groupID, mid).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	other, err := s.Member(peer)
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.Conversation(groupID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{1, 2} {
		_, err := box.perform(ctx, room, other, w, toolCall("recall", map[string]any{"id": h.ID, "version": v}))
		if (v == 1) != (err == nil) {
			t.Fatalf("读者 v%d %v", v, err)
		}
	}
	// 同一入口实际检索并打开另外两类记录。逐字对照消息正文与成员自有记忆。
	for _, kind := range []string{"message", "memory"} {
		run("recall", map[string]any{"query": query, "types": []string{kind}, "limit": 1}, &page)
		hits = page.Records
		if len(hits) != 1 || hits[0].Kind != kind {
			t.Fatalf("%s 分类返回错误", kind)
		}
		var got map[string]json.RawMessage
		run("recall", map[string]any{"id": hits[0].ID, "length": 100000}, &got)
		var rec struct{ Body string }
		if err = json.Unmarshal(got[kind], &rec); err != nil {
			t.Fatal(err)
		}
		table := map[string]string{"message": "messages", "memory": "memories"}[kind]
		if err = s.db.QueryRow("select body from "+table+" where id=?", hits[0].ID).Scan(&expected); err != nil || expected != rec.Body {
			t.Fatalf("%s 原文不符", kind)
		}
	}
	t.Logf("晨稿 %s；v1 %d 字符 SHA256 %x；v2 正文修改成功；群消息作品卡仍是 v1；同伴可读 v1，不能读取 v2；消息和记忆也能检索并打开。", h.Title, len([]rune(orig.Content)), digest)
}

// 用固定的工具调用复现输入，不向供应商发送任何历史资料。复用实际工作循环、
// Gemini 请求编码以及本地向量模型。只捕获 HTTP 接收端收到的 functionResponse，
// 对照同一次工具调用在库里的快照，避免把整份请求或模拟用量误当作召回内容。
func verifyLiangRecallRequests(t *testing.T, s *Store, scene Conversation, m Member, target recallRecord, description string) {
	t.Helper()
	ledger, _, _ := fixture(t) // 模拟供应商只使用空的测试账本，成员库中的历史费用不参与。
	var expectedRows int
	var expectedBody string
	if err := s.db.QueryRow("SELECT count(*) FROM messages WHERE conversation_id IN (SELECT conversation_id FROM participants WHERE member_id=?)", m.ID).Scan(&expectedRows); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT content FROM artifacts WHERE id=? AND version=?", target.ID, target.Version).Scan(&expectedBody); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"short", map[string]any{"sql": `SELECT title,version,substr(body,instr(body,'单纯胖到飞不起来'),8) FROM records WHERE mine AND kind='artifact' AND created_at BETWEEN '2026-11-16T00:00' AND '2026-11-16T04:00' AND instr(body,'飞不起来')`}},
		{"wide", map[string]any{"sql": "SELECT body FROM records WHERE kind='message' ORDER BY seq"}},
		{"wide_full", map[string]any{"sql": "SELECT body FROM records WHERE kind='message' ORDER BY seq", "full": true}},
		{"one_long_cell", map[string]any{"sql": "SELECT group_concat(body,char(10)) FROM records WHERE kind='message'"}},
		{"artifact_full", map[string]any{"id": target.ID, "version": target.Version, "full": true}},
		{"semantic", map[string]any{"query": description, "limit": 1}},
	} {
		t.Run("wire_"+tc.name, func(t *testing.T) {
			// 每个现场从空工作窗口发起独立查询。历史资料和真实作品版本仍来自同一份快照。
			if _, err := s.db.Exec("DELETE FROM working_contexts WHERE conversation_id=? AND member_id=?", scene.ID, m.ID); err != nil {
				t.Fatal(err)
			}
			work := addWork(t, s, scene, m, "explore")
			var turn int
			var receipt struct {
				ID     string `json:"tool_run_id"`
				Status string
				Result json.RawMessage
			}
			var envelopeChars, requestBytes int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 供应商容量探测也在回环地址模拟，只用来放行这个能容纳的测试请求。
				// 下方报告的是实际收到的字符和字节，不采用这里的模拟 token 数。
				if strings.HasSuffix(r.URL.Path, ":countTokens") {
					io.WriteString(w, `{"totalTokens":40000}`)
					return
				}
				if !strings.HasSuffix(r.URL.Path, ":generateContent") {
					http.Error(w, "意外端点", 404)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var req struct {
					Contents []struct {
						Parts []struct {
							Response *struct {
								ID       string
								Response json.RawMessage
							} `json:"functionResponse"`
						}
					}
				}
				if err = json.Unmarshal(body, &req); err != nil {
					t.Error(err)
					return
				}
				turn++
				var part any
				switch turn {
				case 1:
					part = map[string]any{"functionCall": map[string]any{"id": "snapshot_lookup", "name": "recall", "args": tc.args}}
				case 2:
					requestBytes = len(body)
					for _, c := range req.Contents {
						for _, p := range c.Parts {
							if p.Response != nil && p.Response.ID == "snapshot_lookup" {
								envelopeChars = utf8.RuneCount(p.Response.Response)
								if err := json.Unmarshal(p.Response.Response, &receipt); err != nil {
									t.Error(err)
								}
							}
						}
					}
					part = map[string]any{"text": `{"outcome":"已核对"}`}
				default:
					http.Error(w, "额外的模型调用", 400)
					return
				}
				fmt.Fprintf(w, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[%s]}}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":20,"totalTokenCount":1020}}`, asJSON(part))
			}))
			defer server.Close()
			cfg := testConfig()
			cfg.Models.Providers["isolated"] = modelProvider{Protocol: gemini.Protocol, BaseURL: server.URL, apiKey: "test-only"}
			route := modelSettings{Provider: "isolated", Model: gemini.Hydrogen, Options: map[string]any{"generationConfig": map[string]any{"maxOutputTokens": 1024}}}
			if err := resolveProviderScene(&route, sceneExploration, gemini.Protocol, false, nil); err != nil {
				t.Fatal(err)
			}
			cfg.Models.Scenes[sceneExploration] = route
			e := NewEngine(s, &HTTPModel{Client: server.Client(), Store: ledger, Config: cfg}, cfg)
			if _, err := e.act(t.Context(), scene, m, work, "核对旧记录"); err != nil {
				t.Fatal(err)
			}
			if turn != 2 || receipt.Status != "succeeded" {
				t.Fatalf("工具回执未交付：轮次 %d，状态 %s", turn, receipt.Status)
			}
			var stored string
			if err := s.db.QueryRow("SELECT result FROM tool_runs WHERE id=?", receipt.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}

			// SQL 输出保留原始数字和字符串。各行先按 JSON 接收，按现场分别核对。
			var rows struct{ Rows [][]json.RawMessage }
			if err := json.Unmarshal(receipt.Result, &rows); err != nil {
				t.Fatal(err)
			}
			var info struct {
				Records   []recallRecord
				Artifact  Artifact
				Preview   bool
				TotalRows int `json:"total_rows"`
			}
			if err := json.Unmarshal(receipt.Result, &info); err != nil {
				t.Fatal(err)
			}

			switch tc.name {
			case "short":
				if len(rows.Rows) != 1 || !strings.Contains(string(receipt.Result), target.Title) || !strings.Contains(string(receipt.Result), "胖到飞不起来") {
					t.Fatal("短原文未直接返回")
				}
			case "wide", "one_long_cell":
				n := expectedRows
				if tc.name == "one_long_cell" {
					n = 1
				}
				if !info.Preview || info.TotalRows != n || len(rows.Rows) == 0 || len(receipt.Result) >= len(stored) {
					t.Fatal("未交付可读的部分原文")
				}
			case "wide_full":
				if info.Preview || len(rows.Rows) != expectedRows || string(receipt.Result) != stored {
					t.Fatal("明确请求的原始结果有遗漏")
				}
			case "artifact_full":
				if info.Artifact.Content != expectedBody || info.Artifact.Version != target.Version {
					t.Fatal("指定稿件原文未完全交付")
				}
			case "semantic":
				if len(info.Records) != 1 || info.Records[0].ID != target.ID || info.Records[0].Match == "" {
					t.Fatal("意译未交付晨稿与原句")
				}
			}
			t.Logf("快照 %d 字符；模型请求内 result %d 字符，含编号状态的完整回执 %d 字符；整份 HTTP 请求 %d 字节；交付 SQL 行 %d", utf8.RuneCountInString(stored), utf8.RuneCount(receipt.Result), envelopeChars, requestBytes, len(rows.Rows))
		})
	}
}
