package dropby

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// requestFailure 保留故障分类和供应商 error.message，凭据在请求边界脱敏。
// 只在外部只读/模型请求边界重试；不重放工具，也不重跑整项工作。
type requestFailure struct {
	Detail     string
	Stage      string
	Status     int
	RetryAfter time.Duration
	Temporary  bool
}

func (e *requestFailure) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s：HTTP %d %s", e.Stage, e.Status, e.Detail)
	}
	return e.Stage
}
func retryRequest(ctx context.Context, request func() error) error {
	for attempt := 0; ; attempt++ {
		err := request()
		var failure *requestFailure
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

// 写入结果未知时不盲目重放，避免重复发布或创建任务。
var errUncertainTool = errors.New("上次工具执行结果尚未确认，未重复执行")
