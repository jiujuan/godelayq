// faultinject_test.go 是 TASK-RE08 的核心域故障注入：flaky 替身按"第 N 次调用"
// 注入失败，验证"状态写失败任务不丢、账本失败执行不断、关停与取消竞态口径正确"。
// 注入都在接口边界（Store/AttemptLedger），不摸私有状态；断言只写对外可观察行为。
package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// flakyStore 包装 core.Store：failOn[method] = N 表示该方法的第 N 次调用起全部失败。
type flakyStore struct {
	Store
	mu     sync.Mutex
	failOn map[string]int
	calls  map[string]int
}

func newFlakyStore(inner Store, failOn map[string]int) *flakyStore {
	return &flakyStore{Store: inner, failOn: failOn, calls: map[string]int{}}
}

func (s *flakyStore) maybeFail(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[method]++
	if n, ok := s.failOn[method]; ok && s.calls[method] >= n {
		return fmt.Errorf("injected %s failure at call #%d", method, s.calls[method])
	}
	return nil
}

func (s *flakyStore) Save(job *Job) error {
	if err := s.maybeFail("Save"); err != nil {
		return err
	}
	return s.Store.Save(job)
}

func (s *flakyStore) Update(snapshot JobSnapshot) error {
	if err := s.maybeFail("Update"); err != nil {
		return err
	}
	return s.Store.Update(snapshot)
}

func (s *flakyStore) Delete(jobID string) error {
	if err := s.maybeFail("Delete"); err != nil {
		return err
	}
	return s.Store.Delete(jobID)
}

func (s *flakyStore) LoadAll() ([]JobSnapshot, error) {
	if err := s.maybeFail("LoadAll"); err != nil {
		return nil, err
	}
	return s.Store.LoadAll()
}

// flakyLedger 包装 AttemptLedger：注入语义同 flakyStore。
type flakyLedger struct {
	AttemptLedger
	mu     sync.Mutex
	failOn map[string]int
	calls  map[string]int
}

func newFlakyLedger(inner AttemptLedger, failOn map[string]int) *flakyLedger {
	return &flakyLedger{AttemptLedger: inner, failOn: failOn, calls: map[string]int{}}
}

func (l *flakyLedger) maybeFail(method string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls[method]++
	if n, ok := l.failOn[method]; ok && l.calls[method] >= n {
		return fmt.Errorf("injected %s failure at call #%d", method, l.calls[method])
	}
	return nil
}

func (l *flakyLedger) BeginAttempt(jobID string, rec Attempt) error {
	if err := l.maybeFail("BeginAttempt"); err != nil {
		return err
	}
	return l.AttemptLedger.BeginAttempt(jobID, rec)
}

func (l *flakyLedger) FinishAttempt(jobID string, seq int, rec Attempt) error {
	if err := l.maybeFail("FinishAttempt"); err != nil {
		return err
	}
	return l.AttemptLedger.FinishAttempt(jobID, seq, rec)
}

func (l *flakyLedger) PruneAttempts(jobID string, keepLast int) error {
	if err := l.maybeFail("PruneAttempts"); err != nil {
		return err
	}
	return l.AttemptLedger.PruneAttempts(jobID, keepLast)
}

// TestFault_UpdateFailsOnRunningPersist：运行态落盘失败——任务不丢（快照保持
// 上一次成功写入的状态）、错误日志可见、调度循环不死、后续收尾照常走。
func TestFault_UpdateFailsOnRunningPersist(t *testing.T) {
	inner := newMockStore()
	// 第 1 次 Update = Schedule 的落盘（成功），第 2 次起失败 = running/终态落盘全失败
	store := newFlakyStore(inner, map[string]int{"Update": 2})
	scheduler := NewScheduler(store, nil, nil, quietLogger())
	scheduler.RegisterHandler("ok", func(ctx context.Context, job *Job) error { return nil })

	require.NoError(t, scheduler.Schedule(&Job{ID: "fault-run", Name: "ok", TriggerAt: time.Now()}))
	scheduler.Start()
	defer scheduler.Stop()

	// 收尾不因写失败而挂死：任务在内存里到终态（等 stub 侧看不到 store 变化，
	// 直接等 worker 走完——用 inFlight 归零判断）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if scheduler.RunningCount() == 0 && scheduler.HeapLen() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, 0, scheduler.RunningCount())

	// 快照保持第一次成功写入的 pending：写失败不制造半截状态，重启由 Restore
	// 重放（at-least-once 的既有口径，任务不丢）
	snap, ok := inner.snapshotOf("fault-run")
	require.True(t, ok, "Schedule 的首次落盘应成功")
	require.Equal(t, StatusPending, JobStatus(snap.Status))
}

// TestFault_UpdateFailsOnFailurePath：失败收尾时落盘失败——重试仍被排上
//（Schedule 的容错口径：落盘失败照常入堆），调度循环不死。断言走 retrying
// 事件而不是堆长度：重试延迟毫秒级，堆窗口窄于轮询间隔。
func TestFault_UpdateFailsOnFailurePath(t *testing.T) {
	inner := newMockStore()
	store := newFlakyStore(inner, map[string]int{"Update": 2})
	scheduler := NewScheduler(store, &mockRetryPolicy{delay: time.Millisecond}, nil, quietLogger())
	events := collectEvents(scheduler.GetEventBus())
	scheduler.RegisterHandler("flaky", func(ctx context.Context, job *Job) error {
		return errors.New("boom")
	})

	require.NoError(t, scheduler.Schedule(&Job{ID: "fault-fail", Name: "flaky", TriggerAt: time.Now(), MaxRetries: 1, RetryDelay: time.Millisecond}))
	scheduler.Start()
	defer scheduler.Stop()

	events.waitFor(t, EventJobRetrying, "fault-fail", 3*time.Second)
}

// TestFault_BeginAttemptFails：账本开始行写失败——执行不断，收尾行的 upsert
// 语义把账本补全（不因 begin 的单点失败永远缺一行）。
func TestFault_BeginAttemptFails(t *testing.T) {
	inner := newMockStore()
	ledger := newMockLedger()
	faulty := newFlakyLedger(ledger, map[string]int{"BeginAttempt": 1})
	scheduler := NewScheduler(inner, nil, nil, quietLogger(), WithAttemptLedger(faulty, 0))
	scheduler.RegisterHandler("ok", func(ctx context.Context, job *Job) error { return nil })

	require.NoError(t, scheduler.Schedule(&Job{ID: "fault-ledger", Name: "ok", TriggerAt: time.Now()}))
	scheduler.Start()
	defer scheduler.Stop()

	waitSnapshotStatus(t, inner, "fault-ledger", StatusSuccess, 3*time.Second)

	// 收尾行落了（begin 失败被 finish 的 upsert 补上）
	require.Eventually(t, func() bool {
		_, finishes, _ := ledger.snapshot()
		_, ok := finishes[1]
		return ok
	}, 3*time.Second, 5*time.Millisecond, "finish 行应存在（upsert 补齐 begin 的缺口）")
}

// TestFault_ShutdownLandsPending：优雅关停打断在途任务——落 pending（不消耗
// 重试、不判失败），重启照常重跑。
func TestFault_ShutdownLandsPending(t *testing.T) {
	inner := newMockStore()
	scheduler := NewScheduler(inner, nil, nil, quietLogger())
	started := make(chan struct{})
	scheduler.RegisterHandler("hang", func(ctx context.Context, job *Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})

	require.NoError(t, scheduler.Schedule(&Job{ID: "fault-shutdown", Name: "hang", TriggerAt: time.Now()}))
	scheduler.Start()
	<-started
	scheduler.Stop()

	snap, ok := inner.snapshotOf("fault-shutdown")
	require.True(t, ok)
	require.Equal(t, StatusPending, JobStatus(snap.Status), "优雅关停在途任务落 pending")
	require.NotZero(t, snap.Attempts, "账本计数不因打断清零")
}

// TestFault_ShutdownWithCancelInFlight：取消请求在途时优雅关停——取消认领优先，
// 任务落 cancelled（用户的取消意图不因关停被改写成 pending 重跑）。
func TestFault_ShutdownWithCancelInFlight(t *testing.T) {
	inner := newMockStore()
	scheduler := NewScheduler(inner, nil, nil, quietLogger())
	started := make(chan struct{})
	scheduler.RegisterHandler("hang", func(ctx context.Context, job *Job) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})

	require.NoError(t, scheduler.Schedule(&Job{ID: "fault-cancel", Name: "hang", TriggerAt: time.Now()}))
	scheduler.Start()
	<-started
	require.NoError(t, scheduler.Cancel("fault-cancel"))
	scheduler.Stop()

	snap, ok := inner.snapshotOf("fault-cancel")
	require.True(t, ok)
	require.Equal(t, StatusCancelled, JobStatus(snap.Status), "取消认领优先于关停复位")
}

// TestFault_MisfireGraceBoundary：宽限边界与"时钟回拨"（TriggerAt 出现在未来）
// 都不触发 misfire 事件——applyMisfirePolicy 的判定是严格 Before，边界值归入不错过。
func TestFault_MisfireGraceBoundary(t *testing.T) {
	scheduler, _, events := misfireScenario(t, "fire_once", time.Minute)

	now := time.Now()
	// 恰在宽限边界（now-60s）：严格 Before 为假 → 不错过
	exact := &Job{ID: "boundary", Name: "t", CronExpr: "0 * * * * *", IsRepeat: true, TriggerAt: now.Add(-time.Minute)}
	scheduler.applyMisfirePolicy(exact, now)
	require.False(t, events.has(EventJobMisfired, "boundary"), "宽限边界不算错过")

	// 时钟回拨场景：TriggerAt 出现在"未来" → 不错过、不改写
	future := &Job{ID: "rollback", Name: "t", CronExpr: "0 * * * * *", IsRepeat: true, TriggerAt: now.Add(time.Hour)}
	scheduler.applyMisfirePolicy(future, now)
	require.False(t, events.has(EventJobMisfired, "rollback"))
	require.Equal(t, now.Add(time.Hour).UnixNano(), future.TriggerAt.UnixNano())
}
