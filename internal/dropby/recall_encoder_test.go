package dropby

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 编码器是离线依赖。未安装时保留可修正的错误，原词查找及精读仍能独立完成。
func TestRecallWithoutEncoder(t *testing.T) {
	s, c, m := fixture(t)
	t.Setenv("DROPBY_RECALL_PYTHON", filepath.Join(t.TempDir(), "missing-python"))
	if err := s.SystemMessage(c.ID, "sighting", "手稿末页写着蓝色邮筒"); err != nil {
		t.Fatal(err)
	}
	q := recallRequest{Query: "蓝色邮筒", Types: []string{"message"}}
	if _, err := s.recall(context.Background(), m.ID, q); err == nil || !strings.Contains(err.Error(), "sql") {
		t.Fatal("缺少依赖时应说明语义检索失败", err)
	}
	q = recallRequest{SQL: "SELECT id FROM records WHERE body LIKE '%蓝色邮筒%'"}
	raw, err := s.recall(context.Background(), m.ID, q)
	if err != nil || !strings.Contains(asJSON(raw), "sighting") {
		t.Fatal("原词查找无法独立工作", raw, err)
	}
	q.SQL = "SELECT id FROM records WHERE body LIKE '%没写过的原话%'"
	raw, err = s.recall(context.Background(), m.ID, q)
	if err != nil || len(raw.(recallTable).Rows) != 0 {
		t.Fatal("空结果应正常返回", raw, err)
	}
}

// 原文字面命中也不会改变成员明确选择的向量搜索排序。
func TestRecallKeepsSemanticOrder(t *testing.T) {
	s, room, m := fixture(t)
	if err := s.SystemMessage(room.ID, "catalog", strings.Repeat("旧目录。", 100)+"灯具编号 K-41，登记名 Night Lens。"); err != nil {
		t.Fatal(err)
	}
	if err := s.SystemMessage(room.ID, "experiment", "复查时把透镜往后退，墙上的影子更深了。"); err != nil {
		t.Fatal(err)
	}
	// 新消息在候选首位。用固定分差重放同一语义现场，避免让测试依赖模型权重或机器。
	encoder := filepath.Join(t.TempDir(), "encoder")
	script := `#!/bin/sh
while read -r request; do
  echo '{"hits":[{"index":0,"score":0.71,"match":"复查时把透镜往后退，墙上的影子更深了。"},{"index":1,"score":0.70,"match":"旧目录。"}]}'
done
`
	if err := os.WriteFile(encoder, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROPBY_RECALL_PYTHON", encoder)
	for _, tc := range []struct{ query, id, evidence string }{
		{"K-41 K-41 我想找那次透镜退后、影子更深的测量，编号可能记错。", "experiment", "影子更深"},
		{"K-41", "experiment", "影子更深"},
		{"我记得登记名是「Night Lens」", "experiment", "影子更深"},
	} {
		page, err := s.recall(context.Background(), m.ID, recallRequest{Query: tc.query, Room: room.ID, Types: []string{"message"}, Author: "system", Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		hits := page.(map[string]any)["records"].([]recallRecord)
		if len(hits) != 1 || hits[0].ID != tc.id || !strings.Contains(hits[0].Match, tc.evidence) {
			t.Fatalf("查询 %q，期望原件 %s 和原文 %q，得到 %+v", tc.query, tc.id, tc.evidence, hits)
		}
	}
}

// 第一次请求已交给编码器时取消。下一位成员只接收自己请求的结果，
// 旧进程没有机会把前一位的片段写回新管道；测试无需安装真实模型。
func TestRecallCancelsWorkerBeforeReusingPipe(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	t.Setenv("RECALL_TEST_READY", ready)
	script := filepath.Join(dir, "encoder")
	body := `#!/bin/sh
read -r line
if [ ! -f "$RECALL_TEST_READY" ]; then
  touch "$RECALL_TEST_READY"
  read -r hold
else
  echo '{"hits":[{"index":0,"match":"本次记录"}]}'
fi
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROPBY_RECALL_PYTHON", script)
	proc := newRecallEncoder(dir)
	defer proc.close()
	records := []recallRecord{{body: "本次记录"}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := proc.rank(ctx, "第一次", records); result <- err }()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("假编码器未启动")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("取消未传给调用者", err)
	}
	next, finish := context.WithTimeout(context.Background(), 4*time.Second)
	defer finish()
	hits, err := proc.rank(next, "第二次", records)
	if err != nil || len(hits) != 1 || hits[0].Match != "本次记录" {
		t.Fatal("新请求没有独立结果", hits, err)
	}
}
