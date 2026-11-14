package upstream

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Failure 保留故障分类和供应商 error.message，凭据在请求边界脱敏。
// 只在外部只读/模型请求边界重试；不重放工具，也不重跑整项工作。
type Failure struct {
	Cause      error // 保留取消/超时等原始语义；费用状态不能改变调度器的恢复判断。
	Detail     string
	Stage      string
	Status     int
	RetryAfter time.Duration
	Temporary  bool
	NotSent    bool // 仅在可证明连接未建立/请求尚未开始时释放预留。
}

func (e *Failure) Unwrap() error { return e.Cause }

func (e *Failure) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s：HTTP %d %s", e.Stage, e.Status, e.Detail)
	}
	return e.Stage
}
func Retry(ctx context.Context, request func() error) error {
	for attempt := 0; ; attempt++ {
		err := request()
		var failure *Failure
		if !errors.As(err, &failure) || !failure.Temporary || attempt >= 2 {
			return err
		}
		delay := time.Duration(1<<attempt) * time.Second
		if failure.RetryAfter > delay {
			delay = failure.RetryAfter
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
