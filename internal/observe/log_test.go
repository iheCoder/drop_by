package observe

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttemptsShareCallAndRetryLink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observe.sqlite")
	log, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	go log.Serve()
	defer log.Close()

	runID := log.BeginRun(Run{WorkID: "work_1", ParentWorkID: "work_parent", MemberID: "m1", ConversationID: "room", Kind: "chat", Trigger: "message", Execution: 2})
	ctx := WithTrace(context.Background(), log, runID)
	tr := Current(ctx)
	tr.SetCall("call_1")
	tr.Write(Step{ID: "compact_1", Kind: "compact", CallID: tr.Call(), Status: "succeeded", Input: "压缩前的完整请求", Output: "压缩结果"})
	tr.SetCompact("compact_1")
	tr.Write(Step{ID: "model_1", Kind: "model", CallID: tr.Call(), ParentID: tr.Compact(), Attempt: 1, Status: "failed", Output: "timeout", CostStatus: "unknown", ReservedMicros: int64ptr(84000)})
	tr.Write(Step{ID: "model_2", Kind: "model", CallID: tr.Call(), ParentID: tr.Compact(), RetryOf: "model_1", Attempt: 2, Status: "succeeded", CostStatus: "settled", CostMicros: int64ptr(120000)})
	tr.End("done", "")
	log.Sync()

	db, err := OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runs, gaps, err := ListRuns(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 0 || len(runs) != 1 || runs[0].WorkID != "work_1" || runs[0].Execution != 2 || runs[0].ParentWorkID != "work_parent" || runs[0].Status != "done" {
		t.Fatalf("runs=%+v gaps=%+v", runs, gaps)
	}
	steps, err := ListSteps(db, runID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(steps)
	if strings.Contains(string(raw), "压缩前的完整请求") {
		t.Fatalf("列表带出了快照正文: %s", raw)
	}
	if len(steps) != 3 || steps[1].RetryOf != "" || steps[2].RetryOf != "model_1" || steps[2].CallID != "call_1" || steps[0].CallID != "call_1" || steps[2].ParentID != "compact_1" {
		t.Fatalf("steps=%+v", steps)
	}
	if steps[1].CostMicros != nil || steps[1].ReservedMicros == nil || *steps[1].ReservedMicros != 84000 || steps[1].CostStatus != "unknown" {
		t.Fatalf("未知费用被写成了数字: %+v", steps[1])
	}
	// The list reports whether a snapshot exists without returning its payload.
	if !steps[0].HasSnapshot {
		t.Fatal("压缩步骤应有快照")
	}
	input, output, err := Snapshot(db, "compact_1")
	if err != nil || input != "压缩前的完整请求" || output != "压缩结果" {
		t.Fatalf("snapshot %q %q %v", input, output, err)
	}
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM members`).Scan(&n); err == nil {
		t.Fatal("只读观测库不应带上业务表")
	}
}

func TestDroppedObservationIsMarked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observe.sqlite")
	log, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	log.ch = make(chan writeOp, 1)
	for i := 0; i < 3; i++ {
		log.WriteStep(Step{ID: "s", Kind: "model", Status: "started"})
	}
	go log.Serve()
	log.Close()
	db, err := OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, gaps, err := ListRuns(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) == 0 || !strings.Contains(gaps[0].Note, "可能不完整") {
		t.Fatalf("gaps=%+v", gaps)
	}
}

func TestMissingDBIsNotCreatedByTheReader(t *testing.T) {
	_, err := OpenRead(filepath.Join(t.TempDir(), "missing.sqlite"))
	if err == nil {
		t.Fatal("不存在的观测库不应被读端创建")
	}
}

func TestNilTraceDoesNothing(t *testing.T) {
	if Current(context.Background()) != nil || Current(nil) != nil {
		t.Fatal("空上下文没有观测")
	}
	var tr *Trace
	tr.SetCall("x")
	tr.SetModel("x")
	tr.SetCompact("x")
	tr.SetTool("x")
	tr.SetActor("m", "r")
	tr.End("done", "")
	tr.Write(Step{ID: "x"})
	if tr.Call() != "" || tr.Model() != "" || tr.Compact() != "" || tr.Tool() != "" {
		t.Fatal("空观测不应返回步骤")
	}
}

func TestParallelBranchesDoNotShareCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observe.sqlite")
	log, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	go log.Serve()
	defer log.Close()
	runID := log.BeginRun(Run{WorkID: "group", Kind: "chat", Trigger: "message"})
	base := WithTrace(context.Background(), log, runID)
	done := make(chan struct{})
	for _, member := range []string{"A", "B"} {
		member := member
		go func() {
			defer func() { done <- struct{}{} }()
			ctx := Branch(base)
			tr := Current(ctx)
			tr.SetActor(member, "room")
			tr.SetCall("call_" + member)
			tr.SetModel("model_" + member)
			tr.Write(Step{ID: "tool_" + member, Kind: "tool", ParentID: tr.Model(), CallID: tr.Call(), Status: "succeeded"})
		}()
	}
	<-done
	<-done
	log.Sync()
	db, err := OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	steps, err := ListSteps(db, runID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]StepView{}
	for _, step := range steps {
		got[step.MemberID] = step
	}
	if got["A"].CallID != "call_A" || got["A"].ParentID != "model_A" || got["B"].CallID != "call_B" || got["B"].ParentID != "model_B" {
		t.Fatalf("并行成员串线了: %+v", steps)
	}
}

func TestResumeStaysOpenUntilItFinishes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observe.sqlite")
	log, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	go log.Serve()
	defer log.Close()
	log.WriteStep(Step{ID: "tool_resume", RunID: "run", Kind: "tool", Status: "resume", Summary: "接着上次未完成的执行"})
	log.Sync()
	db, err := OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	steps, err := ListSteps(db, "run")
	if err != nil || len(steps) != 1 || steps[0].EndedAt != "" {
		t.Fatalf("恢复开始就被写成结束: %+v %v", steps, err)
	}
	log.WriteStep(Step{ID: "tool_resume", Status: "succeeded", Output: "done"})
	log.Sync()
	steps, err = ListSteps(db, "run")
	if err != nil || steps[0].Status != "succeeded" || steps[0].EndedAt == "" {
		t.Fatalf("结束后没有结束时间: %+v %v", steps, err)
	}
}

func TestRunPagesStayInsideTheFilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observe.sqlite")
	log, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	go log.Serve()
	defer log.Close()
	log.BeginRun(Run{ID: "run_old", WorkID: "work_room", ConversationID: "room", MemberID: "m", Kind: "chat"})
	log.Sync()
	log.BeginRun(Run{ID: "run_new", WorkID: "work_room", ConversationID: "room", MemberID: "m", Kind: "chat", Status: "running"})
	log.WriteRunEnd("run_new", "waiting_budget", "本周可用预算还剩 $0.080，本次调用需预留 $0.261")
	log.BeginRun(Run{ID: "run_personal", WorkID: "work_personal", RootID: "work_personal", MemberID: "m", Kind: "explore"})
	log.WriteStep(Step{ID: "sent", RunID: "run_personal", Kind: "message", Name: "send_message", Status: "sent", BusinessID: "message_1", TargetID: "room"})
	log.Sync()

	db, err := OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, err := ListRunPage(db, RunFilter{Conversation: "room", Limit: 1})
	if err != nil || len(first.Runs) != 1 || !first.HasEarlier || first.BudgetBlocked != 1 || first.Runs[0].ID != "run_new" {
		t.Fatalf("第一页 %+v %v", first, err)
	}
	second, err := ListRunPage(db, RunFilter{Conversation: "room", Limit: 1, BeforeAt: first.Runs[0].StartedAt, BeforeID: first.Runs[0].ID})
	if err != nil || len(second.Runs) != 1 || second.HasEarlier || second.Runs[0].ID != "run_old" || second.BudgetBlocked != 1 {
		t.Fatalf("翻页后范围变了 %+v %v", second, err)
	}
	personal, err := ListRunPage(db, RunFilter{Personal: true, Limit: 10})
	if err != nil || len(personal.Runs) != 1 || personal.Runs[0].ID != "run_personal" {
		t.Fatalf("个人活动 %+v %v", personal, err)
	}
	own, err := ListRunPage(db, RunFilter{Work: "work_personal", Limit: 10})
	if err != nil || len(own.Outputs) != 1 || own.Outputs[0].ConversationID != "room" || own.Outputs[0].Count != 1 {
		t.Fatalf("活动产出 %+v %v", own, err)
	}
	links, err := Deliveries(db, "room")
	if err != nil || len(links) != 1 || links[0].MessageID != "message_1" || links[0].ConversationID != "" || links[0].WorkID != "work_personal" {
		t.Fatalf("消息来源 %+v %v", links, err)
	}
}

func TestMemberFilterIncludesStepsWhenRunHasNoMember(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observe.sqlite")
	log, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	go log.Serve()
	defer log.Close()
	log.BeginRun(Run{ID: "run_group", WorkID: "work_group", ConversationID: "room", Kind: "followup"})
	log.WriteStep(Step{ID: "lin_model", RunID: "run_group", Kind: "model", Name: "gemini", Status: "succeeded", MemberID: "lin"})
	log.WriteStep(Step{ID: "cheng_model", RunID: "run_group", Kind: "model", Name: "gemini", Status: "succeeded", MemberID: "cheng"})
	log.BeginRun(Run{ID: "run_lin", WorkID: "work_lin", MemberID: "lin", Kind: "explore"})
	log.Sync()

	db, err := OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lin, err := ListRunPage(db, RunFilter{Member: "lin", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, run := range lin.Runs {
		got[run.ID] = true
	}
	if !got["run_group"] || !got["run_lin"] || len(lin.Runs) != 2 {
		t.Fatalf("林的群内执行和个人执行都要在: %+v", lin.Runs)
	}
	cheng, err := ListRunPage(db, RunFilter{Member: "cheng", Limit: 10})
	if err != nil || len(cheng.Runs) != 1 || cheng.Runs[0].ID != "run_group" {
		t.Fatalf("程只应看到自己参与的群执行 %+v %v", cheng.Runs, err)
	}
	other, err := ListRunPage(db, RunFilter{Member: "other", Limit: 10})
	if err != nil || len(other.Runs) != 0 {
		t.Fatalf("没有步骤的角色不应看到这次执行 %+v %v", other.Runs, err)
	}
}

func int64ptr(v int64) *int64 { return &v }
