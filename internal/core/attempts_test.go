// attempts_test.go 是 TASK-RE02 的调度器挂点测试：用内存账本替身证明
// "每次尝试各有一行、trigger 与 seq 语义正确、Restore 收口悬空行"。
// 表结构与 SQL 形状在 store/jobs 的双后端测试里。
package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mockLedger 是 AttemptLedger 的内存替身：记录调用流水量，供断言轮询读取。
type mockLedger struct {
	mu        sync.Mutex
	begins    []Attempt
	finishes  map[int]Attempt
	abandoned []int
}

func newMockLedger() *mockLedger {
	return &mockLedger{finishes: make(map[int]Attempt)}
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
	return nil
}

func (m *mockLedger) AbandonAttempts(jobID string, uptoSeq int, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abandoned = append(m.abandoned, uptoSeq)
	return nil
}

func (m *mockLedger) Attempts(jobID string, limit int) ([]Attempt, error) { return nil, nil }

func (m *mockLedger) PruneAttempts(jobID string, keepLast int) error { return nil }

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
		t.Errorf("原任务的来源章不得被改动，实际 %v", job.dispatchSource)
	}
}
