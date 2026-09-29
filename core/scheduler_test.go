package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Mock Store
type mockStore struct {
	mu          sync.Mutex
	jobs        map[string]JobSnapshot
	saveErr     error
	loadErr     error
	saveCalls   int
	deleteCalls int
}

func newMockStore() *mockStore {
	return &mockStore{
		jobs: make(map[string]JobSnapshot),
	}
}

func (m *mockStore) Save(job *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCalls++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.jobs[job.ID] = job.ToSnapshot()
	return nil
}

func (m *mockStore) Update(snapshot JobSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[snapshot.ID] = snapshot
	return nil
}

func (m *mockStore) Delete(jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls++
	delete(m.jobs, jobID)
	return nil
}

func (m *mockStore) LoadAll() ([]JobSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	result := make([]JobSnapshot, 0, len(m.jobs))
	for _, job := range m.jobs {
		result = append(result, job)
	}
	return result, nil
}

func (m *mockStore) Flush() error {
	return nil
}

func (m *mockStore) Close() error {
	return nil
}

func (m *mockStore) GetSaveCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveCalls
}

func (m *mockStore) GetDeleteCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteCalls
}

// snapshotOf 读取当前存储中的快照，用于断言状态迁移的落盘时机。
func (m *mockStore) snapshotOf(jobID string) (JobSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, ok := m.jobs[jobID]

	return snapshot, ok
}

// Mock Retry Policy
type mockRetryPolicy struct {
	delay time.Duration
}

func (m *mockRetryPolicy) NextRetry(job *Job) time.Time {
	return time.Now().Add(m.delay)
}

func TestNewScheduler(t *testing.T) {
	store := newMockStore()
	retryPolicy := &mockRetryPolicy{delay: 1 * time.Second}
	eventBus := NewEventBus(10)

	scheduler := NewScheduler(store, retryPolicy, eventBus)

	if scheduler == nil {
		t.Fatal("Expected scheduler to be created")
	}
	if scheduler.heap == nil {
		t.Error("Expected heap to be initialized")
	}
	if scheduler.store != store {
		t.Error("Expected store to be set")
	}
	if scheduler.retryPolicy != retryPolicy {
		t.Error("Expected retry policy to be set")
	}
	if scheduler.eventBus != eventBus {
		t.Error("Expected event bus to be set")
	}
	if scheduler.handlers == nil {
		t.Error("Expected handlers map to be initialized")
	}
	if scheduler.cancelMap == nil {
		t.Error("Expected cancelMap to be initialized")
	}
}

func TestNewScheduler_DefaultRetryPolicy(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	if scheduler.retryPolicy == nil {
		t.Error("Expected default retry policy to be set")
	}
	if scheduler.eventBus == nil {
		t.Error("Expected default event bus to be created")
	}
}

func TestScheduler_RegisterHandler(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	handlerCalled := false
	handler := func(ctx context.Context, job *Job) error {
		handlerCalled = true
		return nil
	}

	scheduler.RegisterHandler("test-job", handler)

	if len(scheduler.handlers) != 1 {
		t.Errorf("Expected 1 handler, got %d", len(scheduler.handlers))
	}

	// Test handler is callable
	if h, ok := scheduler.handlers["test-job"]; ok {
		h(context.Background(), &Job{})
		if !handlerCalled {
			t.Error("Handler was not called")
		}
	} else {
		t.Error("Handler not found")
	}
}

func TestScheduler_Schedule(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	job := &Job{
		Name:      "test-job",
		Payload:   []byte("test"),
		TriggerAt: time.Now().Add(1 * time.Hour),
	}

	err := scheduler.Schedule(job)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Verify job was added to heap
	if scheduler.heap.Len() != 1 {
		t.Errorf("Expected heap length 1, got %d", scheduler.heap.Len())
	}

	// Verify job ID was generated
	if job.ID == "" {
		t.Error("Expected job ID to be generated")
	}

	// Verify job was persisted
	if store.GetSaveCalls() != 1 {
		t.Errorf("Expected 1 save call, got %d", store.GetSaveCalls())
	}

	// Verify status was set
	if job.Status != StatusPending {
		t.Errorf("Expected status Pending, got %v", job.Status)
	}

	// Verify CreatedAt was set
	if job.CreatedAt.IsZero() {
		t.Error("Expected CreatedAt to be set")
	}
}

func TestScheduler_Schedule_WithCron(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	job := &Job{
		Name:     "cron-job",
		CronExpr: "*/5 * * * *", // Every 5 minutes
		IsRepeat: true,
	}

	err := scheduler.Schedule(job)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Verify trigger time was calculated
	if job.TriggerAt.IsZero() {
		t.Error("Expected TriggerAt to be calculated from cron expression")
	}

	// Verify trigger time is in the future
	if !job.TriggerAt.After(time.Now()) {
		t.Error("Expected TriggerAt to be in the future")
	}
}

func TestScheduler_Schedule_InvalidCron(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	job := &Job{
		Name:     "invalid-cron-job",
		CronExpr: "invalid cron",
		IsRepeat: true,
	}

	err := scheduler.Schedule(job)
	if err == nil {
		t.Error("Expected error for invalid cron expression")
	}
}

func TestScheduler_Cancel(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	job := &Job{
		ID:        "cancel-test",
		Name:      "test-job",
		TriggerAt: time.Now().Add(1 * time.Hour),
	}

	scheduler.Schedule(job)

	err := scheduler.Cancel("cancel-test")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Verify job was removed from heap
	if scheduler.heap.Len() != 0 {
		t.Errorf("Expected heap to be empty, got length %d", scheduler.heap.Len())
	}

	// Verify job was deleted from store
	if store.GetDeleteCalls() != 1 {
		t.Errorf("Expected 1 delete call, got %d", store.GetDeleteCalls())
	}
}

func TestScheduler_Cancel_NotFound(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	err := scheduler.Cancel("nonexistent")
	if err != ErrJobNotFound {
		t.Errorf("Expected ErrJobNotFound, got %v", err)
	}
}

func TestScheduler_StartStop(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	if scheduler.running {
		t.Error("Expected scheduler to not be running initially")
	}

	scheduler.Start()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	scheduler.mu.RLock()
	running := scheduler.running
	scheduler.mu.RUnlock()

	if !running {
		t.Error("Expected scheduler to be running after Start()")
	}

	// Test double start (should not panic)
	scheduler.Start()

	scheduler.Stop()

	scheduler.mu.RLock()
	running = scheduler.running
	scheduler.mu.RUnlock()

	if running {
		t.Error("Expected scheduler to be stopped after Stop()")
	}

	// Test double stop (should not panic)
	scheduler.Stop()
}

func TestScheduler_ExecuteJob_Success(t *testing.T) {
	store := newMockStore()
	eventBus := NewEventBus(10)
	scheduler := NewScheduler(store, nil, eventBus)

	executed := make(chan struct{})
	handler := func(ctx context.Context, job *Job) error {
		close(executed)
		return nil
	}

	job := &Job{
		ID:        "exec-test",
		Name:      "test-job",
		Handler:   handler,
		TriggerAt: time.Now(),
	}

	// Subscribe to events
	_, eventCh := eventBus.Subscribe(EventJobStarted, EventJobCompleted)

	scheduler.executeJob(job)

	// Wait for execution
	select {
	case <-executed:
	case <-time.After(2 * time.Second):
		t.Error("Expected handler to be executed")
	}

	// Verify events were published
	eventsReceived := 0
	timeout := time.After(100 * time.Millisecond)

eventLoop:
	for {
		select {
		case event := <-eventCh:
			eventsReceived++
			if event.Type == EventJobStarted {
				if event.Status != StatusRunning {
					t.Errorf("Expected status Running, got %v", event.Status)
				}
			} else if event.Type == EventJobCompleted {
				if event.Status != StatusSuccess {
					t.Errorf("Expected status Success, got %v", event.Status)
				}
			}
		case <-timeout:
			break eventLoop
		}
	}

	if eventsReceived < 2 {
		t.Errorf("Expected at least 2 events, got %d", eventsReceived)
	}
}

func TestScheduler_ExecuteJob_Failure(t *testing.T) {
	store := newMockStore()
	eventBus := NewEventBus(10)
	retryPolicy := &mockRetryPolicy{delay: 10 * time.Millisecond}
	scheduler := NewScheduler(store, retryPolicy, eventBus)

	testErr := errors.New("test error")
	handler := func(ctx context.Context, job *Job) error {
		return testErr
	}

	job := &Job{
		ID:         "fail-test",
		Name:       "test-job",
		Handler:    handler,
		TriggerAt:  time.Now(),
		MaxRetries: 2,
		RetryDelay: 10 * time.Millisecond,
	}

	// Subscribe to events
	_, eventCh := eventBus.Subscribe(EventJobFailed, EventJobRetrying)

	scheduler.executeJob(job)

	// Wait for execution
	time.Sleep(50 * time.Millisecond)

	// Verify failure event
	select {
	case event := <-eventCh:
		if event.Type != EventJobFailed {
			t.Errorf("Expected EventJobFailed, got %v", event.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Expected failure event")
	}
}

func TestScheduler_HandleSuccess_OneTime(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	job := &Job{
		ID:       "success-test",
		IsRepeat: false,
	}

	scheduler.handleSuccess(job)

	if job.Status != StatusSuccess {
		t.Errorf("Expected status Success, got %v", job.Status)
	}

	// 成功任务留下终态快照，而不是被直接删除
	if store.GetDeleteCalls() != 0 {
		t.Errorf("Expected no delete call, got %d", store.GetDeleteCalls())
	}
	stored, ok := store.jobs["success-test"]
	if !ok {
		t.Fatal("Expected a success snapshot to be persisted")
	}
	if stored.Status != int(StatusSuccess) {
		t.Errorf("Expected stored status Success, got %v", JobStatus(stored.Status))
	}
	if stored.UpdatedAt.IsZero() {
		t.Error("Expected UpdatedAt stamped on the terminal snapshot")
	}
}

func TestScheduler_HandleSuccess_Repeat(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	handler := func(ctx context.Context, job *Job) error {
		return nil
	}

	job := &Job{
		ID:       "repeat-test",
		Name:     "repeat-job",
		Handler:  handler,
		CronExpr: "*/5 * * * *",
		IsRepeat: true,
	}

	scheduler.handleSuccess(job)

	// Verify new job was scheduled
	if scheduler.heap.Len() != 1 {
		t.Errorf("Expected 1 job in heap, got %d", scheduler.heap.Len())
	}

	// Verify new job has future trigger time
	nextJob := scheduler.heap.Peek().(*Job)
	if !nextJob.TriggerAt.After(time.Now()) {
		t.Error("Expected next trigger time to be in the future")
	}
}

// TestScheduler_HandleSuccess_RepeatDropsPreviousExec 钉住执行摘要的第四处搬运：
// Cron 重排出来的下一轮是一次新的执行，带着上一轮的退出码与输出预览，
// 会让详情页在新的一轮跑完之前显示旧结论。
func TestScheduler_HandleSuccess_RepeatDropsPreviousExec(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	job := &Job{
		ID:       "repeat-exec-test",
		Name:     "exec.nightly_report",
		Handler:  func(ctx context.Context, job *Job) error { return nil },
		CronExpr: "*/5 * * * *",
		IsRepeat: true,
		Exec:     &ExecMeta{Kind: "script", Profile: "nightly_report", ExitCode: 1, DurationMs: 20},
	}

	scheduler.handleSuccess(job)

	item := scheduler.heap.Peek()
	if item == nil {
		t.Fatal("Expected the cron job to be rescheduled")
	}
	nextJob := item.(*Job)
	if nextJob.Exec != nil {
		t.Errorf("Cron 下一轮不应带上一次的执行摘要，实际 %+v", *nextJob.Exec)
	}
	// 本轮的结论仍挂在原对象上：收尾日志与事件读的是它
	if job.Exec == nil {
		t.Error("重排不应清空本轮任务的摘要")
	}
}

func TestScheduler_HandleFailure_WithRetries(t *testing.T) {
	store := newMockStore()
	retryPolicy := &mockRetryPolicy{delay: 10 * time.Millisecond}
	scheduler := NewScheduler(store, retryPolicy, nil)

	job := &Job{
		ID:         "retry-test",
		Name:       "test-job",
		MaxRetries: 3,
		RetryCount: 1,
		RetryDelay: 10 * time.Millisecond,
	}

	scheduler.handleFailure(job)

	// Verify retry job was scheduled
	if scheduler.heap.Len() != 1 {
		t.Errorf("Expected 1 retry job in heap, got %d", scheduler.heap.Len())
	}
}

func TestScheduler_HandleFailure_MaxRetriesExceeded(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	job := &Job{
		ID:         "max-retry-test",
		Name:       "test-job",
		MaxRetries: 3,
		RetryCount: 3,
	}

	scheduler.handleFailure(job)

	// Verify no retry was scheduled
	if scheduler.heap.Len() != 0 {
		t.Errorf("Expected 0 jobs in heap, got %d", scheduler.heap.Len())
	}

	// Verify status is failed
	if job.Status != StatusFailed {
		t.Errorf("Expected status Failed, got %v", job.Status)
	}
}

func TestScheduler_HeapLen(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	if scheduler.HeapLen() != 0 {
		t.Errorf("Expected heap length 0, got %d", scheduler.HeapLen())
	}

	scheduler.Schedule(&Job{
		Name:      "job1",
		TriggerAt: time.Now().Add(1 * time.Hour),
	})

	if scheduler.HeapLen() != 1 {
		t.Errorf("Expected heap length 1, got %d", scheduler.HeapLen())
	}

	scheduler.Schedule(&Job{
		Name:      "job2",
		TriggerAt: time.Now().Add(2 * time.Hour),
	})

	if scheduler.HeapLen() != 2 {
		t.Errorf("Expected heap length 2, got %d", scheduler.HeapLen())
	}
}

func TestScheduler_GetEventBus(t *testing.T) {
	eventBus := NewEventBus(10)
	scheduler := NewScheduler(nil, nil, eventBus)

	result := scheduler.GetEventBus()
	if result != eventBus {
		t.Error("Expected GetEventBus to return the same event bus")
	}
}

func TestScheduler_Integration_ExecuteAtTime(t *testing.T) {
	store := newMockStore()
	eventBus := NewEventBus(10)
	scheduler := NewScheduler(store, nil, eventBus)

	executed := make(chan struct{})
	handler := func(ctx context.Context, job *Job) error {
		close(executed)
		return nil
	}

	scheduler.RegisterHandler("integration-test", handler)

	// Schedule job to execute very soon
	job := &Job{
		Name:      "integration-test",
		TriggerAt: time.Now().Add(50 * time.Millisecond),
	}

	scheduler.Schedule(job)
	scheduler.Start()
	defer scheduler.Stop()

	// Wait for execution
	select {
	case <-executed:
	case <-time.After(2 * time.Second):
		t.Error("Expected job to be executed")
	}

	if scheduler.HeapLen() != 0 {
		t.Errorf("Expected heap to be empty, got length %d", scheduler.HeapLen())
	}
}

func TestScheduler_Integration_MultipleJobs(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	executionOrder := make([]string, 0)
	var mu sync.Mutex

	handler := func(ctx context.Context, job *Job) error {
		mu.Lock()
		executionOrder = append(executionOrder, job.Name)
		mu.Unlock()
		return nil
	}

	scheduler.RegisterHandler("multi-test", handler)

	now := time.Now()

	// Schedule jobs in reverse order
	scheduler.Schedule(&Job{
		Name:      "multi-test",
		Payload:   []byte("job3"),
		TriggerAt: now.Add(150 * time.Millisecond),
	})
	scheduler.Schedule(&Job{
		Name:      "multi-test",
		Payload:   []byte("job1"),
		TriggerAt: now.Add(50 * time.Millisecond),
	})
	scheduler.Schedule(&Job{
		Name:      "multi-test",
		Payload:   []byte("job2"),
		TriggerAt: now.Add(100 * time.Millisecond),
	})

	scheduler.Start()
	defer scheduler.Stop()

	// Wait for all executions
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(executionOrder) != 3 {
		t.Errorf("Expected 3 jobs executed, got %d", len(executionOrder))
	}
}

func TestScheduler_CancelRunningJob(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.RegisterHandler("long-job", func(ctx context.Context, job *Job) error { return nil })

	jobStarted := make(chan struct{})
	cancelObserved := make(chan error, 1)

	handler := func(ctx context.Context, job *Job) error {
		close(jobStarted)
		<-ctx.Done()
		cancelObserved <- ctx.Err()
		return ctx.Err()
	}

	job := &Job{
		ID:        "cancel-running",
		Name:      "long-job",
		Handler:   handler,
		TriggerAt: time.Now(),
	}

	scheduler.Schedule(job)
	scheduler.Start()
	defer scheduler.Stop()

	// Wait for job to start
	select {
	case <-jobStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Expected job to start")
	}

	// Cancel the job
	if err := scheduler.Cancel("cancel-running"); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	select {
	case err := <-cancelObserved:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Expected job to be cancelled")
	}
}
