package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// waitForHeapJob 轮询堆中指定 ID 的任务，直到满足条件或超时；未命中返回 nil。
func waitForHeapJob(s *Scheduler, id string, pred func(*Job) bool) *Job {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if item := s.heap.Get(id); item != nil {
			if j, ok := item.(*Job); ok && pred(j) {
				return j
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// TestScheduler_JobTimeout 超时的 Handler 收到 DeadlineExceeded，
// 任务按失败处理并落盘，事件带 timeout 标记。
func TestScheduler_JobTimeout(t *testing.T) {
	store := newMockStore()
	eventBus := NewEventBus(10)
	scheduler := NewScheduler(store, nil, eventBus)
	scheduler.SetConcurrency(2)

	_, eventCh := eventBus.Subscribe(EventJobFailed)

	scheduler.RegisterHandler("hang", func(ctx context.Context, job *Job) error {
		<-ctx.Done()
		return ctx.Err()
	})

	scheduler.Schedule(&Job{
		ID:        "timeout-job",
		Name:      "hang",
		TriggerAt: time.Now(),
		Timeout:   100 * time.Millisecond,
	})
	scheduler.Start()

	select {
	case ev := <-eventCh:
		if ev.Type != EventJobFailed {
			t.Fatalf("Expected EventJobFailed, got %v", ev.Type)
		}
		if timeout, _ := ev.Metadata["timeout"].(bool); !timeout {
			t.Errorf("Expected timeout=true in metadata, got %v", ev.Metadata)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Expected timeout failure event")
	}

	scheduler.Stop()

	snap, ok := store.jobs["timeout-job"]
	if !ok {
		t.Fatal("Expected timed-out job persisted for inspection")
	}
	if JobStatus(snap.Status) != StatusFailed {
		t.Errorf("Expected status Failed, got %d", snap.Status)
	}
	if snap.Attempts != 1 {
		t.Errorf("Expected 1 attempt, got %d", snap.Attempts)
	}
}

// TestScheduler_TimeoutCountsAsRetryableFailure 超时是可重试的失败：
// 重试副本沿用原 ID 并保留超时配置。
func TestScheduler_TimeoutCountsAsRetryableFailure(t *testing.T) {
	// 足够长的重试延迟，让重试副本留在堆里可被观察
	retryPolicy := &mockRetryPolicy{delay: 30 * time.Second}
	scheduler := NewScheduler(nil, retryPolicy, nil)
	scheduler.SetConcurrency(2)

	scheduler.RegisterHandler("hang", func(ctx context.Context, job *Job) error {
		<-ctx.Done()
		return ctx.Err()
	})

	scheduler.Schedule(&Job{
		ID:         "retry-on-timeout",
		Name:       "hang",
		TriggerAt:  time.Now(),
		Timeout:    100 * time.Millisecond,
		MaxRetries: 1,
		RetryDelay: 5 * time.Millisecond,
	})
	scheduler.Start()

	retried := waitForHeapJob(scheduler, "retry-on-timeout", func(j *Job) bool {
		return j.RetryCount == 1
	})
	if retried == nil {
		t.Fatal("Expected a retry scheduled under the original ID after timeout")
	}
	if retried.Timeout != 100*time.Millisecond {
		t.Errorf("Expected retry to preserve timeout, got %v", retried.Timeout)
	}

	scheduler.Stop()
}

// TestScheduler_ShutdownInterruptKeepsRecoveryState 关停打断的执行不记失败、
// 不消耗重试次数，任务保持 Pending 等待下次 Restore。
func TestScheduler_ShutdownInterruptKeepsRecoveryState(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(2)

	scheduler.RegisterHandler("hang", func(ctx context.Context, job *Job) error {
		<-ctx.Done()
		return ctx.Err()
	})

	scheduler.Schedule(&Job{
		ID:         "interrupted",
		Name:       "hang",
		TriggerAt:  time.Now(),
		MaxRetries: 3,
	})
	scheduler.Start()

	// 等任务真正进入执行（Attempts 递增）后再关停
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		scheduler.mu.RLock()
		_, running := scheduler.cancelMap["interrupted"]
		scheduler.mu.RUnlock()
		if running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	scheduler.Stop()

	snap, ok := store.jobs["interrupted"]
	if !ok {
		t.Fatal("Expected interrupted job to stay persisted")
	}
	if JobStatus(snap.Status) != StatusPending {
		t.Errorf("Expected interrupted job kept as Pending, got status %d", snap.Status)
	}
	if snap.RetryCount != 0 {
		t.Errorf("Interruption must not consume retry budget, RetryCount=%d", snap.RetryCount)
	}
	if snap.Attempts != 1 {
		t.Errorf("Expected the attempt to be counted, got %d", snap.Attempts)
	}
}

// TestScheduler_CancelRunningJobNotResurrected 取消正在执行的任务后，
// 存储不得留下 Pending 快照，否则下次启动会把它救活。
func TestScheduler_CancelRunningJobNotResurrected(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(2)

	returned := make(chan error, 1)
	scheduler.RegisterHandler("hang", func(ctx context.Context, job *Job) error {
		<-ctx.Done()
		returned <- ctx.Err()
		return ctx.Err()
	})

	scheduler.Schedule(&Job{ID: "cancel-me", Name: "hang", TriggerAt: time.Now()})
	scheduler.Start()
	defer scheduler.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		scheduler.mu.RLock()
		_, running := scheduler.cancelMap["cancel-me"]
		scheduler.mu.RUnlock()
		if running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := scheduler.Cancel("cancel-me"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Expected context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Expected handler to return after cancel")
	}

	// 给执行协程走完收尾分类
	time.Sleep(100 * time.Millisecond)

	if _, ok := store.jobs["cancel-me"]; ok {
		t.Error("Cancelled job must not remain persisted")
	}
	if scheduler.heap.Get("cancel-me") != nil {
		t.Error("Cancelled job must not be re-queued")
	}
}

// TestJob_TimeoutSnapshotRoundTrip 超时需随快照持久化，重启后仍生效。
func TestJob_TimeoutSnapshotRoundTrip(t *testing.T) {
	job := &Job{
		ID:        "with-timeout",
		Name:      "task",
		Timeout:   90 * time.Second,
		TriggerAt: time.Now().Add(time.Minute),
	}

	snap := job.ToSnapshot()
	if snap.Timeout != int64(90*time.Second) {
		t.Errorf("Expected snapshot timeout %d, got %d", int64(90*time.Second), snap.Timeout)
	}

	restored := &Job{}
	restored.FromSnapshot(snap)
	if restored.Timeout != 90*time.Second {
		t.Errorf("Expected restored timeout 90s, got %v", restored.Timeout)
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var decoded JobSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Timeout != int64(90*time.Second) {
		t.Error("Expected timeout to survive JSON round trip")
	}
}

// TestDirectoryLoader_TimeoutField 任务文件的 timeout 字段应被解析。
func TestDirectoryLoader_TimeoutField(t *testing.T) {
	loader, err := NewDirectoryLoader(NewScheduler(nil, nil, nil), LoaderOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	f := &FileJobFormat{Name: "file-job", Delay: "1m", Timeout: "45s"}
	job, err := loader.formatToJob(f)
	if err != nil {
		t.Fatal(err)
	}
	if job.Timeout != 45*time.Second {
		t.Errorf("Expected timeout 45s, got %v", job.Timeout)
	}

	bad := &FileJobFormat{Name: "file-job", Timeout: "soon"}
	if _, err := loader.formatToJob(bad); err == nil {
		t.Error("Expected error for invalid timeout format")
	}
}
