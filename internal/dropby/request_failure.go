package dropby

import (
	"errors"

	"drop_by/internal/dropby/upstream"
)

type requestFailure = upstream.Failure

var retryRequest = upstream.Retry

// 写入结果未知时不盲目重放，避免重复发布或创建任务。
var errUncertainTool = errors.New("上次工具执行结果尚未确认，未重复执行")
