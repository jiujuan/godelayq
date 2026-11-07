package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestExponentialBackoffRetry_FirstRetry(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 10 * time.Minute,
	}

	base := time.Now()
	job := &Job{
		RetryCount: 0,
		RetryDelay: 1 * time.Minute,
	}

	// 第1次重试: 1m * 2^0 + jitter = ~1m (with 0-20% jitter)
	next := policy.NextRetry(job)

	// Should be at least 1 minute in the future
	assert.True(t, next.After(base.Add(1*time.Minute)),
		"First retry should be at least 1 minute in the future")

	// Should not exceed 1m + 20% jitter = 1.2m
	assert.True(t, next.Before(base.Add(1*time.Minute+13*time.Second)),
		"First retry should not exceed 1m + 20% jitter")
}

func TestExponentialBackoffRetry_SecondRetry(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 10 * time.Minute,
	}

	base := time.Now()
	job := &Job{
		RetryCount: 1,
		RetryDelay: 1 * time.Minute,
	}

	// 第2次重试: 1m * 2^1 = 2m + jitter
	next := policy.NextRetry(job)

	assert.True(t, next.After(base.Add(2*time.Minute)),
		"Second retry should be at least 2 minutes in the future")

	// Should not exceed 2m + 20% jitter = 2.4m
	assert.True(t, next.Before(base.Add(2*time.Minute+30*time.Second)),
		"Second retry should not exceed 2m + 20% jitter")
}

func TestExponentialBackoffRetry_ThirdRetry(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 10 * time.Minute,
	}

	base := time.Now()
	job := &Job{
		RetryCount: 2,
		RetryDelay: 1 * time.Minute,
	}

	// 第3次重试: 1m * 2^2 = 4m + jitter
	next := policy.NextRetry(job)

	assert.True(t, next.After(base.Add(4*time.Minute)),
		"Third retry should be at least 4 minutes in the future")

	assert.True(t, next.Before(base.Add(5*time.Minute)),
		"Third retry should not exceed 5 minutes")
}

func TestExponentialBackoffRetry_FourthRetry(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 10 * time.Minute,
	}

	base := time.Now()
	job := &Job{
		RetryCount: 3,
		RetryDelay: 1 * time.Minute,
	}

	// 第4次重试: 1m * 2^3 = 8m + jitter
	next := policy.NextRetry(job)

	assert.True(t, next.After(base.Add(8*time.Minute)),
		"Fourth retry should be at least 8 minutes in the future")

	assert.True(t, next.Before(base.Add(10*time.Minute)),
		"Fourth retry should not exceed 10 minutes")
}

func TestExponentialBackoffRetry_MaxDelayLimit(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 10 * time.Minute,
	}

	base := time.Now()
	job := &Job{
		RetryCount: 10, // 理论上应该是 1m * 2^10 = 1024m，但应被限制在10m
		RetryDelay: 1 * time.Minute,
	}

	next := policy.NextRetry(job)

	// Should be capped at MaxDelay (10m) + jitter (max 2m)
	assert.True(t, next.After(base.Add(10*time.Minute)),
		"Retry should be at least MaxDelay in the future")

	assert.True(t, next.Before(base.Add(13*time.Minute)),
		"Retry should not exceed MaxDelay + 20% jitter")
}

func TestExponentialBackoffRetry_NoMaxDelay(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 0, // No limit
	}

	base := time.Now()
	job := &Job{
		RetryCount: 5,
		RetryDelay: 1 * time.Minute,
	}

	// 1m * 2^5 = 32m + jitter
	next := policy.NextRetry(job)

	assert.True(t, next.After(base.Add(32*time.Minute)),
		"Retry without max delay should follow exponential growth")

	assert.True(t, next.Before(base.Add(40*time.Minute)),
		"Retry should not exceed calculated delay + jitter")
}

func TestExponentialBackoffRetry_SmallBaseDelay(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 1 * time.Hour,
	}

	base := time.Now()
	job := &Job{
		RetryCount: 0,
		RetryDelay: 10 * time.Second,
	}

	// 10s * 2^0 = 10s + jitter
	next := policy.NextRetry(job)

	assert.True(t, next.After(base.Add(10*time.Second)),
		"Retry with small base delay should work correctly")

	assert.True(t, next.Before(base.Add(13*time.Second)),
		"Retry should not exceed base delay + jitter")
}

func TestExponentialBackoffRetry_LargeBaseDelay(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 2 * time.Hour,
	}

	base := time.Now()
	job := &Job{
		RetryCount: 0,
		RetryDelay: 30 * time.Minute,
	}

	// 30m * 2^0 = 30m + jitter
	next := policy.NextRetry(job)

	assert.True(t, next.After(base.Add(30*time.Minute)),
		"Retry with large base delay should work correctly")

	assert.True(t, next.Before(base.Add(37*time.Minute)),
		"Retry should not exceed base delay + jitter")
}

func TestExponentialBackoffRetry_ProgressiveGrowth(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 1 * time.Hour,
	}

	job := &Job{
		RetryDelay: 1 * time.Minute,
	}

	var previousDelay time.Duration

	// Test that delays grow exponentially
	for i := 0; i < 5; i++ {
		job.RetryCount = i
		base := time.Now()
		next := policy.NextRetry(job)
		currentDelay := next.Sub(base)

		if i > 0 {
			// Each retry should be roughly double the previous (accounting for jitter)
			assert.True(t, currentDelay > previousDelay,
				"Retry delay should grow with each attempt")
		}

		previousDelay = currentDelay
	}
}

func TestExponentialBackoffRetry_JitterVariation(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 10 * time.Minute,
	}

	job := &Job{
		RetryCount: 2,
		RetryDelay: 1 * time.Minute,
	}

	// Call multiple times to verify jitter causes variation
	delays := make([]time.Duration, 10)
	base := time.Now()

	for i := 0; i < 10; i++ {
		next := policy.NextRetry(job)
		delays[i] = next.Sub(base)
	}

	// Check that not all delays are identical (jitter is working)
	allSame := true
	for i := 1; i < len(delays); i++ {
		if delays[i] != delays[0] {
			allSame = false
			break
		}
	}

	// Note: There's a small chance all could be same, but very unlikely
	assert.False(t, allSame,
		"Jitter should cause variation in retry delays")
}

// 编译期断言：内置策略必须满足 RetryPolicy 接口
var _ RetryPolicy = &ExponentialBackoffRetry{}

func TestExponentialBackoffRetry_ZeroRetryCount(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 10 * time.Minute,
	}

	job := &Job{
		RetryCount: 0,
		RetryDelay: 2 * time.Minute,
	}

	base := time.Now()
	next := policy.NextRetry(job)

	// 2m * 2^0 = 2m + jitter
	assert.True(t, next.After(base.Add(2*time.Minute)),
		"Zero retry count should use base delay")
}

func TestExponentialBackoffRetry_HighRetryCount(t *testing.T) {
	policy := &ExponentialBackoffRetry{
		MaxDelay: 30 * time.Minute,
	}

	job := &Job{
		RetryCount: 20, // Very high retry count
		RetryDelay: 1 * time.Second,
	}

	base := time.Now()
	next := policy.NextRetry(job)

	// Should be capped at MaxDelay
	assert.True(t, next.After(base.Add(30*time.Minute)),
		"High retry count should be capped at MaxDelay")

	assert.True(t, next.Before(base.Add(37*time.Minute)),
		"High retry count should not exceed MaxDelay + jitter")
}
