package core

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestScheduler_SetQueueCapacity 队列容量可独立于 worker 数配置，
// 非负约束与 Start 后忽略两条边界都要成立。
func TestScheduler_SetQueueCapacity(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	scheduler.SetQueueCapacity(25)
	if scheduler.queueCapacity != 25 {
		t.Errorf("Expected queue capacity 25, got %d", scheduler.queueCapacity)
	}

	scheduler.SetQueueCapacity(-3)
	if scheduler.queueCapacity != 0 {
		t.Errorf("Negative capacity should clamp to 0 (== workers), got %d", scheduler.queueCapacity)
	}

	scheduler.SetQueueCapacity(40)
	scheduler.Start()
	defer scheduler.Stop()

	if cap(scheduler.workCh) != 40 {
		t.Errorf("Expected queue capacity 40, got %d", cap(scheduler.workCh))
	}

	scheduler.SetQueueCapacity(7)
	if scheduler.queueCapacity != 40 {
		t.Error("SetQueueCapacity must be ignored after Start")
	}
}

// TestScheduler_Restart Work pool 复位后 Start→Stop→Start 应可继续使用。
func TestScheduler_Restart(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(2)

	var mu sync.Mutex
	ran := make([]string, 0, 4)
	scheduler.RegisterHandler("cycle", func(ctx context.Context, job *Job) error {
		mu.Lock()
		ran = append(ran, job.ID)
		mu.Unlock()
		return nil
	})

	runBatch := func(batch string, count int) {
		t.Helper()
		for i := 0; i < count; i++ {
			if err := scheduler.Schedule(&Job{
				ID:        batch + "-" + strconv.Itoa(i),
				Name:      "cycle",
				TriggerAt: time.Now().Add(10 * time.Millisecond),
			}); err != nil {
				t.Fatal(err)
			}
		}

		scheduler.Start()

		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			done := len(ran) >= count
			mu.Unlock()
			if done {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		scheduler.Stop()

		mu.Lock()
		defer mu.Unlock()
		if len(ran) != count {
			t.Fatalf("Expected %d jobs from batch %s to run, got %d", count, batch, len(ran))
		}
		ran = ran[:0]
	}

	runBatch("first", 3)
	runBatch("second", 3)
}

// TestScheduler_RestoreAfterRestart 重启后仍会从存储恢复未完成任务。
func TestScheduler_RestoreAfterRestart(t *testing.T) {
	store := newMockStore()
	store.jobs = map[string]JobSnapshot{
		"leftover": {ID: "leftover", Name: "recover", TriggerAt: time.Now().Add(-time.Minute), Status: int(StatusPending)},
	}

	var mu sync.Mutex
	runs := 0
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.RegisterHandler("recover", func(ctx context.Context, job *Job) error {
		mu.Lock()
		runs++
		mu.Unlock()
		// 模拟被打断：任务不会被 handleSuccess 清理，下次启动应重新恢复
		return context.Canceled
	})

	// 第一轮：Restore 入堆并执行一次
	scheduler.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := runs == 1
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	scheduler.Stop()

	mu.Lock()
	if runs != 1 {
		mu.Unlock()
		t.Fatalf("Expected exactly one run in the first cycle, got %d", runs)
	}
	mu.Unlock()

	// 第二轮：重新 Start 应再次恢复同一个任务
	scheduler.Start()
	defer scheduler.Stop()

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := runs == 2
		mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected the leftover job to be restored and executed again after restart")
}

// TestScheduler_DefaultConcurrency 未显式设置时使用 DefaultConcurrency，
// 与 docs/deployment.md 的 workers 口径一致。
func TestScheduler_DefaultConcurrency(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	if scheduler.concurrency != DefaultConcurrency {
		t.Errorf("Expected default concurrency %d, got %d", DefaultConcurrency, scheduler.concurrency)
	}
}

// TestScheduler_SetConcurrency 覆盖非法值回退与 Start 之后不生效两种边界。
func TestScheduler_SetConcurrency(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	scheduler.SetConcurrency(4)
	if scheduler.concurrency != 4 {
		t.Errorf("Expected concurrency 4, got %d", scheduler.concurrency)
	}

	scheduler.SetConcurrency(0)
	if scheduler.concurrency != DefaultConcurrency {
		t.Errorf("Non-positive value should fall back to default, got %d", scheduler.concurrency)
	}

	scheduler.SetConcurrency(2)
	scheduler.Start()
	defer scheduler.Stop()

	scheduler.SetConcurrency(99)
	if scheduler.concurrency != 2 {
		t.Error("SetConcurrency must be ignored after Start")
	}
}

// TestScheduler_LimitsConcurrency 到期风暴下同时在执行的任务数不得超过 worker 数，
// 且队列满时被阻塞的任务不会丢失，最终全部执行完。
func TestScheduler_LimitsConcurrency(t *testing.T) {
	const (
		workers = 3
		total   = 20
	)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(workers)

	var (
		mu       sync.Mutex
		active   int
		peak     int
		finished int
	)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	scheduler.RegisterHandler("burst", func(ctx context.Context, job *Job) error {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()

		<-release

		mu.Lock()
		active--
		finished++
		mu.Unlock()
		return nil
	})

	now := time.Now()
	for i := 0; i < total; i++ {
		if err := scheduler.Schedule(&Job{Name: "burst", TriggerAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	scheduler.Start()

	// worker 全部占满后处理器都卡在 release 上，此时峰值并发不应超过 workers
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	peakDuringBlock := peak
	finishedDuringBlock := finished
	mu.Unlock()

	if peakDuringBlock > workers {
		t.Errorf("Concurrency exceeded worker count: peak=%d workers=%d", peakDuringBlock, workers)
	}
	if peakDuringBlock == 0 {
		t.Fatal("Expected jobs to start executing")
	}
	if finishedDuringBlock != 0 {
		t.Errorf("Expected no job to finish while blocked, got %d", finishedDuringBlock)
	}
	if scheduler.heap.Len() == 0 {
		t.Error("Expected remaining jobs to stay queued in the heap under backpressure")
	}

	unblock()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := finished == total
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	scheduler.Stop()

	mu.Lock()
	defer mu.Unlock()
	if finished != total {
		t.Errorf("Expected all %d jobs to run, got %d", total, finished)
	}
	if peak > workers {
		t.Errorf("Concurrency peaked above worker count: %d > %d", peak, workers)
	}
	if scheduler.HeapLen() != 0 {
		t.Errorf("Expected heap drained after the burst, got %d", scheduler.HeapLen())
	}
}

// TestScheduler_StopIsPromptUnderBackpressure 关停时：调度循环停在阻塞投递上、
// 在途任务收到上下文取消，Stop 不得被拖住；未执行的任务仍留在存储里等待下次恢复。
func TestScheduler_StopIsPromptUnderBackpressure(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(2)

	started := make(chan string, 4)
	returned := make(chan error, 4)
	scheduler.RegisterHandler("slow", func(ctx context.Context, job *Job) error {
		started <- job.ID
		<-ctx.Done()
		returned <- ctx.Err()
		return ctx.Err()
	})

	now := time.Now()
	for i := 0; i < 8; i++ {
		if err := scheduler.Schedule(&Job{
			ID:        "slow-" + strconv.Itoa(i),
			Name:      "slow",
			TriggerAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	scheduler.Start()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("Expected 2 jobs to be executing")
		}
	}

	stopDone := make(chan struct{})
	go func() {
		scheduler.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked: in-flight jobs must receive context cancellation")
	}

	for i := 0; i < 2; i++ {
		select {
		case err := <-returned:
			if err != context.Canceled {
				t.Errorf("Expected context.Canceled, got %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Expected in-flight handlers to return after cancellation")
		}
	}

	// 未拿到执行机会的任务保持 Pending 落盘，下次 Start 由 Restore 重新入队
	remaining, err := store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) == 0 {
		t.Error("Expected not-yet-executed jobs to remain persisted for the next restore")
	}
}
