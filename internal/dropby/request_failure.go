package dropby

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// requestFailure 保留足够的故障分类，不携带 URL、密钥或供应商响应正文。
// 只在外部只读/模型请求边界重试；不重放工具，也不重跑整项工作。
type requestFailure struct {
	Stage      string
	Status     int
	RetryAfter time.Duration
	Temporary  bool
}

func (e *requestFailure) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s：HTTP %d", e.Stage, e.Status)
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

// 这两个错误只暂停无法安全继续的具体调用，不决定角色应否发言。
var errUncertainTool = errors.New("上次工具执行结果尚未确认，未重复执行")
var errRepeatedTool = errors.New("相同工具请求已连续失败，保留现场等待改变方法")
