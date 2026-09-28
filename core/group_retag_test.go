package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 分组改写的核心风险是"只改存储、堆里还是旧值"：任务一执行就把旧分组写回去，
// 改名在跑一轮之后悄悄失效。下面的用例专门钉住堆内条目与快照同时生效。

func TestScheduler_SetGroup_RewritesHeapEntryAndSnapshot(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	job := &Job{ID: "grp-1", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "nightly"}
	require.NoError(t, scheduler.Schedule(job))

	require.NoError(t, scheduler.SetGroup("grp-1", "nightly-b"))

	item := scheduler.heap.Get("grp-1")
	require.NotNil(t, item, "待执行任务应仍在堆里")
	assert.Equal(t, "nightly-b", item.(*Job).Group, "堆内条目必须一起改")

	snap, ok := store.snapshotOf("grp-1")
	require.True(t, ok)
	assert.Equal(t, "nightly-b", snap.Group)
}

func TestScheduler_SetGroup_KeepsNewGroupAfterTheJobRuns(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	scheduler.RegisterHandler("noop", func(ctx context.Context, job *Job) error { return nil })
	events := collectEvents(scheduler.GetEventBus())

	job := &Job{ID: "grp-run", Name: "noop", Type: "noop", TriggerAt: time.Now().Add(20 * time.Millisecond), Group: "nightly"}
	require.NoError(t, scheduler.Schedule(job))
	require.NoError(t, scheduler.SetGroup("grp-run", "nightly-b"))

	scheduler.Start()
	defer scheduler.Stop()

	events.waitFor(t, EventJobCompleted, "grp-run", 2*time.Second)

	snap, ok := store.snapshotOf("grp-run")
	require.True(t, ok)
	assert.Equal(t, int(StatusSuccess), snap.Status)
	assert.Equal(t, "nightly-b", snap.Group, "执行收尾不得把旧分组写回去")
}

func TestScheduler_SetGroup_PausedJobUpdatesSnapshotInPlace(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	job := &Job{ID: "grp-paused", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "nightly"}
	require.NoError(t, scheduler.Schedule(job))
	_, err := scheduler.Pause("grp-paused")
	require.NoError(t, err)

	require.NoError(t, scheduler.SetGroup("grp-paused", "weekend"))

	snap, ok := store.snapshotOf("grp-paused")
	require.True(t, ok)
	assert.Equal(t, "weekend", snap.Group)
	assert.Equal(t, int(StatusPaused), snap.Status, "改分组不该把任务唤回")
	assert.Zero(t, scheduler.heap.Len())
}

func TestScheduler_SetGroup_RunningJobIsRejected(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.RegisterHandler("slow", func(ctx context.Context, job *Job) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})

	job := &Job{ID: "grp-running", Name: "slow", Type: "slow", TriggerAt: time.Now(), Group: "nightly"}
	require.NoError(t, scheduler.Schedule(job))
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

	err := scheduler.SetGroup("grp-running", "weekend")
	require.ErrorIs(t, err, ErrJobNotPending, "执行中的任务由 worker 持有，改分组会被收尾覆盖")

	snap, ok := store.snapshotOf("grp-running")
	require.True(t, ok)
	assert.Equal(t, "nightly", snap.Group, "被拒绝的改动不该留下半个值")
}

func TestScheduler_SetGroup_UnknownJob(t *testing.T) {
	scheduler := NewScheduler(newMockStore(), nil, nil)

	assert.ErrorIs(t, scheduler.SetGroup("missing", "nightly"), ErrJobNotFound)
}

func TestScheduler_SetGroup_MemoryOnlyDeploymentStillMovesTheHeapEntry(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	job := &Job{ID: "grp-mem", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "nightly"}
	require.NoError(t, scheduler.Schedule(job))

	require.NoError(t, scheduler.SetGroup("grp-mem", ""))

	item := scheduler.heap.Get("grp-mem")
	require.NotNil(t, item)
	assert.Empty(t, item.(*Job).Group)
}

func TestScheduler_SetGroup_SameValueIsANoOp(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	job := &Job{ID: "grp-same", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "nightly"}
	require.NoError(t, scheduler.Schedule(job))

	require.NoError(t, scheduler.SetGroup("grp-same", "nightly"))
}

func TestScheduler_RetagGroup_RenamesAcrossHeapAndStore(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	// 堆里的待执行任务
	require.NoError(t, scheduler.Schedule(&Job{ID: "rt-1", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "Nightly"}))
	// 暂停中的任务：不在堆里，只在存储
	require.NoError(t, scheduler.Schedule(&Job{ID: "rt-2", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "nightly"}))
	_, err := scheduler.Pause("rt-2")
	require.NoError(t, err)
	// 终态留痕：与别的分组
	require.NoError(t, store.Update(JobSnapshot{ID: "rt-3", Name: "test-job", Group: "nightly", Status: int(StatusSuccess)}))
	require.NoError(t, store.Update(JobSnapshot{ID: "rt-4", Name: "test-job", Group: "weekend", Status: int(StatusSuccess)}))

	changed, err := scheduler.RetagGroup("nightly", "nightly-b")
	require.NoError(t, err)
	assert.Equal(t, 3, changed, "大小写不同的同组也应一起改名，终态留痕同样跟着走")

	item := scheduler.heap.Get("rt-1")
	require.NotNil(t, item)
	assert.Equal(t, "nightly-b", item.(*Job).Group)

	for _, id := range []string{"rt-1", "rt-2", "rt-3"} {
		snap, ok := store.snapshotOf(id)
		require.True(t, ok, id)
		assert.Equal(t, "nightly-b", snap.Group, id)
	}

	untouched, ok := store.snapshotOf("rt-4")
	require.True(t, ok)
	assert.Equal(t, "weekend", untouched.Group)
}

func TestScheduler_RetagGroup_DetachClearsEveryMatch(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	require.NoError(t, scheduler.Schedule(&Job{ID: "dt-1", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "nightly"}))
	require.NoError(t, scheduler.Schedule(&Job{ID: "dt-2", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "nightly"}))
	_, err := scheduler.Pause("dt-2")
	require.NoError(t, err)

	changed, err := scheduler.RetagGroup("nightly", "")
	require.NoError(t, err)
	assert.Equal(t, 2, changed)

	for _, id := range []string{"dt-1", "dt-2"} {
		snap, ok := store.snapshotOf(id)
		require.True(t, ok, id)
		assert.Empty(t, snap.Group, id)
	}
}

func TestScheduler_RetagGroup_EmptySourceMatchesNothing(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	require.NoError(t, scheduler.Schedule(&Job{ID: "no-src", Name: "test-job", TriggerAt: time.Now().Add(time.Hour)}))

	changed, err := scheduler.RetagGroup("", "someone")
	require.NoError(t, err)
	assert.Zero(t, changed, "空来源等于把所有未分组任务吞进来，必须直接拒绝")

	snap, ok := store.snapshotOf("no-src")
	require.True(t, ok)
	assert.Empty(t, snap.Group)
}

func TestScheduler_RetagGroup_MemoryOnlyDeploymentHasNothingToRewrite(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	require.NoError(t, scheduler.Schedule(&Job{ID: "rt-mem", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "nightly"}))

	changed, err := scheduler.RetagGroup("nightly", "nightly-b")
	require.NoError(t, err)
	assert.Zero(t, changed, "没有存储就没有可枚举的任务；分组改名以持久化记录为范围")
}
