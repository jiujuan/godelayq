package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/jiujuan/godelayq/internal/executor"
	"github.com/jiujuan/godelayq/internal/store/jobs"
	"github.com/jiujuan/godelayq/internal/store/mgr"
	"github.com/jiujuan/godelayq/internal/store/observe"
	"github.com/jiujuan/godelayq/internal/store/pgtest"
	"github.com/stretchr/testify/require"
)

// TestRun_PostgresStoresEndToEnd 是 PG 方言的装配级端到端用例（B06）：
// 三库全部按真实生产闭包打开（jobs/mgr/observe 各一份一次性 PG 测试库），
// 配置节走 dialect: postgres + dsn——与真实部署切方言的取值形态一致。
// 链路：run() 完整装配 → 总线发事件、任务库直写、管理库存分组 → 优雅关停 →
// 重新打开三库核对数据仍在（"重启后历史仍在"的本体）。
//
// 无 GODELAYQ_TEST_PG_DSN 时整个用例干净跳过。
func TestRun_PostgresStoresEndToEnd(t *testing.T) {
	jobsDSN := pgtest.Database(t, "srv_jobs")
	mgrDSN := pgtest.Database(t, "srv_mgr")
	obsDSN := pgtest.Database(t, "srv_obs")

	cfg := core.DefaultConfig()
	cfg.Store.Type = "sqlite"
	cfg.Store.SQLite = core.StoreSQLiteConfig{Dialect: "postgres", DSN: jobsDSN, BusyTimeout: core.DefaultJobsBusyTimeout, Synchronous: core.DefaultJobsSynchronous}
	cfg.MgrStore = core.MgrStoreConfig{Enabled: true, Dialect: "postgres", DSN: mgrDSN, BusyTimeout: core.DefaultMgrBusyTimeout, Synchronous: core.DefaultMgrSynchronous}
	obs := core.DefaultConfig().Observability
	obs.Enabled = true
	obs.Dialect = "postgres"
	obs.DSN = obsDSN
	cfg.Observability = obs

	var logs strings.Builder
	logger := newCaptureLogger(&logs)

	// 真实生产闭包：newStore/newMgrDB/newObservabilityDB/newEventLog/newAuditLog
	// 全部来自 defaultRuntimeDeps，按配置节的方言开关走 PG。
	deps := defaultRuntimeDeps(cfg, "", nil, logger)
	deps.notifySignals = func(ch chan<- os.Signal, sig ...os.Signal) {
		go func() { ch <- syscall.SIGTERM }()
	}
	deps.timeout = 20 * time.Millisecond

	// 换掉的是进程壳（调度器探针 + 假 HTTP 服务），存储与写入器都是真的。
	scheduler := newSpyScheduler()
	var gotStore core.Store
	originalNewStore := deps.newStore
	deps.newStore = func() (core.Store, error) {
		s, err := originalNewStore()
		if err == nil {
			gotStore = s
		}
		return s, err
	}
	deps.newScheduler = func(store core.Store, retry core.RetryPolicy, bus *core.EventBus) schedulerAPI {
		return scheduler
	}
	var gotMgr mgrAPI
	originalNewMgrDB := deps.newMgrDB
	deps.newMgrDB = func(cfg core.Config, logger *slog.Logger) (mgrAPI, error) {
		h, err := originalNewMgrDB(cfg, logger)
		if err == nil {
			gotMgr = h
		}
		return h, err
	}
	deps.newServer = func(_ schedulerAPI, _ core.Store, _ string, _ *executor.Registry, _ *executor.ArtifactStore, _ *observabilityAPI, _ *profileStoreAPI) (serverAPI, error) {
		// 装配完成、服务未起：任务库直写一条、管理库存一个分组、总线发两条事件。
		require.NotNil(t, gotStore, "装配顺序保证 newStore 先于 newServer")
		require.NoError(t, gotStore.Save(&core.Job{
			ID: "job-pg-1", Name: "demo", Status: core.StatusPending, TriggerAt: time.Now(),
		}))
		handle, ok := gotMgr.(*mgr.Handle)
		require.True(t, ok, "管理库句柄应已晚绑定")
		require.NoError(t, mgr.NewGroupStore(handle).Save(core.Group{
			Name: "prod", Description: "生产环境", Color: "#ff0000",
		}))
		scheduler.GetEventBus().Publish(core.Event{Type: core.EventJobScheduled, JobID: "job-pg-1",
			JobName: "demo", Status: core.StatusPending, Timestamp: time.Now()})
		scheduler.GetEventBus().Publish(core.Event{Type: core.EventJobStarted, JobID: "job-pg-1",
			JobName: "demo", Status: core.StatusRunning, Timestamp: time.Now()})
		return newFakeServer(), nil
	}

	require.NoError(t, run(deps))

	// 启动日志按方言呈现：三库打开、事件写入器挂上；Path 是脱敏 DSN，口令不出现。
	for _, want := range []string{"observability enabled", "events_writer=true", "mgr database opened"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("expected %q in startup logs, got %q", want, logs.String())
		}
	}
	require.NotContains(t, logs.String(), "admin123", "启动日志不得泄漏 DSN 口令")

	// 重新打开任务库：直写的那条快照还在——恢复链路的数据源成立。
	reopened, err := jobs.Open(cfg.Store.SQLite, 0, 0, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	defer reopened.Close()
	snaps, err := reopened.LoadAll()
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	require.Equal(t, "job-pg-1", snaps[0].ID)

	// 重新打开观测库：两条事件落库、schema 版本照常。
	obsAgain, err := observe.Open(cfg.Observability, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	defer obsAgain.Close()
	stats, err := obsAgain.Stats()
	require.NoError(t, err)
	require.Equal(t, int64(2), stats.Events)
	require.Equal(t, 2, stats.SchemaVersion)

	// 重新打开管理库：分组在、大小写变体可查（lower(name) 唯一索引那条链）。
	mgrAgain, err := mgr.Open(cfg.MgrStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	defer mgrAgain.Close()
	group, ok, err := mgr.NewGroupStore(mgrAgain).Get("PROD")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "生产环境", group.Description)
}
