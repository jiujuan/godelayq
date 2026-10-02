package sqlite

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"godelayq/core"
)

// 本文件的用例证明的是行为：改完保留策略之后，下一个批量周期的淘汰用了新值。
// 落盘由用例显式 Flush 触发（newTest* 把周期定成一小时），所以"下一个周期"是确定的，
// 等待一律等队列条数而不是 sleep（与 events_test.go 的 waitQueued 同一口径）。

// waitAuditQueued 等台账写入器队列里的条数达到 n，形状与事件侧的 waitQueued 相同。
// 台账的 Append 是同步入队（没有事件那条转发协程），所以这个条件几乎立刻成立；
// 仍然等条件而不是直接断言，是为了不让用例依赖"入队一定同步"这个实现细节。
func waitAuditQueued(t *testing.T, log *AuditLog, n int) {
	t.Helper()

	deadline := time.After(2 * time.Second)
	for log.batch.queued() != n {
		select {
		case <-deadline:
			t.Fatalf("queue did not reach %d audit rows in time, got %d", n, log.batch.queued())
		case <-time.After(200 * time.Microsecond):
		}
	}
}

// TestEventLog_SetRetentionPrunesNextBatch 条数上限换小之后，下一批的淘汰切在新值上。
func TestEventLog_SetRetentionPrunesNextBatch(t *testing.T) {
	log, _, bus := newTestEventLog(t, func(opts *EventLogOptions) {
		opts.RetentionCount = 1000
	})

	// 六条带 job_id 的事件（总线缓冲是 100，这里六条不会丢）
	for i := 0; i < 6; i++ {
		bus.Publish(core.Event{Type: core.EventJobCompleted,
			JobID: fmt.Sprintf("job-%d", i), JobName: "task"})
	}
	waitQueued(t, log, 6)
	require.NoError(t, log.Flush())

	count, err := log.Count()
	require.NoError(t, err)
	require.EqualValues(t, 6, count, "six events must be stored before the retention change")

	log.SetRetention(2, 0)
	bus.Publish(core.Event{Type: core.EventJobCompleted, JobID: "job-6", JobName: "task"})
	waitQueued(t, log, 1)
	require.NoError(t, log.Flush())

	count, err = log.Count()
	require.NoError(t, err)
	assert.EqualValues(t, 2, count, "the next batch's prune must use the new count")
}

// TestEventLog_SetRetentionZeroFallsBackToDefault 0 不是"不限量"，而是"回到
// DefaultObserveEventRetentionCount"——这条口径与 NewEventLog 里的补齐同一条，
// 否则 setter 就成了第二套规则。age<0 同样按 0（不按时间淘汰）处理。
func TestEventLog_SetRetentionZeroFallsBackToDefault(t *testing.T) {
	log, _, bus := newTestEventLog(t, func(opts *EventLogOptions) {
		opts.RetentionCount = 3
	})

	log.SetRetention(0, -time.Hour)
	for i := 0; i < 5; i++ {
		bus.Publish(core.Event{Type: core.EventJobCompleted,
			JobID: fmt.Sprintf("job-%d", i), JobName: "task"})
	}
	waitQueued(t, log, 5)
	require.NoError(t, log.Flush())

	count, err := log.Count()
	require.NoError(t, err)
	assert.EqualValues(t, 5, count, "count<=0 falls back to the default (200000), age<0 to 0")
}

// TestEventLog_SetRetentionConcurrentWithPrune 写策略与读策略交错：
// 后台落盘协程每轮 prune 都读这两个字段，重载协程随时可能写它们。
// 周期定成一毫秒就是为了让淘汰真的在后台跑，而不只是被用例的 Flush 触发。
func TestEventLog_SetRetentionConcurrentWithPrune(t *testing.T) {
	log, _, bus := newTestEventLog(t, func(opts *EventLogOptions) {
		opts.FlushInterval = time.Millisecond
		opts.RetentionCount = 1000
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 200; i++ {
			log.SetRetention(1+i%3, 0)
		}
	}()

	for i := 0; i < 200; i++ {
		bus.Publish(core.Event{Type: core.EventJobCompleted,
			JobID: fmt.Sprintf("job-%d", i), JobName: "task"})
	}
	wg.Wait()

	// 判据是 -race 干净加上"淘汰值半途变化也不会让行数越过最后一次设定的上界"：
	// 这里显式收到 3 条再落一次盘，行数就不能再大于 3。
	log.SetRetention(3, 0)
	require.NoError(t, log.Flush())

	count, err := log.Count()
	require.NoError(t, err)
	assert.LessOrEqual(t, count, int64(3), "prune must converge on the retention written last")
}

// TestAuditLog_SetRetentionPrunesNextBatch 台账侧同构成：下一批的淘汰用新的条数。
func TestAuditLog_SetRetentionPrunesNextBatch(t *testing.T) {
	log, _ := newTestAuditLog(t, func(opts *AuditLogOptions) {
		opts.RetentionCount = 1000
	})

	for i := 0; i < 6; i++ {
		require.NoError(t, log.Append(auditEntry(i)))
	}
	waitAuditQueued(t, log, 6)
	require.NoError(t, log.Flush())

	count, err := log.Count()
	require.NoError(t, err)
	require.EqualValues(t, 6, count)

	log.SetRetention(2, 0)
	require.NoError(t, log.Append(auditEntry(6)))
	waitAuditQueued(t, log, 1)
	require.NoError(t, log.Flush())

	count, err = log.Count()
	require.NoError(t, err)
	assert.EqualValues(t, 2, count, "the next batch's prune must use the new count")
}

// TestAuditLog_SetRetentionZeroFallsBackToDefault 台账侧的 0 与负时长走同一条补齐：
// 回到 DefaultObserveAuditRetentionCount、不按时间淘汰，与 NewAuditLog 一致。
func TestAuditLog_SetRetentionZeroFallsBackToDefault(t *testing.T) {
	log, _ := newTestAuditLog(t, func(opts *AuditLogOptions) {
		opts.RetentionCount = 3
	})

	log.SetRetention(0, -time.Hour)
	for i := 0; i < 5; i++ {
		require.NoError(t, log.Append(auditEntry(i)))
	}
	waitAuditQueued(t, log, 5)
	require.NoError(t, log.Flush())

	count, err := log.Count()
	require.NoError(t, err)
	assert.EqualValues(t, 5, count, "count<=0 falls back to the default, age<0 to 0")
}
