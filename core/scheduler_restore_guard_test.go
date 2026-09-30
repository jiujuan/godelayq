package core

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 这一组是 TASK-E14 的崩溃恢复改判用例。构造方式沿用 scheduler_recovery_test.go：
// 直接往存储里写一份"上次正在执行"的快照，再新建调度器走 Restore，
// 不需要真的把子进程杀掉——本卡要钉住的是恢复时的判定，不是进程终止。

// noopHandler 是不关心内容的占位处理函数，恢复用例只看任务会不会被拦住。
func noopHandler() Handler {
	return func(ctx context.Context, job *Job) error { return nil }
}

// pauseRunningExecGuard 是装配方要装的那条规则（cmd/server 里的实现与此同形）：
// 崩溃瞬间仍是 running 的执行器任务停在 paused 上等人确认。
func pauseRunningExecGuard(scheduler *Scheduler) RestoreGuard {
	return func(snap JobSnapshot) (JobStatus, bool) {
		if JobStatus(snap.Status) != StatusRunning {
			return 0, false
		}
		class, registered := scheduler.HandlerClass(snap.HandlerKey())
		if !registered || class != JobClassExec {
			return 0, false
		}
		return StatusPaused, true
	}
}

// seedStore 按给定状态写几条快照，返回存储。
func seedStore(rows ...JobSnapshot) *mockStore {
	store := newMockStore()
	for _, row := range rows {
		store.jobs[row.ID] = row
	}
	return store
}

// TestRestore_GuardSkipsRunningExec 覆盖第 5.1 第一条：running 的执行器快照被拦在堆外，
// 状态落到 paused（内存与存储都是）、发出一条带崩溃来源的暂停事件、留下一条汇总日志。
func TestRestore_GuardSkipsRunningExec(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	store := seedStore(JobSnapshot{
		ID: "exec-crashed", Name: "夜间报表", Type: "exec.report",
		TriggerAt: past, Status: int(StatusRunning), Attempts: 1,
	})

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	eventBus := NewEventBus(16)
	_, eventCh := eventBus.Subscribe(EventJobPaused)

	scheduler := NewScheduler(store, nil, eventBus, WithLogger(logger))
	scheduler.RegisterHandlerClass("exec.report", noopHandler(), JobClassExec)
	scheduler.SetRestoreGuard(pauseRunningExecGuard(scheduler))

	if err := scheduler.Restore(); err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}

	if scheduler.heap.Len() != 0 {
		t.Errorf("被拦住的任务不该进堆，实际堆长度 %d", scheduler.heap.Len())
	}
	snap, ok := store.snapshotOf("exec-crashed")
	if !ok {
		t.Fatal("存储里应当还有这条任务")
	}
	if JobStatus(snap.Status) != StatusPaused {
		t.Errorf("存储状态应为 paused，实际 %s", JobStatus(snap.Status))
	}

	paused := countEvents(drainEvents(eventCh, 300*time.Millisecond), EventJobPaused)
	if len(paused) != 1 {
		t.Fatalf("应发布一条暂停事件，实际 %d 条", len(paused))
	}
	ev := paused[0]
	if ev.JobID != "exec-crashed" || ev.Status != StatusPaused {
		t.Errorf("事件指向不对：job_id=%s status=%s", ev.JobID, ev.Status)
	}
	if got := ev.Metadata["reason"]; got != RestoreReasonAfterCrash {
		t.Errorf("事件缺少崩溃来源标记，reason=%v", got)
	}
	if got := ev.Metadata["forced"]; got != true {
		t.Errorf("这类暂停不是用户点的，forced 要为 true，实际 %v", got)
	}
	if got := ev.Metadata["attempts"]; got != 1 {
		t.Errorf("事件应带上次执行次数，attempts=%v", got)
	}

	output := logs.String()
	if !strings.Contains(output, "paused executor jobs after crash") || !strings.Contains(output, "count=1") {
		t.Fatalf("缺少规模汇总日志，实际日志：%q", output)
	}
}

// TestRestore_GuardIgnoresPending 覆盖第 5.1 第二条：崩溃时还没开始执行的档位任务照旧入堆。
// 这条与上一条合起来才是本卡的判定口径——拦人的依据是"跑没跑过"，不是"是不是执行器任务"。
func TestRestore_GuardIgnoresPending(t *testing.T) {
	store := seedStore(JobSnapshot{
		ID: "exec-pending", Name: "夜间报表", Type: "exec.report",
		TriggerAt: time.Now().Add(time.Hour), Status: int(StatusPending),
	})

	scheduler := NewScheduler(store, nil, nil)
	scheduler.RegisterHandlerClass("exec.report", noopHandler(), JobClassExec)
	scheduler.SetRestoreGuard(pauseRunningExecGuard(scheduler))

	if err := scheduler.Restore(); err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}

	item := scheduler.heap.Get("exec-pending")
	if item == nil {
		t.Fatal("未开始执行的任务应当照常恢复入堆")
	}
	if got := item.(*Job).Status; got != StatusPending {
		t.Errorf("恢复后的状态应为 pending，实际 %s", got)
	}
	if snap, _ := store.snapshotOf("exec-pending"); JobStatus(snap.Status) != StatusPending {
		t.Errorf("存储状态不该被改动，实际 %s", JobStatus(snap.Status))
	}
}

// TestRestore_NoGuardUnchanged 是回归底线（DoD 第一条）：没装钩子时，
// 恢复的结果与 TASK-E14 之前逐条一致——running 复位为 pending 并重新排期。
func TestRestore_NoGuardUnchanged(t *testing.T) {
	store := seedStore(
		JobSnapshot{ID: "plain-run", Name: "普通任务", Type: "plain", TriggerAt: time.Now().Add(time.Hour), Status: int(StatusRunning)},
		JobSnapshot{ID: "exec-run", Name: "夜间报表", Type: "exec.report", TriggerAt: time.Now().Add(time.Hour), Status: int(StatusRunning)},
	)

	scheduler := NewScheduler(store, nil, nil)
	scheduler.RegisterHandlerClass("exec.report", noopHandler(), JobClassExec)

	if err := scheduler.Restore(); err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}

	if scheduler.heap.Len() != 2 {
		t.Fatalf("两条未完成任务都该回到堆里，实际 %d", scheduler.heap.Len())
	}
	for _, id := range []string{"plain-run", "exec-run"} {
		if got := scheduler.heap.Get(id).(*Job).Status; got != StatusPending {
			t.Errorf("%s 恢复后的状态应为 pending，实际 %s", id, got)
		}
		if snap, _ := store.snapshotOf(id); JobStatus(snap.Status) != StatusRunning {
			t.Errorf("%s 不该在恢复时被写回存储，实际存储状态 %s", id, JobStatus(snap.Status))
		}
	}
	// 类别照旧按注册表盖章：档位任务恢复后仍落在执行器池里
	if got := scheduler.heap.Get("exec-run").(*Job).class; got != JobClassExec {
		t.Errorf("档位任务的类别盖章不该受钩子影响，实际 %d", got)
	}
}

// TestScheduler_SetRestoreGuardAfterStartIsIgnored 与其它 setter 同一条限制：
// Start 的第一步就是 Restore，事后再装来不及拦任何任务，只能忽略并记日志。
func TestScheduler_SetRestoreGuardAfterStartIsIgnored(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	scheduler := NewScheduler(nil, nil, nil, WithLogger(logger))
	scheduler.Start()
	defer scheduler.Stop()

	scheduler.SetRestoreGuard(func(JobSnapshot) (JobStatus, bool) { return StatusPaused, true })

	if !strings.Contains(logs.String(), "SetRestoreGuard ignored") {
		t.Fatalf("Start 之后安装应留下告警日志，实际：%q", logs.String())
	}
	if scheduler.restoreGuardFn() != nil {
		t.Error("Start 之后的安装不该生效")
	}
}

// TestRestore_GuardPanicIsSafe 覆盖第 5.1 第四条：钩子 panic 不中断恢复，
// 这条任务按默认结果入堆，并留下一条 error 记录。
func TestRestore_GuardPanicIsSafe(t *testing.T) {
	store := seedStore(
		JobSnapshot{ID: "boom", Name: "夜间报表", Type: "exec.report", TriggerAt: time.Now().Add(time.Hour), Status: int(StatusRunning)},
		JobSnapshot{ID: "after", Name: "夜间报表", Type: "exec.report", TriggerAt: time.Now().Add(time.Hour), Status: int(StatusRunning)},
	)

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	scheduler := NewScheduler(store, nil, nil, WithLogger(logger))
	scheduler.RegisterHandlerClass("exec.report", noopHandler(), JobClassExec)
	scheduler.SetRestoreGuard(func(snap JobSnapshot) (JobStatus, bool) {
		if snap.ID == "boom" {
			panic("guard exploded")
		}
		return StatusPaused, true
	})

	if err := scheduler.Restore(); err != nil {
		t.Fatalf("钩子 panic 不该让 Restore 返回错误: %v", err)
	}
	if scheduler.heap.Get("boom") == nil {
		t.Error("panic 的那条任务应按默认结果照常恢复入堆")
	}
	if scheduler.heap.Get("after") != nil {
		t.Error("钩子不该被后面的快照跳过：这一条仍要被拦住")
	}
	output := logs.String()
	if !strings.Contains(output, "level=ERROR") || !strings.Contains(output, "restore guard panicked") {
		t.Fatalf("panic 要留下 error 记录，实际日志：%q", output)
	}
	if !strings.Contains(output, "job_id=boom") || !strings.Contains(output, "guard exploded") {
		t.Errorf("日志要指认是哪条任务与什么原因，实际：%q", output)
	}
}

// failingUpdateStore 只让 Update 失败，其余走 mockStore 的正常行为。
type failingUpdateStore struct {
	*mockStore
	err error
}

func (f failingUpdateStore) Update(snapshot JobSnapshot) error { return f.err }

// TestRestore_GuardPausedJobCanBeResumed 钉住"停住不等于丢弃"（DoD 第四条）：
// 被守卫拦下的任务能用现有 Resume 走回待执行，并且重新入堆时类别仍按注册表盖章，
// 于是它回到的是执行器池而不是共享池。批量恢复走的是同一个 Scheduler.Resume，
// 端点侧的覆盖在 api/handlers_lifecycle_test.go 的 batch-ops 用例里。
func TestRestore_GuardPausedJobCanBeResumed(t *testing.T) {
	store := seedStore(JobSnapshot{
		ID: "exec-crashed", Name: "夜间报表", Type: "exec.report",
		TriggerAt: time.Now().Add(-time.Minute), Status: int(StatusRunning),
	})

	var ran atomic.Int32
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.RegisterHandlerClass("exec.report", func(ctx context.Context, job *Job) error {
		ran.Add(1)
		return nil
	}, JobClassExec)
	scheduler.SetRestoreGuard(pauseRunningExecGuard(scheduler))

	if err := scheduler.Restore(); err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}
	if scheduler.heap.Len() != 0 {
		t.Fatalf("恢复时该任务应被拦在堆外，实际堆长度 %d", scheduler.heap.Len())
	}

	resumed, err := scheduler.Resume("exec-crashed")
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	if resumed.Status != StatusPending {
		t.Errorf("恢复后应为 pending，实际 %s", resumed.Status)
	}
	item := scheduler.heap.Get("exec-crashed")
	if item == nil {
		t.Fatal("Resume 之后任务应回到堆里")
	}
	if got := item.(*Job).class; got != JobClassExec {
		t.Errorf("重新入堆后要落回执行器池，实际类别 %d", got)
	}
	// 过期任务由调度循环立即补跑：这一条同时证明"停在 paused 不是为了丢弃，是为了确认后只跑一次"
	scheduler.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && ran.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	scheduler.Stop()
	if got := ran.Load(); got != 1 {
		t.Fatalf("确认后应恰好补跑一次，实际执行 %d 次", got)
	}
}

// TestRestore_GuardStoreUpdateFailure 覆盖第 5.1 第五条：改判落盘失败只记日志，
// 后续快照继续处理。这条任务的后果是"下次启动再判一次"，不是任务丢失。
func TestRestore_GuardStoreUpdateFailure(t *testing.T) {
	store := seedStore(
		JobSnapshot{ID: "exec-one", Name: "夜间报表", Type: "exec.report", TriggerAt: time.Now().Add(time.Hour), Status: int(StatusRunning)},
		JobSnapshot{ID: "exec-two", Name: "夜间报表", Type: "exec.report", TriggerAt: time.Now().Add(time.Hour), Status: int(StatusRunning)},
	)
	updateErr := errors.New("disk full")
	faulty := failingUpdateStore{mockStore: store, err: updateErr}

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	scheduler := NewScheduler(faulty, nil, nil, WithLogger(logger))
	scheduler.RegisterHandlerClass("exec.report", noopHandler(), JobClassExec)
	scheduler.SetRestoreGuard(pauseRunningExecGuard(scheduler))

	if err := scheduler.Restore(); err != nil {
		t.Fatalf("落盘失败不该让 Restore 返回错误: %v", err)
	}
	if scheduler.heap.Len() != 0 {
		t.Errorf("两条都该被拦在堆外（判定与落盘是两件事），实际 %d", scheduler.heap.Len())
	}
	output := logs.String()
	for _, id := range []string{"exec-one", "exec-two"} {
		if !strings.Contains(output, "failed to persist the restored job status") || !strings.Contains(output, "job_id="+id) {
			t.Fatalf("%s 的落盘失败没有记日志，实际日志：%q", id, output)
		}
	}
	if !strings.Contains(output, updateErr.Error()) {
		t.Errorf("日志要带上存储报出的原因，实际：%q", output)
	}
}

// TestRestore_GuardOnlyAffectsExecClass 覆盖第 5.1 第六条与卡片 §9 的第二条风险：
// 同为 running 快照，注册为档位的被停住，注册为普通任务的、以及档位已被删掉（查不到类别）的，
// 都照旧复活。
func TestRestore_GuardOnlyAffectsExecClass(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	eventBus := NewEventBus(16)
	_, eventCh := eventBus.Subscribe(EventJobPaused)

	future := time.Now().Add(time.Hour)
	store := seedStore(
		JobSnapshot{ID: "exec", Name: "夜间报表", Type: "exec.report", TriggerAt: future, Status: int(StatusRunning)},
		JobSnapshot{ID: "plain", Name: "普通任务", Type: "plain", TriggerAt: future, Status: int(StatusRunning)},
		JobSnapshot{ID: "gone", Name: "已删档位", Type: "exec.deleted", TriggerAt: future, Status: int(StatusRunning)},
	)

	scheduler := NewScheduler(store, nil, eventBus, WithLogger(logger))
	scheduler.RegisterHandlerClass("exec.report", noopHandler(), JobClassExec)
	scheduler.RegisterHandler("plain", noopHandler())
	scheduler.SetRestoreGuard(pauseRunningExecGuard(scheduler))

	// HandlerClass 的三条答案要在断言行为之前先钉住：类别与"注册没注册"是守卫的全部输入
	if class, ok := scheduler.HandlerClass("exec.report"); !ok || class != JobClassExec {
		t.Fatalf("档位键应报出 (exec, true)，实际 (%d, %v)", class, ok)
	}
	if class, ok := scheduler.HandlerClass("plain"); !ok || class != JobClassDefault {
		t.Fatalf("普通键应报出 (default, true)，实际 (%d, %v)", class, ok)
	}
	if class, ok := scheduler.HandlerClass("exec.deleted"); ok || class != JobClassDefault {
		t.Fatalf("未注册的键应报出 (default, false)，实际 (%d, %v)", class, ok)
	}

	if err := scheduler.Restore(); err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}

	if scheduler.heap.Get("exec") != nil {
		t.Error("档位任务被漏下了")
	}
	for _, id := range []string{"plain", "gone"} {
		if scheduler.heap.Get(id) == nil {
			t.Errorf("%s 应当照常复活入堆", id)
		}
	}
	if snap, _ := store.snapshotOf("exec"); JobStatus(snap.Status) != StatusPaused {
		t.Errorf("档位任务应停在 paused，实际 %s", JobStatus(snap.Status))
	}
	for _, id := range []string{"plain", "gone"} {
		if snap, _ := store.snapshotOf(id); JobStatus(snap.Status) != StatusRunning {
			t.Errorf("%s 的存储快照不该被改写，实际 %s", id, JobStatus(snap.Status))
		}
	}

	paused := countEvents(drainEvents(eventCh, 300*time.Millisecond), EventJobPaused)
	if len(paused) != 1 || paused[0].JobID != "exec" {
		t.Fatalf("只该有一条暂停事件且指向档位任务，实际 %+v", paused)
	}
	if output := logs.String(); !strings.Contains(output, "paused executor jobs after crash") || !strings.Contains(output, "count=1") {
		t.Fatalf("汇总日志的数量应当只算被停住的那一条，实际日志：%q", output)
	}
}
