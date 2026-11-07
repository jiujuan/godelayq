package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduler_Suspend_HoldsDueJobsBack(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(1)

	var runs int32
	scheduler.RegisterHandler("tick", func(ctx context.Context, job *Job) error {
		atomic.AddInt32(&runs, 1)
		return nil
	})

	// 300ms 后到点：挂起状态下不应该被弹出
	if err := scheduler.Schedule(&Job{ID: "susp-1", Name: "tick", Type: "tick", TriggerAt: time.Now().Add(300 * time.Millisecond)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	scheduler.Start()
	defer scheduler.Stop()

	scheduler.Suspend()
	time.Sleep(800 * time.Millisecond)

	if got := atomic.LoadInt32(&runs); got != 0 {
		t.Errorf("挂起期间不应执行任务，实际执行 %d 次", got)
	}
	if scheduler.heap.Len() != 1 {
		t.Errorf("挂起不应改动堆，实际堆长度 %d", scheduler.heap.Len())
	}

	scheduler.Unsuspend()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&runs) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&runs) != 1 {
		t.Error("恢复后到期任务应补跑")
	}
}

// 挂起只停"取新任务"，已经交给 worker 的任务必须跑完——
// 这正是它与 Stop 的区别，也是维护窗口敢按下的前提。
func TestScheduler_Suspend_DoesNotAbortRunningJob(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(1)

	started := make(chan struct{})
	var finished int32
	scheduler.RegisterHandler("slow", func(ctx context.Context, job *Job) error {
		close(started)
		time.Sleep(400 * time.Millisecond)
		atomic.StoreInt32(&finished, 1)
		return nil
	})

	if err := scheduler.Schedule(&Job{ID: "susp-run", Name: "slow", Type: "slow", TriggerAt: time.Now()}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	scheduler.Start()
	defer scheduler.Stop()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("任务没有开始执行")
	}

	scheduler.Suspend()
	time.Sleep(700 * time.Millisecond)

	if atomic.LoadInt32(&finished) != 1 {
		t.Error("挂起不应中断已在执行的任务")
	}
}

func TestScheduler_Suspend_StillAcceptsNewJobs(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(1)

	var runs int32
	var mu sync.Mutex
	scheduler.RegisterHandler("echo", func(ctx context.Context, job *Job) error {
		mu.Lock()
		atomic.AddInt32(&runs, 1)
		mu.Unlock()
		return nil
	})

	scheduler.Start()
	defer scheduler.Stop()

	// 挂起在启动之后：Start 会清除挂起状态（见 TestScheduler_StartClearsSuspend），
	// 先挂起再启动等于没挂起
	scheduler.Suspend()

	// 挂起期间提交任务：入队与落盘照常，只是不触发
	if err := scheduler.Schedule(&Job{ID: "susp-add", Name: "echo", Type: "echo", TriggerAt: time.Now().Add(20 * time.Millisecond)}); err != nil {
		t.Fatalf("挂起期间 Schedule 应仍然可用: %v", err)
	}
	if scheduler.heap.Len() != 1 {
		t.Errorf("任务应已入堆，实际堆长度 %d", scheduler.heap.Len())
	}
	if _, ok := store.snapshotOf("susp-add"); !ok {
		t.Error("挂起期间提交的任务也应落盘")
	}

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if runs != 0 {
		mu.Unlock()
		t.Fatalf("挂起期间不应执行，实际 %d 次", runs)
	}
	mu.Unlock()

	scheduler.Unsuspend()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&runs) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&runs) != 1 {
		t.Error("恢复后应执行挂起期间提交的任务")
	}
}

// 总开关是进程内意图：Start（含重启与 Start→Stop→Start）必须把它清零，
// 否则一个遗留的挂起状态会让调度器"看起来在跑却永不出任务"。
func TestScheduler_StartClearsSuspend(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(1)

	var runs int32
	scheduler.RegisterHandler("tick", func(ctx context.Context, job *Job) error {
		atomic.AddInt32(&runs, 1)
		return nil
	})

	scheduler.Suspend()
	if err := scheduler.Schedule(&Job{ID: "susp-start", Name: "tick", Type: "tick", TriggerAt: time.Now().Add(50 * time.Millisecond)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	scheduler.Start()
	defer scheduler.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&runs) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&runs) != 1 {
		t.Error("Start 应清除挂起状态，任务应照常执行")
	}
}

// 挂起中关停必须能及时返回：调度循环停在等待分支上，
// 如果没把 stopCh 一起 select 进去，进程会卡在关停里。
func TestScheduler_StopWhileSuspendedReturnsPromptly(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.RegisterHandler("tick", func(ctx context.Context, job *Job) error { return nil })
	if err := scheduler.Schedule(&Job{ID: "susp-stop", Name: "tick", Type: "tick", TriggerAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	scheduler.Start()
	scheduler.Suspend()
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		scheduler.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("挂起状态下 Stop 没能及时返回")
	}
}
