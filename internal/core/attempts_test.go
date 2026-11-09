// attempts_test.go 是 TASK-RE02 的调度器挂点测试：用内存账本替身证明
// "每次尝试各有一行、trigger 与 seq 语义正确、Restore 收口悬空行"。
// 表结构与 SQL 形状在 store/jobs 的双后端测试里。
package core

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mockLedger 是 AttemptLedger 的内存替身：记录调用流水量，供断言轮询读取。
// finishes 按单任务 seq 键控（单任务用例）；finishesByJob 按 "jobID#seq" 键控
// （多任务并跑的用例——seq 在不同任务间会撞号）。
type mockLedger struct {
	mu            sync.Mutex
	begins        []Attempt
	finishes      map[int]Attempt
	finishesByJob map[string]Attempt
	abandoned     []int
	prunes        []string // 每次 PruneAttempts 记 "jobID#keepLast"（W3/Q10 的位置断言用）
}

func newMockLedger() *mockLedger {
	return &mockLedger{finishes: make(map[int]Attempt), finishesByJob: make(map[string]Attempt)}
}

func (m *mockLedger) BeginAttempt(jobID string, rec Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.begins = append(m.begins, rec)
	return nil
}

func (m *mockLedger) FinishAttempt(jobID string, seq int, rec Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finishes[seq] = rec
	m.finishesByJob[jobID+"#"+strconv.Itoa(seq)] = rec
	return nil
}

func (m *mockLedger) AbandonAttempts(jobID string, uptoSeq int, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abandoned = append(m.abandoned, uptoSeq)
	return nil
}

func (m *mockLedger) Attempts(jobID string, limit int) ([]Attempt, error) { return nil, nil }

func (m *mockLedger) PruneAttempts(jobID string, keepLast int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prunes = append(m.prunes, jobID+"#"+strconv.Itoa(keepLast))
	return nil
}

func (m *mockLedger) beginCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.begins)
}

func (m *mockLedger) snapshot() (begins []Attempt, finishes map[int]Attempt, abandoned []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Attempt(nil), m.begins...), func() map[int]Attempt {
		out := make(map[int]Attempt, len(m.finishes))
		for k, v := range m.finishes {
			out[k] = v
		}
		return out
	}(), append([]int(nil), m.abandoned...)
}

// finishOf 按任务+seq 取收口行（多任务并跑的用例）。
func (m *mockLedger) finishOf(jobID string, seq int) (Attempt, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.finishesByJob[jobID+"#"+strconv.Itoa(seq)]
	return rec, ok
}

// waitForLedger 轮询等待账本行数到达期望值（worker 异步收尾）。
func waitForLedger(t *testing.T, l *mockLedger, wantBegins int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if l.beginCount() >= wantBegins {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("账本开始行未到达 %d 条，实际 %d", wantBegins, l.beginCount())
}

// TestScheduler_AttemptLedger_Success：成功任务一行账本，scheduled 触发、success 收口。
func TestScheduler_AttemptLedger_Success(t *testing.T) {
	store := newMockStore()
	ledger := newMockLedger()
	scheduler := NewScheduler(store, nil, nil, quietLogger(), WithAttemptLedger(ledger, 0))
	scheduler.RegisterHandler("ok", func(ctx context.Context, job *Job) error { return nil })

	scheduler.Schedule(&Job{ID: "led-ok", Name: "ok", TriggerAt: time.Now()})
	scheduler.Start()
	defer scheduler.Stop()

	waitSnapshotStatus(t, store, "led-ok", StatusSuccess, 3*time.Second)
	waitForLedger(t, ledger, 1)

	begins, finishes, _ := ledger.snapshot()
	require.Equal(t, 1, begins[0].Seq, "首次尝试 seq 从 1 起")
	require.Equal(t, AttemptTriggerScheduled, begins[0].Trigger)
	require.Equal(t, AttemptRunning, begins[0].Status)
	fin, ok := finishes[1]
	if !ok {
		t.Fatal("seq 1 缺少收口行")
	}
	require.Equal(t, AttemptSuccess, fin.Status)
	require.False(t, fin.FinishedAt.IsZero())
}

// TestScheduler_AttemptLedger_Retry：失败重试两行账本——seq 连续、
// trigger 分别是 scheduled 与 retry、job.started 事件的 attempt 值单调（RE01 缺陷修复的回看面）。
func TestScheduler_AttemptLedger_Retry(t *testing.T) {
	store := newMockStore()
	ledger := newMockLedger()
	policy := &mockRetryPolicy{delay: 5 * time.Millisecond}
	scheduler := NewScheduler(store, policy, nil, quietLogger(), WithAttemptLedger(ledger, 0))
	scheduler.RegisterHandler("flaky", func(ctx context.Context, job *Job) error {
		return errors.New("boom")
	})

	scheduler.Schedule(&Job{ID: "led-retry", Name: "flaky", TriggerAt: time.Now(), MaxRetries: 1, RetryDelay: time.Millisecond})
	scheduler.Start()
	defer scheduler.Stop()

	// 终态：重试 1 次后耗尽 → failed
	waitSnapshotStatus(t, store, "led-retry", StatusFailed, 3*time.Second)
	waitForLedger(t, ledger, 2)

	begins, finishes, _ := ledger.snapshot()
	if len(begins) != 2 {
		t.Fatalf("两次执行应有两行开始记录，实际 %d", len(begins))
	}
	require.Equal(t, 1, begins[0].Seq)
	require.Equal(t, AttemptTriggerScheduled, begins[0].Trigger)
	require.Equal(t, 2, begins[1].Seq)
	require.Equal(t, AttemptTriggerRetry, begins[1].Trigger)
	require.Equal(t, AttemptFailed, finishes[1].Status)
	require.Equal(t, AttemptFailed, finishes[2].Status)

	stored, ok := store.snapshotOf("led-retry")
	if !ok {
		t.Fatal("任务应落终态快照")
	}
	require.Equal(t, 2, stored.Attempts, "快照的 Attempts 计数跨重试单调")
}

// TestScheduler_AttemptLedger_RestoreAbandons：崩溃前 running 的快照在 Restore 时
// 账本行落 abandoned，补跑开出新行（trigger=restore）。
func TestScheduler_AttemptLedger_RestoreAbandons(t *testing.T) {
	store := newMockStore()
	ledger := newMockLedger()
	// 崩溃现场：任务在跑（快照 running，Attempts=1），账本挂着一行 running
	require.NoError(t, store.Update(JobSnapshot{
		ID: "led-crash", Name: "ok", Status: int(StatusRunning), Attempts: 1,
		TriggerAt: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-2 * time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
	}))
	require.NoError(t, ledger.BeginAttempt("led-crash", Attempt{
		JobID: "led-crash", Seq: 1, Status: AttemptRunning, Trigger: AttemptTriggerScheduled,
		StartedAt: time.Now().Add(-time.Hour),
	}))

	scheduler := NewScheduler(store, nil, nil, quietLogger(), WithAttemptLedger(ledger, 0))
	scheduler.RegisterHandler("ok", func(ctx context.Context, job *Job) error { return nil })

	// Start 第一步是 Restore；补跑完成后任务到终态
	scheduler.Start()
	defer scheduler.Stop()

	waitSnapshotStatus(t, store, "led-crash", StatusSuccess, 3*time.Second)
	waitForLedger(t, ledger, 2)

	_, _, abandoned := ledger.snapshot()
	require.Equal(t, []int{1}, abandoned, "Restore 应把 seq<=1 的 running 行收口为 abandoned")

	begins, _, _ := ledger.snapshot()
	require.Equal(t, 2, begins[1].Seq)
	require.Equal(t, AttemptTriggerRestore, begins[1].Trigger, "补跑行的触发来源是 restore")
}

// TestScheduler_AttemptLedger_NilIsNoop：JSON 部署（无账本）不注入即降级——
// 执行链路照常，不炸不记。
func TestScheduler_AttemptLedger_NilIsNoop(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil, quietLogger())
	scheduler.RegisterHandler("ok", func(ctx context.Context, job *Job) error { return nil })

	scheduler.Schedule(&Job{ID: "led-nil", Name: "ok", TriggerAt: time.Now()})
	scheduler.Start()
	defer scheduler.Stop()

	waitSnapshotStatus(t, store, "led-nil", StatusSuccess, 3*time.Second)
}

// TestJob_CloneForRetryCarriesAttempts：重试副本带原计数与 retry 来源章（RE02 缺陷修复）。
func TestJob_CloneForRetryCarriesAttempts(t *testing.T) {
	job := &Job{ID: "x", Attempts: 3, RetryCount: 1, RetryDelay: time.Second}
	clone := job.CloneForRetry(time.Now())
	if clone.Attempts != 3 {
		t.Errorf("重试副本应保留 Attempts=3，实际 %d", clone.Attempts)
	}
	if clone.dispatchSource != AttemptTriggerRetry {
		t.Errorf("重试副本应盖 retry 来源章，实际 %v", clone.dispatchSource)
	}
	if job.dispatchSource != AttemptTriggerScheduled {
		t.Errorf("原任务的来源章不得被改动，实际 %v", clone.dispatchSource)
	}
}

// classifiedError 是 AttemptOutcome 的测试替身（TASK-RE04）：core 不能 import
// executor（依赖方向 executor → core），接口读取语义用替身验证，executor 侧的
// 真实实现由 exit_class_test.go 钉住。
type classifiedError struct {
	class      string
	retryable  bool
	exitCode   int
	hasCode    bool
	httpStatus int
	hasStatus  bool
}

func (e *classifiedError) Error() string                  { return "classified: " + e.class }
func (e *classifiedError) AttemptClass() string           { return e.class }
func (e *classifiedError) AttemptRetryable() (bool, bool) { return e.retryable, true }
func (e *classifiedError) AttemptExitCode() (int, bool)   { return e.exitCode, e.hasCode }
func (e *classifiedError) AttemptSignal() string          { return "" }
func (e *classifiedError) AttemptHTTPStatus() (int, bool) { return e.httpStatus, e.hasStatus }
func (e *classifiedError) AttemptErrorText() string       { return e.Error() }

// TestScheduler_AttemptLedger_FailureOutcome：失败明细落账（TASK-RE04）——
// AttemptOutcome 的字段进账本行，class 与事件 metadata 同源。
func TestScheduler_AttemptLedger_FailureOutcome(t *testing.T) {
	store := newMockStore()
	ledger := newMockLedger()
	scheduler := NewScheduler(store, nil, nil, quietLogger(), WithAttemptLedger(ledger, 0))
	scheduler.RegisterHandler("classified", func(ctx context.Context, job *Job) error {
		return &classifiedError{class: "transport", retryable: true}
	})
	scheduler.RegisterHandler("plain", func(ctx context.Context, job *Job) error {
		return errors.New("plain boom")
	})
	scheduler.RegisterHandler("observed", func(ctx context.Context, job *Job) error {
		return &classifiedError{class: "exit_code", exitCode: 9, hasCode: true}
	})
	events := collectEvents(scheduler.GetEventBus())

	// 执行器形态的失败（有 outcome）：class/retryable 进账本行
	scheduler.Schedule(&Job{ID: "led-outcome", Name: "classified", TriggerAt: time.Now()})
	// 普通处理函数失败：handler_error + 错误文本
	scheduler.Schedule(&Job{ID: "led-plain", Name: "plain", TriggerAt: time.Now()})
	// exit_code 类别：退出码 9 进列
	scheduler.Schedule(&Job{ID: "led-exit", Name: "observed", TriggerAt: time.Now()})
	scheduler.Start()
	defer scheduler.Stop()

	waitSnapshotStatus(t, store, "led-outcome", StatusFailed, 3*time.Second)
	waitSnapshotStatus(t, store, "led-plain", StatusFailed, 3*time.Second)
	waitSnapshotStatus(t, store, "led-exit", StatusFailed, 3*time.Second)
	waitForLedger(t, ledger, 3)

	// 各任务的收口行按 job+seq 取（三个任务都是 seq=1，全局 seq 键会撞号）
	outcome, ok := ledger.finishOf("led-outcome", 1)
	require.True(t, ok)
	require.Equal(t, "transport", outcome.Class)
	require.True(t, outcome.RetryableSet)
	require.True(t, outcome.Retryable)

	plain, ok := ledger.finishOf("led-plain", 1)
	require.True(t, ok)
	require.Equal(t, "handler_error", plain.Class)
	require.False(t, plain.RetryableSet, "普通 handler 的失败没有可重试性判定")
	require.Equal(t, "plain boom", plain.Error)

	exit, ok := ledger.finishOf("led-exit", 1)
	require.True(t, ok)
	require.Equal(t, "exit_code", exit.Class)
	require.True(t, exit.ExitCodeSet)
	require.Equal(t, 9, exit.ExitCode)

	// job.failed 事件的 metadata 带 class（与账本行同源）
	events.mu.Lock()
	allFailed := countEvents(events.items, EventJobFailed)
	events.mu.Unlock()
	require.Len(t, allFailed, 3)
	for _, ev := range allFailed {
		require.NotNil(t, ev.Metadata["class"], "job.failed 事件必须带 class")
	}
}

// TestAttemptClassOf：类别判定的顺序即口径——超时优先、接口其次、handler_error 兜底。
func TestAttemptClassOf(t *testing.T) {
	require.Equal(t, "timeout", attemptClassOf(context.DeadlineExceeded, true))
	require.Equal(t, "timeout", attemptClassOf(errors.New("unrelated"), true))
	require.Equal(t, "transport", attemptClassOf(&classifiedError{class: "transport"}, false))
	require.Equal(t, "handler_error", attemptClassOf(errors.New("plain"), false))
}

// W3/Q10：prune 按 seq 取模触发——只在 attempts % keepLast == 0 的收尾跑，
// 不逢取模点的收尾（含终态）不补裁；cron 高频路径的空跑 DELETE 因此消除。
// mockLedger 记的是 keepLast（每次调用同值），位置断言按"逐 seq 的累计次数"做。
func TestFinishAttempt_PruneThrottledBySeqModulo(t *testing.T) {
	ledger := newMockLedger()
	scheduler := NewScheduler(newMockStore(), nil, nil, quietLogger(), WithAttemptLedger(ledger, 3))

	job := &Job{ID: "w3", Status: StatusRunning}
	cumulative := make(map[int]int, 9) // seq -> 该次收尾后的累计 prune 次数
	for seq := 1; seq <= 7; seq++ {
		job.Attempts = seq
		scheduler.finishAttempt(job, AttemptFailed, nil)
		ledger.mu.Lock()
		cumulative[seq] = len(ledger.prunes)
		ledger.mu.Unlock()
	}
	require.Equal(t, map[int]int{1: 0, 2: 0, 3: 1, 4: 1, 5: 1, 6: 2, 7: 2}, cumulative,
		"prune 恰在 seq=3、6 两个取模点各触发一次；seq=7（终态不逢取模）不补裁")

	// 取模点恰逢终态：照常裁剪，不做任何特殊化。
	job.Attempts = 9
	scheduler.finishAttempt(job, AttemptFailed, nil)
	ledger.mu.Lock()
	total := len(ledger.prunes)
	ledger.mu.Unlock()
	require.Equal(t, 3, total)
}
