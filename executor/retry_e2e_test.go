package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// schedulerRun 是一次"真实调度器 + 真实 Runner"接线的观察点。
//
// 两个事件通道分别只收 job.failed 与 job.retrying：本卡的判定点是
// "永久失败不再排期重试"，而这两类事件是收尾链路对外的唯一出口。
type schedulerRun struct {
	failed   <-chan core.Event
	retrying <-chan core.Event
	runs     *int32
	last     *atomic.Pointer[core.Job]
}

// startSchedulerWithRunner 把 fixture 的 Runner 挂到一个真实调度器上并立刻启动，
// 记录处理函数被调用的次数与最后一次拿到的任务对象。
func startSchedulerWithRunner(t *testing.T, fixture *runnerFixture, job *core.Job) *schedulerRun {
	t.Helper()

	store, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)

	eventBus := core.NewEventBus(64)
	scheduler := core.NewScheduler(store, nil, eventBus, core.WithLogger(quietLogger()))
	_, failed := eventBus.Subscribe(core.EventJobFailed)
	_, retrying := eventBus.Subscribe(core.EventJobRetrying)

	var runs int32
	var last atomic.Pointer[core.Job]
	handler := fixture.runner.Handler()
	key := fixture.profile.HandlerKey()
	scheduler.RegisterHandler(key, func(ctx context.Context, in *core.Job) error {
		atomic.AddInt32(&runs, 1)
		last.Store(in)
		return handler(ctx, in)
	})

	job.Type = key
	job.Name = key
	require.NoError(t, scheduler.Schedule(job))
	scheduler.Start()

	// 清理顺序有讲究：先让调度器把在途任务收尾，再关存储。
	t.Cleanup(func() {
		scheduler.Stop()
		require.NoError(t, store.Close())
	})

	return &schedulerRun{failed: failed, retrying: retrying, runs: &runs, last: &last}
}

// TestRunner_PermanentFailureNoRetry 是卡片 §5.3 的端到端用例：
// 参数越界这类永久失败经真实调度器走一遍，只执行一次、一条失败事件、没有重试事件。
func TestRunner_PermanentFailureNoRetry(t *testing.T) {
	command := shellCommand(t, "echo hi")
	command.Args = []core.ExecutorArg{{Name: "day", Required: true, Pattern: "^(today)$"}}
	fixture := newRunnerFixture(t, command, nil)

	run := startSchedulerWithRunner(t, fixture, &core.Job{
		ID:         "job-perm-no-retry",
		TriggerAt:  time.Now(),
		MaxRetries: 2,
		RetryDelay: 10 * time.Millisecond,
		Payload:    []byte(`{"args":{"day":"tomorrow"}}`),
	})

	select {
	case ev := <-run.failed:
		assert.Equal(t, true, ev.Metadata["permanent"],
			"失败事件要带上 permanent 标记，运维才知道这条任务不会自己好")
		assert.NotEqual(t, true, ev.Metadata["timeout"])
	case <-time.After(15 * time.Second):
		t.Fatal("没等到失败事件")
	}

	select {
	case <-run.retrying:
		t.Error("永久失败不该排期重试")
	case <-time.After(500 * time.Millisecond):
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(run.runs),
		"非法参数只执行一次：重试只会把同一个错误再做一遍")

	last := run.last.Load()
	require.NotNil(t, last)
	require.NotNil(t, last.Exec)
	assert.True(t, last.Exec.Permanent, "摘要里的 permanent 要与错误标记一致")
}

// TestRunner_PermanentInArtifactMeta 钉住"产物自述文件与任务快照是同一个结论"：
// meta.json 写在 defer 之前，摘要里的 permanent 写在 defer 里，两处若各算各的，
// 运维看接口和看产物目录会得出两个答案。
func TestRunner_PermanentInArtifactMeta(t *testing.T) {
	command := core.ExecutorCommand{
		Name:    "missing_binary",
		Kind:    string(KindBinary),
		Program: "bin/godelayq-does-not-exist.exe",
	}
	fixture := newRunnerFixture(t, command, nil)

	job, err := fixture.run(context.Background(), "job-meta-perm", "")
	require.Error(t, err)

	data, err := os.ReadFile(filepath.Join(fixture.artifacts.Dir(), "job-meta-perm", "a1.meta.json"))
	require.NoError(t, err)
	var meta core.ExecMeta
	require.NoError(t, json.Unmarshal(data, &meta))

	assert.True(t, meta.Permanent, "起进程失败的摘要与 meta.json 都要写明不会重试")
	require.NotNil(t, job.Exec)
	assert.Equal(t, meta.Permanent, job.Exec.Permanent, "两处读到的必须是同一个取值")
}

// TestRunner_TimeoutFailureStillRetries 是同一条接线上的另一头：
// 可重试的失败（超时）经真实调度器仍然照常排期重试，永久判定没有把它一并压掉。
func TestRunner_TimeoutFailureStillRetries(t *testing.T) {
	command, program := slowCommand(t, 5)
	command.Timeout = 200 * time.Millisecond
	fixture := newRunnerFixture(t, command, func(cfg *core.Config, _ *ArtifactOptions) {
		cfg.Executors.RuntimeAllow = append(cfg.Executors.RuntimeAllow, program)
	})

	// RetryDelay 取一分钟：重试副本要留在堆里可观察，不能立刻又跑第二轮
	run := startSchedulerWithRunner(t, fixture, &core.Job{
		ID:         "job-timeout-retry",
		TriggerAt:  time.Now(),
		MaxRetries: 1,
		RetryDelay: time.Minute,
	})

	select {
	case ev := <-run.failed:
		assert.Equal(t, true, ev.Metadata["timeout"])
		_, permanent := ev.Metadata["permanent"]
		assert.False(t, permanent, "超时是可重试的失败，失败事件里不该出现 permanent 键")
	case <-time.After(15 * time.Second):
		t.Fatal("没等到超时失败事件")
	}

	select {
	case <-run.retrying:
	case <-time.After(5 * time.Second):
		t.Fatal("超时之后应当排期重试")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(run.runs),
		"重试副本按一分钟退避排在后面，这里不该已经跑第二轮")
}
