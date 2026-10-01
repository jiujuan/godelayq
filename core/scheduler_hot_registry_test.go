package core

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本卡的两条新入口都为"档位的在线管理"服务：删档位要先摘掉 handler，
// 再把这一类型下没跑完的任务集体钉住。核心风险有两个，下面的用例逐个盯：
// 一是 handlers/handlerClasses 只删一半（恢复守卫会据此把僵尸任务钉成 paused），
// 二是批量暂停顺手中止了正在执行的那条（与 D6"不强杀"相反）。

func TestScheduler_UnregisterHandler_ClearsBothHandlerTables(t *testing.T) {
	scheduler := NewScheduler(newMockStore(), nil, nil)
	scheduler.RegisterHandlerClass("exec.demo", func(context.Context, *Job) error { return nil }, JobClassExec)
	scheduler.RegisterHandler("email", func(context.Context, *Job) error { return nil })

	require.True(t, scheduler.UnregisterHandler("exec.demo"))

	_, ok := scheduler.LookupHandler("exec.demo")
	assert.False(t, ok, "handler 表里必须摘干净")
	_, ok = scheduler.HandlerClass("exec.demo")
	assert.False(t, ok, "类别表里的孤儿会让恢复守卫把已删档位的任务钉成 paused")
	assert.NotContains(t, scheduler.HandlerNames(), "exec.demo")

	// 别的键一条都不该被牵连
	class, ok := scheduler.HandlerClass("email")
	assert.True(t, ok)
	assert.Equal(t, JobClassDefault, class)
}

func TestScheduler_UnregisterHandler_UnknownKeyChangesNothing(t *testing.T) {
	scheduler := NewScheduler(newMockStore(), nil, nil)
	scheduler.RegisterHandler("email", func(context.Context, *Job) error { return nil })

	assert.False(t, scheduler.UnregisterHandler("missing"))

	assert.Equal(t, []string{"email"}, scheduler.HandlerNames())
}

func TestScheduler_UnregisterHandler_ThenReRegisterRestoresService(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	key := "exec.demo"

	scheduler.RegisterHandlerClass(key, func(context.Context, *Job) error { return nil }, JobClassExec)
	require.True(t, scheduler.UnregisterHandler(key))
	scheduler.RegisterHandlerClass(key, func(context.Context, *Job) error { return nil }, JobClassExec)

	handler, ok := scheduler.LookupHandler(key)
	require.True(t, ok, "PUT 换档定义走的就是重注册这条路")
	require.NotNil(t, handler)
	class, ok := scheduler.HandlerClass(key)
	require.True(t, ok)
	assert.Equal(t, JobClassExec, class)

	// 重注册之后任务真的能按新 handler 跑完
	events := collectEvents(scheduler.GetEventBus())
	require.NoError(t, scheduler.Schedule(&Job{
		ID: "ur-rebind", Name: "demo", Type: key, TriggerAt: time.Now().Add(20 * time.Millisecond),
	}))
	scheduler.Start()
	defer scheduler.Stop()

	events.waitFor(t, EventJobCompleted, "ur-rebind", 2*time.Second)
}

func TestScheduler_UnregisterHandler_LeavesScheduledJobInHeapToFailAtExecute(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	key := "exec.demo"
	scheduler.RegisterHandler(key, func(context.Context, *Job) error { return nil })

	// 堆里的任务触发时间放在摘除之后：它已经在队列里，摘除注册键不该把它送走
	require.NoError(t, scheduler.Schedule(&Job{
		ID: "ur-orphan", Name: "demo", Type: key, TriggerAt: time.Now().Add(50 * time.Millisecond),
	}))
	require.True(t, scheduler.UnregisterHandler(key))

	require.NotNil(t, scheduler.heap.Get("ur-orphan"), "摘除注册键不碰堆")

	scheduler.Start()
	defer scheduler.Stop()

	events := collectEvents(scheduler.GetEventBus())
	events.waitFor(t, EventJobFailed, "ur-orphan", 2*time.Second)

	snap, ok := store.snapshotOf("ur-orphan")
	require.True(t, ok)
	assert.Equal(t, int(StatusFailed), snap.Status, "找不到处理函数按既有分支判失败，不新增行为")
}

func TestScheduler_PauseByHandlerKey_PausesEveryPendingJobOfThatType(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	key := "exec.demo"

	for _, id := range []string{"pk-1", "pk-2", "pk-3"} {
		require.NoError(t, scheduler.Schedule(&Job{
			ID: id, Name: "demo", Type: key, TriggerAt: time.Now().Add(time.Hour),
		}))
	}

	paused, err := scheduler.PauseByHandlerKey(key)
	require.NoError(t, err)
	assert.Equal(t, 3, paused)

	assert.Zero(t, scheduler.heap.Len(), "暂停的任务不该留在堆里")
	for _, id := range []string{"pk-1", "pk-2", "pk-3"} {
		snap, ok := store.snapshotOf(id)
		require.True(t, ok, id)
		assert.Equal(t, int(StatusPaused), snap.Status, id)
	}
}

func TestScheduler_PauseByHandlerKey_SurvivesReloadAndRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	store, err := NewJSONFileStore(path)
	require.NoError(t, err)
	scheduler := NewScheduler(store, nil, nil)
	key := "exec.demo"

	require.NoError(t, scheduler.Schedule(&Job{
		ID: "pk-file", Name: "demo", Type: key, TriggerAt: time.Now().Add(time.Hour),
	}))
	require.NoError(t, scheduler.Schedule(&Job{
		ID: "pk-other", Name: "other", Type: "email", TriggerAt: time.Now().Add(time.Hour),
	}))

	paused, err := scheduler.PauseByHandlerKey(key)
	require.NoError(t, err)
	assert.Equal(t, 1, paused)
	require.NoError(t, store.Flush())
	require.NoError(t, store.Close())

	// 重启：档位还在（这里用同一个键重新注册），但 paused 不因为重启而解除
	reopened, err := NewJSONFileStore(path)
	require.NoError(t, err)
	defer reopened.Close()
	restarted := NewScheduler(reopened, nil, nil)
	restarted.RegisterHandler(key, func(context.Context, *Job) error { return nil })
	restarted.RegisterHandler("email", func(context.Context, *Job) error { return nil })
	require.NoError(t, restarted.Restore())

	assert.Equal(t, 1, restarted.heap.Len(), "只有那个类型之外的任务被重新排期")
	require.NotNil(t, restarted.heap.Get("pk-other"))

	snap, ok := findStored(reopened, "pk-file")
	require.True(t, ok)
	assert.Equal(t, int(StatusPaused), snap.Status)
}

// findStored 从存储里按 ID 取快照，测试断言用。
func findStored(store Store, jobID string) (JobSnapshot, bool) {
	snapshots, err := store.LoadAll()
	if err != nil {
		return JobSnapshot{}, false
	}
	for _, snap := range snapshots {
		if snap.ID == jobID {
			return snap, true
		}
	}
	return JobSnapshot{}, false
}

func TestScheduler_PauseByHandlerKey_TouchesOnlyItsOwnPendingJobs(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	key := "exec.demo"

	// 同类型的待执行任务
	require.NoError(t, scheduler.Schedule(&Job{ID: "mix-pending", Name: "demo", Type: key, TriggerAt: time.Now().Add(time.Hour)}))
	// 同类型、已经暂停
	require.NoError(t, scheduler.Schedule(&Job{ID: "mix-paused", Name: "demo", Type: key, TriggerAt: time.Now().Add(time.Hour)}))
	_, err := scheduler.Pause("mix-paused")
	require.NoError(t, err)
	// 同类型的终态留痕与别的类型
	require.NoError(t, store.Update(JobSnapshot{ID: "mix-done", Name: "demo", Type: key, Status: int(StatusSuccess)}))
	require.NoError(t, store.Update(JobSnapshot{ID: "mix-cancelled", Name: "demo", Type: key, Status: int(StatusCancelled)}))
	require.NoError(t, scheduler.Schedule(&Job{ID: "mix-other", Name: "sync", Type: "email", TriggerAt: time.Now().Add(time.Hour)}))

	paused, err := scheduler.PauseByHandlerKey(key)
	require.NoError(t, err)
	assert.Equal(t, 1, paused, "只有该类型的待执行任务计入")

	for id, want := range map[string]int{
		"mix-pending":   int(StatusPaused),
		"mix-paused":    int(StatusPaused),
		"mix-done":      int(StatusSuccess),
		"mix-cancelled": int(StatusCancelled),
		"mix-other":     int(StatusPending),
	} {
		snap, ok := store.snapshotOf(id)
		require.True(t, ok, id)
		assert.Equal(t, want, snap.Status, id)
	}
	require.NotNil(t, scheduler.heap.Get("mix-other"), "别的类型一条都不该动")
}

func TestScheduler_PauseByHandlerKey_SkipsRunningJobWithoutCancellingIt(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(1)
	key := "exec.demo"
	scheduler.RegisterHandler(key, func(ctx context.Context, job *Job) error {
		select {
		case started <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	require.NoError(t, scheduler.Schedule(&Job{ID: "run-1", Name: "demo", Type: key, TriggerAt: time.Now()}))
	require.NoError(t, scheduler.Schedule(&Job{ID: "run-2", Name: "demo", Type: key, TriggerAt: time.Now().Add(time.Hour)}))

	scheduler.Start()
	defer func() {
		close(release)
		scheduler.Stop()
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("任务没有开始执行")
	}

	paused, err := scheduler.PauseByHandlerKey(key)
	require.NoError(t, err)
	assert.Equal(t, 1, paused, "运行中的那条不计入条数")

	snap, ok := store.snapshotOf("run-1")
	require.True(t, ok)
	assert.Equal(t, int(StatusRunning), snap.Status, "删档位不该中止已经在跑的执行（D6）")
	assert.True(t, scheduler.isRunning("run-1"))

	snap2, ok := store.snapshotOf("run-2")
	require.True(t, ok)
	assert.Equal(t, int(StatusPaused), snap2.Status)
}

func TestScheduler_PauseByHandlerKey_SecondCallIsIdempotent(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	key := "exec.demo"

	require.NoError(t, scheduler.Schedule(&Job{ID: "idem-1", Name: "demo", Type: key, TriggerAt: time.Now().Add(time.Hour)}))
	require.NoError(t, scheduler.Schedule(&Job{ID: "idem-2", Name: "demo", Type: key, TriggerAt: time.Now().Add(time.Hour)}))

	first, err := scheduler.PauseByHandlerKey(key)
	require.NoError(t, err)
	assert.Equal(t, 2, first)

	second, err := scheduler.PauseByHandlerKey(key)
	require.NoError(t, err)
	assert.Zero(t, second, "重复调用不改变结果，调用方因此可以放心重试")

	for _, id := range []string{"idem-1", "idem-2"} {
		snap, ok := store.snapshotOf(id)
		require.True(t, ok, id)
		assert.Equal(t, int(StatusPaused), snap.Status, id)
	}
}

func TestScheduler_PauseByHandlerKey_BlankKeyOrNoStorePausesNothing(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	require.NoError(t, scheduler.Schedule(&Job{ID: "blank-1", Name: "demo", Type: "exec.demo", TriggerAt: time.Now().Add(time.Hour)}))

	for _, key := range []string{"", "   "} {
		paused, err := scheduler.PauseByHandlerKey(key)
		require.NoError(t, err)
		assert.Zero(t, paused, "空键等于把所有未注册类型的任务一起钉住，必须直接拒绝")
	}

	snap, ok := store.snapshotOf("blank-1")
	require.True(t, ok)
	assert.Equal(t, int(StatusPending), snap.Status)

	// 纯内存部署：没有存储就没有可枚举的任务（与 RetagGroup 同一条）
	memoryOnly := NewScheduler(nil, nil, nil)
	require.NoError(t, memoryOnly.Schedule(&Job{ID: "mem-1", Name: "demo", Type: "exec.demo", TriggerAt: time.Now().Add(time.Hour)}))
	paused, err := memoryOnly.PauseByHandlerKey("exec.demo")
	require.NoError(t, err)
	assert.Zero(t, paused)
	require.NotNil(t, memoryOnly.heap.Get("mem-1"), "没存储时不该把任务从堆里变走")
}

func TestScheduler_PauseByHandlerKey_UnknownJobTypePausesNothing(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	require.NoError(t, scheduler.Schedule(&Job{ID: "none-1", Name: "demo", Type: "exec.demo", TriggerAt: time.Now().Add(time.Hour)}))

	paused, err := scheduler.PauseByHandlerKey("exec.gone")
	require.NoError(t, err)
	assert.Zero(t, paused)

	snap, ok := store.snapshotOf("none-1")
	require.True(t, ok)
	assert.Equal(t, int(StatusPending), snap.Status)
}

// 新增的运行期写入口不能破坏 s.mu 的覆盖范围：这里只断言 -race 干净，不比业务结果。
func TestScheduler_UnregisterHandler_RaceWithSchedule(t *testing.T) {
	scheduler := NewScheduler(newMockStore(), nil, nil)
	handler := func(context.Context, *Job) error { return nil }

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("exec.p%d", i)
			for round := 0; round < 20; round++ {
				scheduler.RegisterHandlerClass(key, handler, JobClassExec)
				if err := scheduler.Schedule(&Job{
					ID: fmt.Sprintf("race-%d-%d", i, round), Name: "demo", Type: key,
					TriggerAt: time.Now().Add(time.Hour),
				}); err != nil {
					t.Errorf("schedule failed: %v", err)
					return
				}
				scheduler.UnregisterHandler(key)
				_, _ = scheduler.LookupHandler(key)
				_, _ = scheduler.HandlerClass(key)
			}
		}(i)
	}
	wg.Wait()
}
