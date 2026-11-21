package dropby

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

//go:embed recall_encoder.py
var recallEncoder string

// 每个数据目录按需启动一个本地模型。子进程只接收本次候选；不持有业务库连接。
type recallEncoderProcess struct {
	closed bool
	cache  string
	gate   chan struct{}
	cmd    *exec.Cmd
	in     *json.Encoder
	out    *json.Decoder
}
type semanticHit struct {
	Index int
	Match string
	Score float64 // 归一化向量的余弦值，只在本地排序中使用。
}

func (p *recallEncoderProcess) start() error {
	python := os.Getenv("DROPBY_RECALL_PYTHON")
	if python == "" {
		python = "python3"
	}
	cmd := exec.Command(python, "-u", "-c", recallEncoder, p.cache)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s：%w", python, err)
	}
	p.cmd = cmd
	p.in = json.NewEncoder(stdin)
	p.out = json.NewDecoder(stdout)
	return nil
}
func (p *recallEncoderProcess) stop() {
	if p.cmd != nil {
		p.cmd.Process.Kill()
		p.cmd.Wait()
		p.cmd = nil
	}
}
func (p *recallEncoderProcess) close() {
	if p.gate == nil {
		return
	}
	p.gate <- struct{}{}
	defer func() { <-p.gate }()
	p.closed = true
	p.stop()
}
func newRecallEncoder(dir string) recallEncoderProcess {
	return recallEncoderProcess{cache: filepath.Join(dir, "recall-vectors.sqlite"), gate: make(chan struct{}, 1)}
}
func (p *recallEncoderProcess) rank(ctx context.Context, query string, records []recallRecord) ([]semanticHit, error) {
	select {
	case p.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-p.gate }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.closed {
		return nil, fmt.Errorf("数据目录已关闭")
	}
	if p.cmd == nil {
		if err := p.start(); err != nil {
			return nil, err
		}
	}
	proc := p.cmd.Process
	// 一次只有一个请求在管道中。取消时丢弃这个进程，防止旧响应被交给下一位成员。
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { proc.Kill(); close(done) })
	docs := make([][2]string, len(records))
	for i, r := range records {
		docs[i] = [2]string{r.Title, r.body}
	}
	err := p.in.Encode(map[string]any{"query": query, "documents": docs})
	var response struct {
		Hits  []semanticHit
		Error string
	}
	if err == nil {
		err = p.out.Decode(&response)
	}
	if !stop() {
		<-done
	}
	if ctx.Err() != nil {
		p.stop()
		return nil, ctx.Err()
	}
	if err != nil {
		p.stop()
		return nil, fmt.Errorf("编码进程不可用，请检查 Python 环境和本地 BGE-M3 权重：%w", err)
	}
	if response.Error != "" {
		return nil, fmt.Errorf("本地编码：%s", response.Error)
	}
	seen := make([]bool, len(records))
	if len(response.Hits) != len(records) {
		return nil, fmt.Errorf("语义候选数量不符")
	}
	for _, h := range response.Hits {
		if h.Index < 0 || h.Index >= len(records) || seen[h.Index] {
			return nil, fmt.Errorf("语义候选位置无效")
		}
		seen[h.Index] = true
	}
	return response.Hits, nil
}
