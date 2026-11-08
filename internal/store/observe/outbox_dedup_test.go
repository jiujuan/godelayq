package observe

import (
	"testing"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/require"
)

// outbox_dedup_test.go 钉 TASK-RE07 的读侧去重：中继器按 at-least-once 投递，
// 崩溃重放会把同一事件（同一 outbox_seq）再发一遍；EventLog 的插入按部分唯一
// 索引冲突忽略，job_events 不长重复行。直发事件（NULL 序号）永不冲突。
// SQLite 恒跑；PostgreSQL 与 SQLite 同构断言（矩阵文件的门控方式）。

// dedupScenario 把同一批事件写两遍（模拟"投递成功、标记前崩溃、重放"），
// 返回库内行数：3 条带序号的去重后各留一行，2 条直发行（无序号）各成一行 = 5。
func dedupScenario(t *testing.T, log *EventLog, bus *core.EventBus) int64 {
	t.Helper()

	write := func() {
		for _, event := range []core.Event{
			{Type: core.EventJobScheduled, JobID: "dedup-job", JobName: "dedup", Status: core.StatusPending, OutboxSeq: 101},
			{Type: core.EventJobStarted, JobID: "dedup-job", JobName: "dedup", Status: core.StatusRunning, OutboxSeq: 102},
			{Type: core.EventJobCompleted, JobID: "dedup-job", JobName: "dedup", Status: core.StatusSuccess, OutboxSeq: 103},
			// 直发行：无出箱序号（JSON 部署 + 观测库），与去重无关但必须照常入库
			{Type: core.EventJobPaused, JobID: "dedup-job", JobName: "dedup", Status: core.StatusPaused},
		} {
			bus.Publish(event)
		}
	}

	write()
	waitQueued(t, log, 4)
	require.NoError(t, log.Flush())
	write() // 重放：三条带序号的是重复，直发那条是第二个独立事件
	waitQueued(t, log, 4)
	require.NoError(t, log.Flush())

	count, err := log.Count()
	require.NoError(t, err)
	return count
}

func TestEventLog_DedupsByOutboxSeq_SQLite(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)
	require.EqualValues(t, 5, dedupScenario(t, log, bus), "3 条带序号去重后各一行 + 2 条直发行")
}

func TestEventLog_DedupsByOutboxSeq_Postgres(t *testing.T) {
	db := openPostgresHandle(t)
	log, bus := newEventLogOn(t, db)
	require.EqualValues(t, 5, dedupScenario(t, log, bus))
}

// TestEventLog_ReadsBackOutboxSeq：序号随读端点还原（客户端可识别重放的旧事件）。
func TestEventLog_ReadsBackOutboxSeq(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)

	bus.Publish(core.Event{Type: core.EventJobStarted, JobID: "seq-read", Status: core.StatusRunning, OutboxSeq: 77})
	waitQueued(t, log, 1)
	require.NoError(t, log.Flush())

	events, err := log.Events("seq-read", 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.EqualValues(t, 77, events[0].OutboxSeq)
}
