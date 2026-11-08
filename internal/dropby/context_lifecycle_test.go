package dropby

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

// 从首次输入、整理、故障断点、完成续接到下一行动检查同一条链路。
// 背景确实存在于库中，但成员没有主动查询/选择的内容不能自行回来。
func TestOrganizedWindowSurvivesFailureAndNextAction(t *testing.T) {
	s, c, m := fixture(t)
	old := userMessage(c, "旧综艺触发原话")
	if err := s.Publish(old, c.Revision); err != nil {
		t.Fatal(err)
	}
	w := addWork(t, s, c, m, "research")
	w.SourceID, w.Instruction = old.ID, "旧研究的行动来由"
	box := &Toolbox{Store: s, Hub: NewHub()}
	saveTestExperience(t, box, c, m, w, memoryWrite{Body: "旧综艺触发原话的匹配记忆"})
	state := "本轮休息的旧状态"
	if err := s.recordObservation(c, m.ID, 0, &state, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTool(ToolRun{ID: "old_material", WorkID: w.ID, ConversationID: c.ID, MemberID: m.ID, Name: "read_page", Arguments: json.RawMessage(`{}`), Result: "旧资料正文", Status: "succeeded", CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	note := "只保留我和同伴还没有达成共识，接着聊近况。"
	failure := errors.New("模拟整理后的网络故障")
	calls := 0
	assertClean := func(ms []ChatMessage) {
		t.Helper()
		raw := asJSON(ms)
		for _, text := range []string{old.Body, w.Instruction, "旧资料正文", "old_material", state, "匹配记忆"} {
			if strings.Contains(raw, text) {
				t.Errorf("背景重新注入: %s", text)
			}
		}
		if !strings.Contains(raw, note) {
			t.Error("成员自己的整理丢失")
		}
	}
	e := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		calls++
		if calls == 1 {
			raw := asJSON(ms)
			if strings.Count(raw, old.Body) != 1 || !strings.Contains(raw, w.Instruction) {
				t.Fatal("首次行动原话重复或来由缺失")
			}
			for _, text := range []string{"匹配记忆", "old_material", state} {
				if strings.Contains(raw, text) {
					t.Fatal("首次输入自动注入旧背景", text)
				}
			}
			return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{toolCall("organize_context", map[string]string{"note": note})}}, nil
		}
		assertClean(ms)
		return ChatMessage{}, failure
	}), testConfig())
	if _, err := e.memberAction(context.Background(), c, m, w); !errors.Is(err, failure) {
		t.Fatalf("应保留故障断点: %v", err)
	}
	cp, err := s.loadAction(w.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertClean(cp.Messages)
	// 模拟新 Engine 从同一断点恢复：不重新交付该行动的旧来由。
	resumed := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		assertClean(ms)
		return jsonReply(Outcome{Outcome: "本轮休息的最终决定", State: &state}), nil
	}), testConfig())
	if _, err := resumed.memberAction(context.Background(), c, m, w); err != nil {
		t.Fatal(err)
	}
	saved, err := s.loadWorkingContext(c.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertClean(saved.Messages)
	if strings.Contains(asJSON(saved.Messages), "本轮休息的最终决定") {
		t.Fatal("运行决定进入续接窗口")
	}
	var recorded string
	if err := s.db.QueryRow(`SELECT reason FROM participation WHERE work_id=? AND member_id=? AND action<>'error' ORDER BY rowid DESC LIMIT 1`, w.ID, m.ID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded, "本轮休息的最终决定") {
		t.Fatal("移除上下文时丢掉了行动审计")
	}
	// 真正的新消息仍交付；跟聊沿用旧根触发时，已整理掉的旧原话不回流。
	nextMessage := userMessage(c, "现在你怎么看？")
	if err := s.Publish(nextMessage, c.Revision); err != nil {
		t.Fatal(err)
	}
	next := addWork(t, s, c, m, "followup")
	next.SourceID = old.ID
	next.Instruction = old.Body
	nextEngine := NewEngine(s, modelFunc(func(_ context.Context, ms []ChatMessage, _ []ToolDefinition, _ bool) (ChatMessage, error) {
		assertClean(ms)
		raw := asJSON(ms)
		if strings.Count(raw, nextMessage.Body) != 1 || strings.Contains(raw, "本轮休息的最终决定") {
			t.Fatal("新消息重复/遗漏，或旧休息决定回流")
		}
		return jsonReply(Outcome{}), nil
	}), testConfig())
	if _, err := nextEngine.memberAction(context.Background(), c, m, next); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Member(m.ID)
	if current.Profile != m.Profile {
		t.Fatal("成员特质被修改")
	}
}

// 最终响应已经生成但尚未发布时，取消仍保留完整断点；恢复只消费一次。
func TestPendingOutcomeSurvivesCancellationButNotCompletedWindow(t *testing.T) {
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "followup")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		cancel()
		return jsonReply(Outcome{Message: "已经准备好的回复", Outcome: "本次行动完成"}), nil
	}), testConfig())
	if _, err := e.memberAction(ctx, c, m, w); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消语义丢失: %v", err)
	}
	cp, err := s.loadAction(w.ID, m.ID)
	if err != nil || !strings.Contains(asJSON(cp.Messages), "已经准备好的回复") {
		t.Fatal("取消丢失待发布响应", err)
	}
	resumed := NewEngine(s, modelFunc(func(context.Context, []ChatMessage, []ToolDefinition, bool) (ChatMessage, error) {
		t.Error("恢复不应重新生成最终响应")
		return ChatMessage{}, errors.New("不应调用")
	}), testConfig())
	if _, err := resumed.memberAction(context.Background(), c, m, w); err != nil {
		t.Fatal(err)
	}
	messages, _ := s.Messages(c.ID, 0, -1)
	if len(messages) != 1 || messages[0].Body != "已经准备好的回复" {
		t.Fatal("回复丢失或重复")
	}
	saved, _ := s.loadWorkingContext(c.ID, m.ID)
	if strings.Contains(asJSON(saved.Messages), "本次行动完成") {
		t.Fatal("完成结果仍常驻")
	}
}

type cancelRequestTransport struct {
	cancel context.CancelFunc
	calls  int
}

func (t *cancelRequestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls++
	// 模拟请求头已发出之后取消，此时不能宣称请求免费。
	if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
		trace.WroteHeaders()
	}
	t.cancel()
	return nil, r.Context().Err()
}

// 同时验证控制流与账本，避免修正费用时又把取消变成可重试故障。
func TestCancellationKeepsCauseAndCorrectReservation(t *testing.T) {
	for _, phase := range []string{"before_send", "after_send", "deadline"} {
		t.Run(phase, func(t *testing.T) {
			s, _, _ := fixture(t)
			cfg := testConfig()
			model := &HTTPModel{Store: s, Config: cfg}
			call, err := s.reserveCall(context.Background(), cfg, 10000, "OpenAI", "gpt-6.1-sol", "")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := &cancelRequestTransport{cancel: cancel}
			client := &http.Client{Transport: transport}
			expected := error(context.Canceled)
			if phase == "before_send" {
				cancel()
			}
			if phase == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer stop()
				expected = context.DeadlineExceeded
			}
			attempts := 0
			err = retryRequest(ctx, func() error {
				attempts++
				return requestJSON(ctx, client, "http://unused/responses", "", []byte(`{}`), &map[string]any{})
			})
			if !errors.Is(err, expected) || attempts != 1 {
				t.Fatalf("取消/超时被改写或重试: %v attempts=%d", err, attempts)
			}
			if phase != "after_send" && transport.calls != 0 {
				t.Fatal("已取消请求仍交给 transport")
			}
			if err := model.releaseUnsent(call, err); err != nil {
				t.Fatal(err)
			}
			s.finishUsageCall(call)
			var status string
			var cost int64
			if err := s.db.QueryRow(`SELECT status,cost_micros FROM usage_calls WHERE id=?`, call.id).Scan(&status, &cost); err != nil {
				t.Fatal(err)
			}
			if phase == "after_send" {
				if status != "unknown" {
					t.Fatal("已发出取消被当作免费", status)
				}
			} else if status != "settled" || cost != 0 {
				t.Fatal("未发出预留未释放", status, cost)
			}
		})
	}
}
