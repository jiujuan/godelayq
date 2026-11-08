// state_machine_test.go 是 TASK-RE01 的状态机规格测试：
// 转移表穷举、非法转移拒绝、取消请求标记（cancelRequested）的三条收尾认领与生命周期。
// 三条取消路径各自的端到端用例分散在既有测试里（TestScheduler_Cancel 堆内、
// TestScheduler_Cancel_PausedJobLandsCancelled 暂停中、TestScheduler_CancelRunningJobNotResurrected
// 执行中），本文件不重复它们，只补转移表与标记语义。
package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCanTransition_Table 是转移表的穷举规格：6×6 全对全错。
// 期望矩阵独立写在这里（与 docs/design/reliable-execution-design.md §6.5 同源）——
// 改 jobTransitions 而不改这张表，测试会拦住，逼着先改文档再改代码。
// from == to 恒真是 canTransition 的 identity 分支，不进矩阵。
func TestCanTransition_Table(t *testing.T) {
	all := []JobStatus{StatusPending, StatusRunning, StatusSuccess, StatusFailed, StatusCancelled, StatusPaused}
	allowed := map[JobStatus]map[JobStatus]bool{
		StatusPending:   {StatusRunning: true, StatusPaused: true, StatusCancelled: true, StatusFailed: true},
		StatusRunning:   {StatusSuccess: true, StatusFailed: true, StatusPending: true, StatusPaused: true, StatusCancelled: true},
		StatusSuccess:   {},
		StatusFailed:    {},
		StatusCancelled: {},
		StatusPaused:    {StatusPending: true, StatusCancelled: true},
	}

	for _, from := range all {
		for _, to := range all {
			want := from == to || allowed[from][to]
			if got := canTransition(from, to); got != want {
				t.Errorf("canTransition(%s -> %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// TestScheduler_Transition_RejectsIllegal 钉住拒绝语义的三件套：
// 状态保持原值、不落盘、事件照发（事件是事实，非法状态写是账本说谎）。
func TestScheduler_Transition_RejectsIllegal(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil, quietLogger())
	events := collectEvents(scheduler.GetEventBus())

	job := &Job{ID: "illegal", Name: "n", Status: StatusSuccess}
	accepted := scheduler.transition(job, StatusPending, Event{
		Type:      EventJobResumed,
		JobID:     "illegal",
		Status:    StatusPending,
		Timestamp: time.Now(),
	})
	if accepted {
		t.Fatal("success→pending 必须被转移表拒绝")
	}
	if job.Status != StatusSuccess {
		t.Errorf("非法转移不得改写状态，实际 %v", job.Status)
	}
	if _, ok := store.snapshotOf("illegal"); ok {
		t.Error("非法转移不得落盘")
	}
	events.waitFor(t, EventJobResumed, "illegal", time.Second)
}

// TestScheduler_Transition_IdentityAlwaysAllowed：原地内容更新（from==to）不视为转移，
// UpdatePending/SetGroup 依赖这条口径，且落盘照常发生。
func TestScheduler_Transition_IdentityAlwaysAllowed(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil, quietLogger())

	job := &Job{ID: "identity", Name: "n", Status: StatusPaused}
	if !scheduler.transition(job, StatusPaused) {
		t.Fatal("from == to 的 identity 转移必须放行")
	}
	stored, ok := store.snapshotOf("identity")
	if !ok {
		t.Fatal("identity 转移要落盘（内容更新）")
	}
	if JobStatus(stored.Status) != StatusPaused {
		t.Errorf("identity 转移不得改状态，实际 %v", JobStatus(stored.Status))
	}
}

// waitCancelMapRegistered 轮询等待任务进入执行（出现在 cancelMap 里）。
func waitCancelMapRegistered(t *testing.T, s *Scheduler, jobID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		_, running := s.cancelMap[jobID]
		s.mu.RUnlock()
		if running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s 未进入执行", jobID)
}

// TestScheduler_CancelRequested_HandlerCompletes：Handler 不响应取消、照常返回成功。
// 工作已经做完，结果为真就按成功落终态；取消标记必须被消费，不能泄漏。
func TestScheduler_CancelRequested_HandlerCompletes(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil, quietLogger())

	release := make(chan struct{})
	scheduler.RegisterHandler("ignore-ctx", func(ctx context.Context, job *Job) error {
		<-release // 刻意不检查 ctx：取消对它无效
		return nil
	})

	scheduler.Schedule(&Job{ID: "cancel-success", Name: "ignore-ctx", TriggerAt: time.Now()})
	scheduler.Start()
	defer scheduler.Stop()
	waitCancelMapRegistered(t, scheduler, "cancel-success")

	if err := scheduler.Cancel("cancel-success"); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
	close(release)
	waitSnapshotStatus(t, store, "cancel-success", StatusSuccess, 3*time.Second)

	scheduler.mu.Lock()
	_, leak := scheduler.cancelRequested["cancel-success"]
	scheduler.mu.Unlock()
	if leak {
		t.Error("cancelRequested 标记泄漏：收尾路径没有消费它")
	}
}

// TestScheduler_CancelRequested_HandlerFails：Handler 不响应取消、自己报了错。
// 按用户意图停住：落 cancelled 终态，不再消耗重试次数、不再排期——
// 失败结论已在 job.failed 事件里，但"失败后重试"不能违背取消意图。
func TestScheduler_CancelRequested_HandlerFails(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil, quietLogger())
	events := collectEvents(scheduler.GetEventBus())

	release := make(chan struct{})
	scheduler.RegisterHandler("ignore-ctx-fail", func(ctx context.Context, job *Job) error {
		<-release
		return errors.New("handler said no despite cancel")
	})

	scheduler.Schedule(&Job{ID: "cancel-fail", Name: "ignore-ctx-fail", TriggerAt: time.Now(), MaxRetries: 3})
	scheduler.Start()
	defer scheduler.Stop()
	waitCancelMapRegistered(t, scheduler, "cancel-fail")

	if err := scheduler.Cancel("cancel-fail"); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
	close(release)
	waitSnapshotStatus(t, store, "cancel-fail", StatusCancelled, 3*time.Second)

	if scheduler.heap.Get("cancel-fail") != nil {
		t.Error("取消后的失败不得排期重试")
	}
	events.waitFor(t, EventJobCancelled, "cancel-fail", time.Second)
}

// TestScheduler_StartClearsCancelRequested：取消标记是进程内意图，
// 跨 Start→Stop→Start 不留残留（与 suspended/retireRequests 同一纪律）。
func TestScheduler_StartClearsCancelRequested(t *testing.T) {
	scheduler := NewScheduler(newMockStore(), nil, nil, quietLogger())

	scheduler.mu.Lock()
	scheduler.cancelRequested["stale"] = struct{}{}
	scheduler.mu.Unlock()

	scheduler.Start()
	defer scheduler.Stop()

	scheduler.mu.Lock()
	_, ok := scheduler.cancelRequested["stale"]
	scheduler.mu.Unlock()
	if ok {
		t.Error("Start 应清空上一代残留的取消标记")
	}
}
