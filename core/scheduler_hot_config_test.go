package core

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// TestSetRetryPolicy_SwapsMaxDelay 读数口与新策略：换掉的确实是当前生效的那一条策略。
// nil 那一条守住"重载路径上一次没有取到值的调用不该清空现策略"。
func TestSetRetryPolicy_SwapsMaxDelay(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, &ExponentialBackoffRetry{MaxDelay: time.Minute}, nil)

	if got := scheduler.RetryPolicyMaxDelay(); got != time.Minute {
		t.Fatalf("initial max delay = %v, want 1m", got)
	}

	scheduler.SetRetryPolicy(&ExponentialBackoffRetry{MaxDelay: 5 * time.Second})
	if got := scheduler.RetryPolicyMaxDelay(); got != 5*time.Second {
		t.Fatalf("max delay after swap = %v, want 5s", got)
	}

	scheduler.SetRetryPolicy(nil) // nil 不改
	if got := scheduler.RetryPolicyMaxDelay(); got != 5*time.Second {
		t.Fatalf("max delay after nil call = %v, want 5s (nil must not clear it)", got)
	}
}

// TestRetryPolicyMaxDelay_NonBackoffPolicyReadsZero 读数口不该让调用方去断言接口类型：
// 自定义策略（测试与第三方装配方）读出来是 0，而不是 panic 或猜测值。
func TestRetryPolicyMaxDelay_NonBackoffPolicyReadsZero(t *testing.T) {
	scheduler := NewScheduler(newMockStore(), &mockRetryPolicy{delay: time.Second}, nil)
	if got := scheduler.RetryPolicyMaxDelay(); got != 0 {
		t.Fatalf("max delay of a non-backoff policy = %v, want 0", got)
	}
}

// TestSetRetryPolicy_ChangesScheduledDelay 是行为用例而不是读数用例：
// 换掉的策略必须真的改变下一次失败重试排下来的时刻——retryPolicy 换成原子字段之后，
// 只改字段不改 handleFailure 的读取点也能让上面的读数用例通过。
// 排期取值容忍 ±1ms 抖动，不断言精确时刻（本仓既有重试用例的口径）。
func TestSetRetryPolicy_ChangesScheduledDelay(t *testing.T) {
	store := newMockStore()
	first := &recordingRetryPolicy{delay: 10 * time.Millisecond}
	scheduler := NewScheduler(store, first, nil, quietLogger())

	job := &Job{
		ID:         "hot-retry",
		Name:       "flaky",
		Type:       "flaky",
		TriggerAt:  time.Now(),
		MaxRetries: 2,
		RetryDelay: 5 * time.Millisecond,
	}
	scheduler.handleFailure(job, errors.New("handler failed"))
	assertScheduledDelay(t, retriedJob(t, scheduler, "hot-retry"), first, "before the swap")

	second := &recordingRetryPolicy{delay: 30 * time.Millisecond}
	scheduler.SetRetryPolicy(second)
	scheduler.handleFailure(retriedJob(t, scheduler, "hot-retry"), errors.New("handler failed"))
	assertScheduledDelay(t, retriedJob(t, scheduler, "hot-retry"), second, "after the swap")
}

// TestSetRetryPolicy_ConcurrentWithFailureHandling 覆盖"写策略 / 读策略"交错：
// 重载协程写、worker 侧读同一字段时必须是原子的（普通接口字段在 -race 下直接判红）。
// handleFailure 只在主协程里跑，堆与存储的写入仍由同一条协程串行，race 检测只针对这个字段。
func TestSetRetryPolicy_ConcurrentWithFailureHandling(t *testing.T) {
	scheduler := NewScheduler(newMockStore(), &ExponentialBackoffRetry{MaxDelay: time.Second}, nil, quietLogger())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 200; i++ {
			scheduler.SetRetryPolicy(&ExponentialBackoffRetry{MaxDelay: time.Duration(i) * time.Millisecond})
		}
	}()

	for i := 0; i < 200; i++ {
		scheduler.handleFailure(&Job{
			ID:         "concurrent-retry",
			Name:       "flaky",
			Type:       "flaky",
			TriggerAt:  time.Now(),
			MaxRetries: 1,
			RetryDelay: time.Millisecond,
		}, errors.New("handler failed"))
		_ = scheduler.RetryPolicyMaxDelay()
	}
	wg.Wait()

	// 判据不能只有"-race 没报错"：写完 200 次之后再走一遍"设策略 → 失败重试 → 看排期"，
	// 证明读取点在并发写之后仍然拿得到最近一次写入的策略。
	tail := &recordingRetryPolicy{delay: 250 * time.Millisecond}
	scheduler.SetRetryPolicy(tail)
	scheduler.handleFailure(&Job{
		ID:         "concurrent-tail",
		Name:       "flaky",
		Type:       "flaky",
		TriggerAt:  time.Now(),
		MaxRetries: 1,
		RetryDelay: time.Millisecond,
	}, errors.New("handler failed"))
	assertScheduledDelay(t, retriedJob(t, scheduler, "concurrent-tail"), tail, "after the concurrent writes")
}

// retriedJob 取回堆里的重试副本。
func retriedJob(t *testing.T, scheduler *Scheduler, id string) *Job {
	t.Helper()

	item := scheduler.heap.Get(id)
	if item == nil {
		t.Fatalf("no retried job scheduled under %s", id)
	}
	return item.(*Job)
}

// quietLogger 把 handleFailure 排期时那条 warn 丢掉：并发用例要跑 200 轮，
// 默认日志器往 stderr 写 200 条会把 -v 的结果冲掉。
// 这里消音与排期断言的精度无关——断言测的是"策略被问的那一刻"与副本 TriggerAt 之差，
// 两步都在 warn 之前完成，写不写日志都改变不了那个差值。
func quietLogger() Option {
	return WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// assertScheduledDelay 断言堆里的重试副本就是由这条策略算出来的时刻：
// 副本的 TriggerAt 减去"策略被问到的那一瞬间"等于该策略声明的延迟（±1ms）。
//
// 基准取策略自己取时的瞬间而不是用例开头：`go test ./...` 是各包并行跑的，
// 一次 CPU 抢占就能把 ±1ms 的窗口整个吃掉，判红的是调度延迟而不是被测语义。
// calledAt 为空同时守住另一件事——排期确实用到了这一条策略，不是留着旧的那条在算。
func assertScheduledDelay(t *testing.T, retried *Job, policy *recordingRetryPolicy, stage string) {
	t.Helper()

	policy.mu.Lock()
	defer policy.mu.Unlock()
	if len(policy.calledAt) == 0 {
		t.Fatalf("%s: the retry policy was never consulted", stage)
	}
	got := retried.TriggerAt.Sub(policy.calledAt[len(policy.calledAt)-1])
	if got < policy.delay-time.Millisecond || got > policy.delay+time.Millisecond {
		t.Fatalf("%s: scheduled delay = %v, want %v ±1ms", stage, got, policy.delay)
	}
}

// recordingRetryPolicy 与既有的 mockRetryPolicy 同一形态，只是多记下"自己被问的那一瞬间"，
// 让上面那条断言建立在同一次取时之上。
type recordingRetryPolicy struct {
	delay    time.Duration
	mu       sync.Mutex
	calledAt []time.Time
}

func (p *recordingRetryPolicy) NextRetry(job *Job) time.Time {
	now := time.Now()
	p.mu.Lock()
	p.calledAt = append(p.calledAt, now)
	p.mu.Unlock()
	return now.Add(p.delay)
}
