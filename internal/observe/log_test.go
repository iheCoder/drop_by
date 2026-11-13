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

func int64ptr(v int64) *int64 { return &v }
