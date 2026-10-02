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
// 排期取值留 ±1ms 容差，理由与基准取法见 assertScheduledDelay。
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
	assertScheduledDelay(t, retriedJob(t, scheduler, "hot-retry"), first, 1, "before the swap")

	second := &recordingRetryPolicy{delay: 30 * time.Millisecond}
	scheduler.SetRetryPolicy(second)
	scheduler.handleFailure(retriedJob(t, scheduler, "hot-retry"), errors.New("handler failed"))
	assertScheduledDelay(t, retriedJob(t, scheduler, "hot-retry"), second, 1, "after the swap")
}

// TestSetRetryPolicy_ConcurrentWithFailureHandling 覆盖"写策略 / 读策略"交错：
// 一条协程写、主协程每轮失败都读，字段若是普通接口字段会被 -race 直接抓住。
// 除了 -race 干净，这里还断"每次读数都是写进去过的那一类值"（见循环后的说明）。
// handleFailure 只在主协程里跑，堆与存储的写入仍由同一条协程串行。
func TestSetRetryPolicy_ConcurrentWithFailureHandling(t *testing.T) {
	scheduler := NewScheduler(newMockStore(), &ExponentialBackoffRetry{MaxDelay: time.Second}, nil, quietLogger())

	var wg sync.WaitGroup
	seen := make([]time.Duration, 0, 200)
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
		seen = append(seen, scheduler.RetryPolicyMaxDelay())
	}
	wg.Wait()

	// 每次读数都必须是"构造时那一条（1s）"或"某一轮写进去的那一条（1..200ms）"：
	// 读到 0、读到负数、读到 250ms 这种从没写过的值，说明读写没走原子语义。
	// 这条断言管的是"读到的值完好"，跨协程可见性本身的判据是 -race（读数落在哪一代上
	// 取决于两个协程谁被调度到，所以不能断"一定看到过 ≥2 个不同值"）。
	for i, got := range seen {
		if got == time.Second {
			continue
		}
		if got < time.Millisecond || got > 200*time.Millisecond {
			t.Fatalf("observation %d = %v, want either the initial 1s or one of the written 1..200ms", i, got)
		}
	}

	// 并发写完之后字段仍可用：最后一次写入读得出来。
	tail := &ExponentialBackoffRetry{MaxDelay: 250 * time.Millisecond}
	scheduler.SetRetryPolicy(tail)
	if got := scheduler.RetryPolicyMaxDelay(); got != 250*time.Millisecond {
		t.Fatalf("max delay after the concurrent writes = %v, want the last write 250ms", got)
	}
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
// 副本的 TriggerAt 减去"策略被问到的那一瞬间"等于该策略声明的延迟。
//
// wantCalls 断的是"这一次失败把策略问了几次"：handleFailure 只该 Load 一次、算一个时刻，
// 分两次 Load 会让发出去的事件与真正排下来的时刻来自两代策略（读数一致时看不出，
// 所以判据只能记在调用次数上）。
//
// 基准取策略自己取时的瞬间而不是用例开头：`go test ./...` 是各包并行跑的，
// 一个开用例时取的 time.Now() 与真正调用策略之间可能被任意抢占。
// 容差 ±1ms 是给 time.Now() 与堆写入之间的残余抖动留的，不是既有用例的口径
// （仓内其它重试用例断的是抖动区间，见 core/retry_test.go:25-30）。
func assertScheduledDelay(t *testing.T, retried *Job, policy *recordingRetryPolicy, wantCalls int, stage string) {
	t.Helper()

	policy.mu.Lock()
	defer policy.mu.Unlock()
	if len(policy.calledAt) != wantCalls {
		t.Fatalf("%s: retry policy consulted %d times, want %d", stage, len(policy.calledAt), wantCalls)
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
