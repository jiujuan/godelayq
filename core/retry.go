package core

import (
	"errors"
	"math"
	"math/rand"
	"time"
)

// RetryPolicy 重试策略接口
type RetryPolicy interface {
	NextRetry(job *Job) time.Time
}

// PermanentError 是"这次失败重试也不会变好"的错误标记接口（TASK-E12 §3.1）。
//
// 它定义在 core 而不是 executor，依赖方向保持 executor → core：执行器把失败分类好带过来，
// core 只按接口读，不认识 ExitError 的任何字段。
//
// 与 RetryPolicy 的分工要写清，免得下一个人在这里加判断：
// 本接口管"要不要重试"，RetryPolicy（以及下面的 ExponentialBackoffRetry）只管"什么时候重试"。
// 退避算法里没有错误语义——它拿不到失败的原因，也不该拿。
type PermanentError interface {
	error
	Permanent() bool
}

// isPermanentFailure 判断一次失败被标成了"重试没有意义"。
//
// 用 errors.As 而不是类型断言：处理函数常在错误外面再包一层描述
// （fmt.Errorf("...: %w", err)），包过之后仍然要认得它。
// 没有实现 PermanentError 的错误一律返回 false，也就是维持现状——
// 是否重试继续由 MaxRetries 决定。
func isPermanentFailure(err error) bool {
	var permanent PermanentError
	return errors.As(err, &permanent) && permanent.Permanent()
}

// ExponentialBackoffRetry 指数退避
type ExponentialBackoffRetry struct {
	MaxDelay time.Duration
}

func (e *ExponentialBackoffRetry) NextRetry(job *Job) time.Time {
	// 指数退避: delay * 2^retryCount + jitter
	delay := job.RetryDelay * time.Duration(math.Pow(2, float64(job.RetryCount)))
	if e.MaxDelay > 0 && delay > e.MaxDelay {
		delay = e.MaxDelay
	}
	// 添加随机抖动(0-20%)避免雪崩
	jitter := time.Duration(rand.Float64() * 0.2 * float64(delay))
	return time.Now().Add(delay + jitter)
}
