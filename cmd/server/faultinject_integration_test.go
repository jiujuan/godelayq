package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/jiujuan/godelayq/internal/store/jobs"
	"github.com/jiujuan/godelayq/internal/store/observe"
	"github.com/stretchr/testify/require"
)

// faultinject_integration_test.go 是 TASK-RE08 的跨库集成：任务库（出箱）与
// 观测库（job_events）两份真实 sqlite，走完"调度器同事务落出箱 → 中继器投递 →
// EventLog 落库去重 → 崩溃重放不产生重复行"的完整链路。RE07 的单库用例各管一段，
// 这里证明接起来仍然成立。

// outboxE2EEnv 汇集跨库集成的装配产物，按关停顺序收尾（relay → EventLog → 库）。
type outboxE2EEnv struct {
	scheduler *core.Scheduler
	jobsDB    *jobs.Store
	obsDB     *observe.Handle
	eventLog  *observe.EventLog
	relay     *jobs.OutboxRelay
}

func newOutboxE2EEnv(t *testing.T) *outboxE2EEnv {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(testWriterFor(t), nil))

	jobsDB, err := jobs.Open(core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}, 0, 0, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = jobsDB.Close() })

	obsCfg := core.DefaultConfig().Observability
	obsCfg.Enabled = true
	obsCfg.Path = filepath.Join(t.TempDir(), "observe.sqlite")
	obsDB, err := observe.Open(obsCfg, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = obsDB.Close() })

	bus := core.NewEventBus(100)
	eventLog, err := observe.NewEventLog(bus, obsDB, observe.EventLogOptions{}, logger)
	require.NoError(t, err)

	scheduler := core.NewScheduler(jobsDB, nil, bus,
		core.WithLogger(logger),
		core.WithEventOutbox(jobsDB),
	)

	relay, err := jobsDB.NewOutboxRelay(bus, jobs.OutboxRelayOptions{}, logger)
	require.NoError(t, err)
	// 关停顺序即硬约束的演练：relay → EventLog → 两个库（defer 逆序）
	t.Cleanup(func() {
		_ = relay.Stop()
		_ = eventLog.Close()
	})

	return &outboxE2EEnv{scheduler: scheduler, jobsDB: jobsDB, obsDB: obsDB, eventLog: eventLog, relay: relay}
}

func TestOutboxDeliversToEventLogEndToEnd(t *testing.T) {
	env := newOutboxE2EEnv(t)
	env.scheduler.RegisterHandler("quick", func(ctx context.Context, job *core.Job) error { return nil })

	env.relay.Start()
	env.scheduler.Start()

	require.NoError(t, env.scheduler.Schedule(&core.Job{ID: "e2e-job", Name: "quick", Type: "quick", TriggerAt: time.Now()}))

	// 任务跑到终态（outbox 模式下快照在任务库）
	require.Eventually(t, func() bool {
		snap, found, err := env.jobsDB.Get("e2e-job")
		return err == nil && found && core.JobStatus(snap.Status) == core.StatusSuccess
	}, 5*time.Second, 10*time.Millisecond, "任务应跑到 success")

	// 中继器投递 + EventLog 落库：三种事件各一行，都带出箱序号。
	// EventLog 的默认批量周期是 200ms，用 Eventually 等转发协程入队 + 落盘。
	require.Eventually(t, func() bool {
		events, err := env.eventLog.Events("e2e-job", 50)
		return err == nil && len(events) == 3
	}, 5*time.Second, 20*time.Millisecond, "scheduled/started/completed 应各落一行")
	events, err := env.eventLog.Events("e2e-job", 50)
	require.NoError(t, err)
	for _, event := range events {
		require.Positive(t, event.OutboxSeq, "经中继器投递的事件必须带出箱序号")
	}

	// 崩溃重放面：把 dispatched 全部改回 pending（投递成功、标记前崩溃的现场），
	// 中继器重发同一批——EventLog 按出箱序号去重，行数不变。
	require.NoError(t, markOutboxPending(env.jobsDB))
	require.Eventually(t, func() bool {
		return outboxPendingCount(t, env.jobsDB) == 0
	}, 5*time.Second, 10*time.Millisecond, "重放批次应被再次投递并标记")

	require.Eventually(t, func() bool {
		events, err := env.eventLog.Events("e2e-job", 50)
		return err == nil && len(events) == 3
	}, 5*time.Second, 20*time.Millisecond, "重放不得产生重复行（读侧去重）")
	events, err = env.eventLog.Events("e2e-job", 50)
	require.NoError(t, err)
	for _, event := range events {
		require.Positive(t, event.OutboxSeq)
	}
}

// markOutboxPending 把全部 dispatched 出箱行改回 pending（模拟崩溃重放现场）。
func markOutboxPending(db *jobs.Store) error {
	_, err := db.SQL().Exec(`UPDATE job_outbox SET status = 'pending', dispatched_at = NULL WHERE status = 'dispatched'`)
	return err
}

func outboxPendingCount(t *testing.T, db *jobs.Store) int64 {
	t.Helper()
	count, err := db.SQL().Query(`SELECT COUNT(*) FROM job_outbox WHERE status = 'pending'`)
	require.NoError(t, err)
	defer count.Close()
	require.True(t, count.Next())
	var n int64
	require.NoError(t, count.Scan(&n))
	return n
}

// testWriterFor 是集成测试的日志汇（丢弃输出，保持用例输出干净）。
func testWriterFor(t *testing.T) ioWriterStub {
	t.Helper()
	return ioWriterStub{}
}

type ioWriterStub struct{}

func (ioWriterStub) Write(p []byte) (int, error) { return len(p), nil }
