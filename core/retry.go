package core

import (
	"math"
	"math/rand"
	"time"
)

// RetryPolicy 重试策略接口
type RetryPolicy interface {
	NextRetry(job *Job) time.Time
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

// FixedIntervalRetry 固定间隔重试
type FixedIntervalRetry struct {
	Interval time.Duration
}

func (f *FixedIntervalRetry) NextRetry(job *Job) time.Time {
	return time.Now().Add(f.Interval)
}
