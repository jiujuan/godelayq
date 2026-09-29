package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestScheduler_BindsHandlerByType 验证 Handler 按 Type 查找、Name 回退，
// 且 Type 与 Name 不同时也能正确绑定（原实现误用 Name 作为键）。
func TestScheduler_BindsHandlerByType(t *testing.T) {
	done := make(chan string, 1)
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.RegisterHandler("payment_check", func(ctx context.Context, job *Job) error {
		done <- job.ID
		return nil
	})

	job := &Job{
		ID:        "typed-job",
		Name:      "订单支付检查", // 显示名，与注册键不同
		Type:      "payment_check",
		TriggerAt: time.Now(),
	}
	scheduler.executeJob(job)

	select {
	case id := <-done:
		if id != "typed-job" {
			t.Errorf("Expected job typed-job to run, got %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Error("Expected handler registered under Type to run")
	}
}

// TestScheduler_ExecuteJobWithoutHandler 无对应 Handler 时任务应失败并落盘，
// 而不是静默丢弃。
func TestScheduler_ExecuteJobWithoutHandler(t *testing.T) {
	store := newMockStore()
	eventBus := NewEventBus(10)
	scheduler := NewScheduler(store, nil, eventBus)

	_, eventCh := eventBus.Subscribe(EventJobFailed)

	job := &Job{ID: "orphan", Name: "no_handler_here", TriggerAt: time.Now()}
	scheduler.executeJob(job)

	if job.Status != StatusFailed {
		t.Errorf("Expected status Failed, got %v", job.Status)
	}
	if _, ok := store.jobs["orphan"]; !ok {
		t.Error("Expected failed snapshot persisted for inspection")
	}
	select {
	case ev := <-eventCh:
		if ev.Type != EventJobFailed {
			t.Errorf("Expected EventJobFailed, got %v", ev.Type)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Expected failure event published")
	}
}

// TestScheduler_Restore 验证崩溃恢复：存储中的任务被重建入堆，
// 过期任务在 Start 后补跑，并按 Type 绑定 Handler。
func TestScheduler_Restore(t *testing.T) {
	store := newMockStore()
	store.jobs = map[string]JobSnapshot{
		// 已过期，恢复后应立即补跑
		"overdue": {ID: "overdue", Name: "遗留任务", Type: "restored", TriggerAt: time.Now().Add(-time.Minute), Status: int(StatusPending)},
		// 未来任务，仅入堆不执行
		"future": {ID: "future", Name: "未来任务", Type: "restored", TriggerAt: time.Now().Add(time.Hour), Status: int(StatusPending)},
	}

	var mu sync.Mutex
	ran := make([]string, 0, 1)
	scheduler := NewScheduler(store, nil, nil)
	scheduler.RegisterHandler("restored", func(ctx context.Context, job *Job) error {
		mu.Lock()
		ran = append(ran, job.ID)
		mu.Unlock()
		return nil
	})

	if err := scheduler.Restore(); err != nil {
		t.Fatalf("Restore failed: %v", err)
	}
	if scheduler.heap.Len() != 2 {
		t.Fatalf("Expected 2 restored jobs in heap, got %d", scheduler.heap.Len())
	}
	if scheduler.heap.Get("future") == nil {
		t.Error("Expected future job restored into heap")
	}

	scheduler.Start()
	defer scheduler.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(ran) > 0
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || ran[0] != "overdue" {
		t.Errorf("Expected overdue job to be re-executed, got %v", ran)
	}
}

// TestScheduler_Restore_Idempotent 重复 Restore 不应产生重复任务。
func TestScheduler_Restore_Idempotent(t *testing.T) {
	store := newMockStore()
	store.jobs = map[string]JobSnapshot{
		"once": {ID: "once", Type: "noop", TriggerAt: time.Now().Add(time.Hour)},
	}
	scheduler := NewScheduler(store, nil, nil)

	if err := scheduler.Restore(); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Restore(); err != nil {
		t.Fatal(err)
	}
	if scheduler.heap.Len() != 1 {
		t.Errorf("Expected 1 job after double restore, got %d", scheduler.heap.Len())
	}
}

// TestScheduler_StartAutoRestores Start 应自动完成崩溃恢复接线。
func TestScheduler_StartAutoRestores(t *testing.T) {
	store := newMockStore()
	store.jobs = map[string]JobSnapshot{
		"auto": {ID: "auto", Type: "noop", TriggerAt: time.Now().Add(time.Hour), Status: int(StatusRunning)},
	}
	scheduler := NewScheduler(store, nil, nil)

	scheduler.Start()
	defer scheduler.Stop()

	if scheduler.heap.Get("auto") == nil {
		t.Error("Expected Start to restore persisted jobs")
	}
	// Running 快照恢复后状态应被重置为 Pending
	if got := scheduler.heap.Get("auto").(*Job); got.Status != StatusPending {
		t.Errorf("Expected restored status Pending, got %v", got.Status)
	}
}

// TestScheduler_RetryKeepsJobID 重试副本必须沿用原任务 ID，
// 且失败重试不会被误转成周期任务。
func TestScheduler_RetryKeepsJobID(t *testing.T) {
	store := newMockStore()
	retryPolicy := &mockRetryPolicy{delay: 10 * time.Millisecond}
	scheduler := NewScheduler(store, retryPolicy, nil)

	job := &Job{
		ID:         "chain-job",
		Name:       "flaky",
		Type:       "flaky",
		TriggerAt:  time.Now(),
		MaxRetries: 2,
		RetryDelay: 5 * time.Millisecond,
		CronExpr:   "*/5 * * * *",
		IsRepeat:   true,
	}

	scheduler.handleFailure(job, errors.New("handler failed"))

	retried := scheduler.heap.Get("chain-job")
	if retried == nil {
		t.Fatal("Expected retry to be scheduled under the original job ID")
	}
	rj := retried.(*Job)
	if rj.RetryCount != 1 {
		t.Errorf("Expected RetryCount 1, got %d", rj.RetryCount)
	}
	// 重试链不得继承 cron 语义，否则最后一次重试成功后会被永久排期
	if rj.IsRepeat || rj.CronExpr != "" {
		t.Error("Retry clone must not carry cron semantics")
	}

	// 存储也按同一 ID 覆盖，不产生孤儿记录
	if _, ok := store.jobs["chain-job"]; !ok {
		t.Error("Expected retry persisted under original ID")
	}
}

// TestScheduler_ConcurrentCancelAndExecute 并发调度/取消压测 cancelMap 访问，
// 需在 -race 下运行以验证竞态已修复。
func TestScheduler_ConcurrentCancelAndExecute(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.RegisterHandler("burst", func(ctx context.Context, job *Job) error {
		return nil
	})
	scheduler.Start()
	defer scheduler.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = scheduler.Schedule(&Job{
				Name:      "burst",
				TriggerAt: time.Now().Add(time.Duration(i%5) * time.Millisecond),
			})
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = scheduler.Cancel("missing-" + time.Now().Format("150405.000000000"))
		}(i)
	}
	wg.Wait()
}

// TestScheduler_CancelRunningJobCancelsContext 运行中任务被取消时，
// 其上下文必须收到取消信号（覆盖此前被跳过的取消路径）。
func TestScheduler_CancelRunningJobCancelsContext(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	started := make(chan struct{})
	returned := make(chan error, 1)
	scheduler.RegisterHandler("longrun", func(ctx context.Context, job *Job) error {
		close(started)
		<-ctx.Done()
		returned <- ctx.Err()
		return ctx.Err()
	})

	job := &Job{ID: "running-1", Name: "longrun", TriggerAt: time.Now()}
	// executeJob 由 worker 协程调用且会阻塞到 Handler 返回
	go scheduler.executeJob(job)

	<-started

	if err := scheduler.Cancel("running-1"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Expected running job context to be cancelled")
	}
}

// TestScheduler_RestoreSkipsTerminalRecords 存储现在同时留有终态记录，
// 恢复只能重建未完成任务，并把上次崩溃时的 Running 复位为 Pending。
func TestScheduler_RestoreSkipsTerminalRecords(t *testing.T) {
	store := newMockStore()
	trigger := time.Now().Add(time.Hour)

	seed := []JobSnapshot{
		{ID: "pending-one", Name: "task", TriggerAt: trigger, Status: int(StatusPending)},
		{ID: "running-one", Name: "task", TriggerAt: trigger, Status: int(StatusRunning)},
		{ID: "success-one", Name: "task", TriggerAt: trigger, Status: int(StatusSuccess)},
		{ID: "failed-one", Name: "task", TriggerAt: trigger, Status: int(StatusFailed)},
	}
	for _, snapshot := range seed {
		if err := store.Update(snapshot); err != nil {
			t.Fatalf("seed store failed: %v", err)
		}
	}

	scheduler := NewScheduler(store, nil, nil)
	if err := scheduler.Restore(); err != nil {
		t.Fatalf("Restore failed: %v", err)
	}

	if got := scheduler.heap.Len(); got != 2 {
		t.Fatalf("Expected only the two unfinished jobs re-armed, got %d", got)
	}
	if scheduler.heap.Get("success-one") != nil || scheduler.heap.Get("failed-one") != nil {
		t.Error("Expected terminal records to stay out of the scheduling queue")
	}

	rearmed := scheduler.heap.Get("running-one").(*Job)
	if rearmed.Status != StatusPending {
		t.Errorf("Expected a crashed running job to be reset to pending, got %v", rearmed.Status)
	}
}

// TestScheduler_PersistsRunningWhileExecuting 执行期间快照必须是 Running，
// 这样崩溃后的存储能看出任务当时在跑，统计接口也不依赖进程内状态。
func TestScheduler_PersistsRunningWhileExecuting(t *testing.T) {
	store := newMockStore()
	observed := make(chan int, 1)

	scheduler := NewScheduler(store, nil, nil)
	scheduler.RegisterHandler("probe", func(ctx context.Context, job *Job) error {
		snapshot, ok := store.snapshotOf(job.ID)
		if !ok {
			observed <- -1
			return nil
		}
		observed <- snapshot.Status
		return nil
	})

	if err := scheduler.Schedule(&Job{ID: "watched", Name: "probe", TriggerAt: time.Now()}); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}
	if snapshot, ok := store.snapshotOf("watched"); !ok || snapshot.Status != int(StatusPending) {
		t.Fatalf("Expected a pending snapshot before execution, got %+v", snapshot)
	}

	scheduler.executeJob(scheduler.heap.Get("watched").(*Job))

	select {
	case status := <-observed:
		if status != int(StatusRunning) {
			t.Errorf("Expected the handler to observe a running snapshot, got %d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Expected the handler to run")
	}

	if got := scheduler.RunningCount(); got != 0 {
		t.Errorf("Expected no in-flight jobs after execution, got %d", got)
	}
	if snapshot, _ := store.snapshotOf("watched"); snapshot.Status != int(StatusSuccess) {
		t.Errorf("Expected a terminal success snapshot, got %d", snapshot.Status)
	}
}

// TestScheduler_RunningCountTracksInFlight 执行中的计数随执行开始/结束变化。
func TestScheduler_RunningCountTracksInFlight(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	counts := make(chan int, 1)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.RegisterHandler("blocker", func(ctx context.Context, job *Job) error {
		entered <- struct{}{}
		counts <- scheduler.RunningCount()
		<-release
		return ctx.Err()
	})

	go scheduler.executeJob(&Job{ID: "busy", Name: "blocker", TriggerAt: time.Now()})

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Expected the handler to start")
	}

	if got := <-counts; got != 1 {
		t.Errorf("Expected the running handler to see 1 in-flight job, got %d", got)
	}
	close(release)
	// 等 executeJob 走完 defer 归零
	for i := 0; i < 40; i++ {
		if scheduler.RunningCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("Expected the in-flight counter to drop back to 0, got %d", scheduler.RunningCount())
}
