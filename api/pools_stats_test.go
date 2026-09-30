package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// startTwoPools 起一个"普通池 1 + 执行器池 1"的调度器：
// 普通任务一条、执行器任务 execQueued+1 条，处理函数都卡在上下文结束上。
// 返回两个"处理器已进入"的信号，调用方等到它们就说明两个池同时各有一个在跑。
func startTwoPools(t *testing.T, srv *Server, execQueued int) {
	t.Helper()

	scheduler := srv.scheduler
	scheduler.SetConcurrency(1)
	scheduler.SetExecConcurrency(1)
	scheduler.SetExecQueueCapacity(execQueued)

	plainIn := make(chan struct{}, 1)
	execIn := make(chan struct{}, 1)
	blocker := func(signal chan struct{}) core.Handler {
		return func(ctx context.Context, job *core.Job) error {
			signal <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
	}
	scheduler.RegisterHandler("plain_work", blocker(plainIn))
	scheduler.RegisterHandlerClass("exec_work", blocker(execIn), core.JobClassExec)

	now := time.Now()
	require.NoError(t, scheduler.Schedule(&core.Job{ID: "stats-plain", Name: "plain_work", TriggerAt: now}))
	for i := 0; i < execQueued+1; i++ {
		require.NoError(t, scheduler.Schedule(&core.Job{
			ID: "stats-exec-" + strconv.Itoa(i), Name: "exec_work", TriggerAt: now,
		}))
	}

	scheduler.Start()
	t.Cleanup(scheduler.Stop)

	for _, signal := range []chan struct{}{plainIn, execIn} {
		select {
		case <-signal:
		case <-time.After(5 * time.Second):
			t.Fatal("两个池没有同时跑起来，这条用例没测到两池并存")
		}
	}
}

// TestGetStats_RunningIncludesExecPool 是 §5.3 第一条：
// /stats 的 running 是两池之和——看数字的人要的是"正在执行几个"，不是"普通池几个"。
func TestGetStats_RunningIncludesExecPool(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	startTwoPools(t, srv, 1)

	recorder := doGet(t, srv, "/api/v1/stats", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var stats StatsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &stats), recorder.Body.String())
	assert.Equal(t, 2, stats.Running, "running 必须是两个池之和")

	// 合成一个数字不等于丢掉细节：拆分维度仍然能从 runtime 读到
	runtime := decodeRuntime(t, doGet(t, srv, "/api/v1/admin/runtime", nil))
	assert.Equal(t, 1, runtime.Scheduler.Running, "RuntimeStats.Running 只含普通池")
	assert.Equal(t, 1, runtime.Scheduler.ExecRunning)
}

// TestAdminRuntime_ExposesExecPool 是 §5.3 第二条：
// /admin/runtime 透出四个执行器池字段，并且数值对得上。
func TestAdminRuntime_ExposesExecPool(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	startTwoPools(t, srv, 1)

	// 第二条档位任务进队列的时刻晚于"worker 取走第一条"，而 startTwoPools 只等到
	// 两个 Handler 进入的信号；机器忙时这一步会拖到读接口之后，所以在测试协程里
	// 轮询到队列计数到位再断言（同 handlers_lifecycle_test.go 的收尾轮询写法，
	// 交给 require.Eventually 会让断言跑在非测试 goroutine 上）。
	deadline := time.Now().Add(5 * time.Second)
	var runtime RuntimeResponse
	for {
		runtime = decodeRuntime(t, doGet(t, srv, "/api/v1/admin/runtime", nil))
		if runtime.Scheduler.ExecRunning == 1 && runtime.Scheduler.ExecQueueLength == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("执行器池没进入预期状态：running=%d queue_length=%d， wanted 1/1",
				runtime.Scheduler.ExecRunning, runtime.Scheduler.ExecQueueLength)
		}
		time.Sleep(10 * time.Millisecond)
	}

	assert.Equal(t, 1, runtime.Scheduler.ExecWorkers)
	assert.Equal(t, 1, runtime.Scheduler.ExecQueueCap, "未显式配置队列容量时与 exec worker 数相等")
	assert.Equal(t, 1, runtime.Scheduler.ExecRunning)
	// 队列容量 1、worker 全被占住：另一条档位任务必须还排在执行器队列里
	assert.Equal(t, 1, runtime.Scheduler.ExecQueueLength,
		"队列里的排队数要单独能读到，否则看不出是队列满还是 worker 满")

	encoded, err := json.Marshal(runtime.Scheduler)
	require.NoError(t, err)
	for _, key := range []string{`"exec_workers"`, `"exec_queue_capacity"`, `"exec_queue_length"`, `"exec_running"`} {
		assert.Contains(t, string(encoded), key, "响应要带出拆分字段")
	}
}

// TestAdminRuntime_DisabledExecPoolReadsZero 是 DoD 第五条在接口侧的样子：
// 没建执行器池时这四个字段全是 0，普通任务的数字照旧只有一个总数。
func TestAdminRuntime_DisabledExecPoolReadsZero(t *testing.T) {
	srv := newSecurityServer(t, Security{})
	scheduler := srv.scheduler
	scheduler.SetConcurrency(1)

	in := make(chan struct{}, 1)
	scheduler.RegisterHandler("plain_work", func(ctx context.Context, job *core.Job) error {
		in <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	require.NoError(t, scheduler.Schedule(&core.Job{
		ID: "plain-only", Name: "plain_work", TriggerAt: time.Now(),
	}))
	scheduler.Start()
	t.Cleanup(scheduler.Stop)

	select {
	case <-in:
	case <-time.After(5 * time.Second):
		t.Fatal("普通任务没跑起来")
	}

	runtime := decodeRuntime(t, doGet(t, srv, "/api/v1/admin/runtime", nil))
	assert.Equal(t, 1, runtime.Scheduler.Running)
	assert.Zero(t, runtime.Scheduler.ExecWorkers)
	assert.Zero(t, runtime.Scheduler.ExecQueueCap)
	assert.Zero(t, runtime.Scheduler.ExecQueueLength)
	assert.Zero(t, runtime.Scheduler.ExecRunning)
}
