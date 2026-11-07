package core

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// permanentStubError 是 core 侧测试用的错误：只多带一个 Permanent() 方法。
// 测试不引用 executor——core 认识的只有接口本身（卡片 §3.1 的依赖方向）。
type permanentStubError struct {
	permanent bool
	message   string
}

func (e permanentStubError) Error() string {
	if e.message == "" {
		return "permanent stub failure"
	}
	return e.message
}

func (e permanentStubError) Permanent() bool { return e.permanent }

// waitSnapshotStatus 轮询存储里的任务快照，直到状态变成 want；超时返回最后读到的快照。
func waitSnapshotStatus(t *testing.T, store *mockStore, jobID string, want JobStatus, timeout time.Duration) JobSnapshot {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last JobSnapshot
	for time.Now().Before(deadline) {
		if snap, ok := store.snapshotOf(jobID); ok {
			last = snap
			if JobStatus(snap.Status) == want {
				return snap
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("任务 %s 没在 %v 内变成 %s，最后状态 %s", jobID, timeout, want, JobStatus(last.Status))
	return last
}

// drainEvents 在窗口内收干事件通道的内容。
// 用来数"有且只有一条失败事件""一条重试事件都没有"这种整条收尾链路的结论。
func drainEvents(ch <-chan Event, window time.Duration) []Event {
	var got []Event
	deadline := time.After(window)
	for {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
}

func countEvents(events []Event, typ EventType) []Event {
	var matched []Event
	for _, ev := range events {
		if ev.Type == typ {
			matched = append(matched, ev)
		}
	}
	return matched
}

// runOnceAndFail 起一个只跑一次的调度器：处理函数返回 given 这个错误。
// 返回调度器、事件通道、执行次数与存储。
func runOnceAndFail(t *testing.T, jobID string, given func() error) (*Scheduler, *mockStore, <-chan Event, *int32) {
	t.Helper()

	store := newMockStore()
	eventBus := NewEventBus(64)
	scheduler := NewScheduler(store, &mockRetryPolicy{delay: 30 * time.Second}, eventBus)
	scheduler.SetConcurrency(1)

	_, events := eventBus.SubscribeAll()

	var runs int32
	scheduler.RegisterHandler("failing", func(ctx context.Context, job *Job) error {
		atomic.AddInt32(&runs, 1)
		return given()
	})

	if err := scheduler.Schedule(&Job{
		ID:         jobID,
		Name:       "failing",
		Type:       "failing",
		TriggerAt:  time.Now(),
		MaxRetries: 3,
		RetryDelay: time.Millisecond,
	}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	scheduler.Start()
	t.Cleanup(scheduler.Stop)

	return scheduler, store, events, &runs
}

// TestHandleFailure_PermanentSkipsRetry 是本卡的主用例（§5.1）：
// 处理函数返回永久失败时，任务直接落终态，一次都不多跑。
func TestHandleFailure_PermanentSkipsRetry(t *testing.T) {
	scheduler, store, events, runs := runOnceAndFail(t, "perm-skip", func() error {
		return permanentStubError{permanent: true, message: "invalid submission"}
	})

	snap := waitSnapshotStatus(t, store, "perm-skip", StatusFailed, 3*time.Second)

	if got := atomic.LoadInt32(runs); got != 1 {
		t.Errorf("永久失败不该再跑第二次，实际执行 %d 次", got)
	}
	if snap.RetryCount != 0 {
		t.Errorf("永久失败不该消耗重试次数，实际 RetryCount=%d", snap.RetryCount)
	}
	if scheduler.heap.Len() != 0 {
		t.Errorf("永久失败不该在堆里留下重试副本，堆长度 %d", scheduler.heap.Len())
	}

	collected := drainEvents(events, 300*time.Millisecond)
	failed := countEvents(collected, EventJobFailed)
	if len(failed) != 1 {
		t.Fatalf("应当有且只有一条 job.failed 事件，实际 %d 条", len(failed))
	}
	if permanent, _ := failed[0].Metadata["permanent"].(bool); !permanent {
		t.Errorf("失败事件要带 permanent=true，实际 metadata %v", failed[0].Metadata)
	}
	if retrying := countEvents(collected, EventJobRetrying); len(retrying) != 0 {
		t.Errorf("永久失败不该产生 job.retrying 事件，实际 %d 条", len(retrying))
	}
}

// TestHandleFailure_NonPermanentStillRetries 守住另一头：标记为假时既有行为一字不变。
func TestHandleFailure_NonPermanentStillRetries(t *testing.T) {
	scheduler, _, events, runs := runOnceAndFail(t, "retry-keep", func() error {
		return permanentStubError{permanent: false, message: "downstream was busy"}
	})

	// 重试副本沿用原 ID，退避时间由 mockRetryPolicy 决定（30 秒，所以副本会留在堆里可观察）
	retried := waitForHeapJob(scheduler, "retry-keep", func(j *Job) bool {
		return j.RetryCount == 1
	})
	if retried == nil {
		t.Fatal("非永久失败应当排期重试副本，堆里没有")
	}
	if got := atomic.LoadInt32(runs); got != 1 {
		t.Errorf("重试副本要 30 秒后才跑，这里不该有第二次执行，实际 %d 次", got)
	}

	collected := drainEvents(events, 300*time.Millisecond)
	if retrying := countEvents(collected, EventJobRetrying); len(retrying) != 1 {
		t.Errorf("应当有一条 job.retrying 事件，实际 %d 条", len(retrying))
	} else if _, ok := retrying[0].Metadata["next_retry_at"]; !ok {
		t.Errorf("重试事件仍要带 next_retry_at（退避由 RetryPolicy 决定），实际 %v", retrying[0].Metadata)
	}
	failed := countEvents(collected, EventJobFailed)
	if len(failed) != 1 {
		t.Fatalf("应当有一条 job.failed 事件，实际 %d 条", len(failed))
	}
	if _, ok := failed[0].Metadata["permanent"]; ok {
		t.Errorf("非永久失败不该出现 permanent 键，实际 %v", failed[0].Metadata)
	}
}

// TestHandleFailure_WrappedPermanent 守住"用 errors.As 而不是类型断言"：
// 处理函数在错误外面包一层描述之后仍然要认得它。
func TestHandleFailure_WrappedPermanent(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, &mockRetryPolicy{delay: 30 * time.Second}, nil)

	job := &Job{ID: "wrapped", Name: "wrapped", MaxRetries: 3, RetryCount: 1}
	wrapped := fmt.Errorf("executor run failed: %w", permanentStubError{permanent: true})

	scheduler.handleFailure(job, wrapped)

	if scheduler.heap.Len() != 0 {
		t.Errorf("包一层之后仍要认出永久失败，不该排期重试，堆长度 %d", scheduler.heap.Len())
	}
	if job.Status != StatusFailed {
		t.Errorf("包装后的永久失败应直接落 failed，实际 %s", job.Status)
	}
}

// TestHandleFailure_ForcedPauseTakesPrecedence 确认判定顺序没写反（§3.3 第 1 步在第 2 步之前）：
// 有强制暂停标记且错误是永久失败时，任务仍然停在 paused，既不落 failed 也不排期重试。
//
// 这里直接调 handleFailure 并把标记摆好，而不走"执行中 ForcePause"的真实链路：
// 真链路里取消上下文一定先被 executeJob 认出来交给 handleInterrupted（见 TestScheduler_ForcePause_*），
// handleFailure 的这一分支只在"标记已就位、错误又确实传到失败收尾"时才走得到。
func TestHandleFailure_ForcedPauseTakesPrecedence(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, &mockRetryPolicy{delay: 30 * time.Second}, nil)
	jobID := "fp-order"
	job := &Job{ID: jobID, Name: "failing", Type: "failing", MaxRetries: 3}

	scheduler.mu.Lock()
	scheduler.forcedPause[jobID] = struct{}{}
	scheduler.mu.Unlock()

	scheduler.handleFailure(job, permanentStubError{permanent: true, message: "failed anyway"})

	if job.Status != StatusPaused {
		t.Errorf("强制暂停要优先于永久失败，停在 paused，实际 %s", job.Status)
	}
	if job.RetryCount != 0 {
		t.Errorf("不该消耗重试次数，实际 %d", job.RetryCount)
	}
	if scheduler.heap.Len() != 0 {
		t.Errorf("不该产生重试副本，堆长度 %d", scheduler.heap.Len())
	}
	snap, ok := store.snapshotOf(jobID)
	if !ok || JobStatus(snap.Status) != StatusPaused {
		t.Errorf("落盘的快照也要停在 paused，实际 %+v", snap)
	}
	// 标记被认领掉了：同一次暂停请求不该在下一轮执行里再次生效
	scheduler.mu.Lock()
	_, stillPending := scheduler.forcedPause[jobID]
	scheduler.mu.Unlock()
	if stillPending {
		t.Error("handleFailure 应当消费掉强制暂停标记")
	}
}

// TestHandleFailure_NilErrOrPlainErr 是回归保护：没实现 Permanent() 的错误行为与改动前一致。
func TestHandleFailure_NilErrOrPlainErr(t *testing.T) {
	cases := []struct {
		label string
		given error
	}{
		{"nil 错误", nil},
		{"普通错误", errors.New("handler said no")},
		{"包过一层的普通错误", fmt.Errorf("wrap: %w", errors.New("handler said no"))},
		{"显式标成非永久", permanentStubError{permanent: false}},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			scheduler := NewScheduler(newMockStore(), &mockRetryPolicy{delay: 30 * time.Second}, nil)
			job := &Job{ID: "plain-" + tc.label, Name: "plain", MaxRetries: 2, RetryCount: 0}

			scheduler.handleFailure(job, tc.given)

			if job.Status == StatusFailed {
				t.Errorf("还有重试名额时不该直接落 failed，实际 %s", job.Status)
			}
			if scheduler.heap.Len() != 1 {
				t.Fatalf("应当排期一个重试副本，堆长度 %d", scheduler.heap.Len())
			}
			retried, ok := scheduler.heap.Get(job.ID).(*Job)
			if !ok || retried.RetryCount != 1 {
				t.Errorf("重试副本的 RetryCount 要是 1，实际 %+v", scheduler.heap.Get(job.ID))
			}
		})
	}

	// 重试名额用完之后仍然是老结论：永久判定没有改写"耗尽重试"这条路径
	exhausted := NewScheduler(newMockStore(), nil, nil)
	job := &Job{ID: "exhausted", Name: "plain", MaxRetries: 1, RetryCount: 1}
	exhausted.handleFailure(job, errors.New("handler said no"))
	if job.Status != StatusFailed {
		t.Errorf("重试耗尽要落 failed，实际 %s", job.Status)
	}
}
