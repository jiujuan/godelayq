package observe

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/jiujuan/godelayq/internal/store/pgtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件把三个写入器的关键路径断言挂到真实 PostgreSQL 上（B06）。
// 既有 sqlite 测试文件保持原样作为 SQLite 套件；这里的场景与 sqlite 侧同构：
// 写入往返、seq 定序、保留淘汰、过滤分页、对账幂等。未配置 GODELAYQ_TEST_PG_DSN
// 时整个文件干净跳过。

// openPostgresHandle 在一次性 PG 测试库里开一个观测库句柄。
func openPostgresHandle(t *testing.T) *Handle {
	t.Helper()
	cfg := openConfig("") // path 仅 sqlite 方言使用，PG 走 dsn
	cfg.Dialect = "postgres"
	cfg.DSN = pgtest.Database(t, "observe")
	db, err := Open(cfg, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newEventLogOn / newAuditLogOn 是两个写入器的句柄注入版构造器：
// 与 sqlite 侧的 newTestEventLog/newTestAuditLog 同参同语义，只是不自己开库。
func newEventLogOn(t *testing.T, db *Handle) (*EventLog, *core.EventBus) {
	t.Helper()
	bus := core.NewEventBus(100)
	log, err := NewEventLog(bus, db, EventLogOptions{
		FlushInterval: time.Hour,
		QueueCapacity: 4096,
		Now:           func() time.Time { return fixedTime },
	}, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	return log, bus
}

func TestPostgres_EventLogLifecycle(t *testing.T) {
	db := openPostgresHandle(t)
	log, bus := newEventLogOn(t, db)

	// 八种类型各一条，且同一微秒内连发两条：seq 定序是因果链的根基。
	types := []struct {
		eventType core.EventType
		status    core.JobStatus
	}{
		{core.EventJobScheduled, core.StatusPending},
		{core.EventJobStarted, core.StatusRunning},
		{core.EventJobCompleted, core.StatusSuccess},
		{core.EventJobFailed, core.StatusFailed},
		{core.EventJobCancelled, core.StatusCancelled},
		{core.EventJobRetrying, core.StatusPending},
		{core.EventJobPaused, core.StatusPaused},
		{core.EventJobResumed, core.StatusPending},
	}
	for i, item := range types {
		bus.Publish(core.Event{
			Type:      item.eventType,
			JobID:     fmt.Sprintf("job-%d", i),
			JobName:   "demo",
			Status:    item.status,
			Timestamp: fixedTime.Add(time.Duration(i) * time.Second),
			Data:      []byte(`{"note":"raw"}`),
		})
	}
	waitQueued(t, log, len(types))
	require.NoError(t, log.Flush())

	count, err := log.Count()
	require.NoError(t, err)
	assert.Equal(t, int64(len(types)), count)

	// 库内行序 = 写入序（IDENTITY 自增给出单调 seq）
	got := queryEvents(t, db)
	require.Len(t, got, len(types))
	for i, item := range types {
		assert.Equal(t, string(item.eventType), got[i].Type, "row %d", i)
		assert.Equal(t, int(item.status), got[i].Status, "row %d", i)
	}
	// data 列原文往返
	assert.Equal(t, `{"note":"raw"}`, got[0].Data.String)
	assert.True(t, got[0].Data.Valid)

	// 读端点：按任务过滤 + 降序读回再反转为升序
	items, err := log.Events("job-3", 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, core.EventJobFailed, items[0].Type)

	recent, err := log.Recent(3)
	require.NoError(t, err)
	require.Len(t, recent, 3)
	// Recent 降序取最后三条再反转：升序且是最后三条
	assert.Equal(t, []string{"job-5", "job-6", "job-7"},
		[]string{recent[0].JobID, recent[1].JobID, recent[2].JobID})
}

func TestPostgres_EventLogRetentionPrunesNextBatch(t *testing.T) {
	db := openPostgresHandle(t)
	log, bus := newEventLogOn(t, db)

	log.SetRetention(3, 0) // 下一个批量周期起按条数淘汰
	for i := 0; i < 8; i++ {
		bus.Publish(core.Event{
			Type:      core.EventJobScheduled,
			JobID:     fmt.Sprintf("job-%d", i),
			Timestamp: fixedTime,
		})
	}
	waitQueued(t, log, 8)
	require.NoError(t, log.Flush())

	count, err := log.Count()
	require.NoError(t, err)
	assert.Equal(t, int64(3), count)

	// 淘汰留的是最后三条：读回来的 jobID 应是 5/6/7
	items, err := log.Recent(10)
	require.NoError(t, err)
	var ids []string
	for _, item := range items {
		ids = append(ids, item.JobID)
	}
	assert.Equal(t, []string{"job-5", "job-6", "job-7"}, ids)
}

func TestPostgres_AuditLogLifecycle(t *testing.T) {
	db := openPostgresHandle(t)
	bus := struct{}{} // 占位避免误用；台账不订阅总线
	_ = bus

	opts := AuditLogOptions{
		FlushInterval: time.Hour,
		QueueCapacity: 4096,
		Now:           func() time.Time { return fixedTime },
	}
	log, err := NewAuditLog(db, opts, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })

	// 三个动作、两个 actor：过滤与分页的素材
	for i := 0; i < 5; i++ {
		entry := auditEntry(i + 1)
		entry.Actor = "alice"
		if i%2 == 1 {
			entry.Actor = "bob"
		}
		require.NoError(t, log.Append(entry))
	}
	mustFlush(t, log)

	count, err := log.Count()
	require.NoError(t, err)
	assert.Equal(t, int64(5), count)

	// 无过滤全读：seq 降序（最新在前），total 是不带分页的匹配数
	items, total := mustQuery(t, log, core.AuditFilter{})
	assert.Equal(t, 5, total)
	require.Len(t, items, 5)
	// auditEntry 用 Time 拼序号：最新一行是 seq=5 那条
	assert.Equal(t, auditFixedTime.Add(5*time.Second).UnixMicro(), items[0].Time.UnixMicro(),
		"seq 降序：最新在前")

	// 过滤：actor 精确匹配
	byAlice, total := mustQuery(t, log, core.AuditFilter{Actor: "alice"})
	assert.Equal(t, 3, total)
	require.Len(t, byAlice, 3)
	for _, item := range byAlice {
		assert.Equal(t, "alice", item.Actor)
	}

	// 分页：limit 2 + offset 2，不重不漏
	page, total := mustQuery(t, log, core.AuditFilter{Limit: 2, Offset: 2})
	assert.Equal(t, 5, total, "total 不受分页影响")
	require.Len(t, page, 2)

	// 空结果返回空列表而不是 nil（端点序列化成 []）
	none, total := mustQuery(t, log, core.AuditFilter{Actor: "carol"})
	assert.Equal(t, 0, total)
	assert.NotNil(t, none)
	assert.Empty(t, none)
}

func TestPostgres_ArtifactIndexLifecycle(t *testing.T) {
	db := openPostgresHandle(t)
	root := t.TempDir()
	idx, err := NewArtifactIndex(db, ArtifactIndexOptions{RootDir: root, Now: func() time.Time { return fixedTime }}, testLogger())
	require.NoError(t, err)

	record := func(jobID string, attempt int, outBytes int64, truncated bool) core.ArtifactIndexRecord {
		return core.ArtifactIndexRecord{
			JobID:   jobID,
			Attempt: attempt,
			Kind:    "script",
			Profile: "hello",
			Info: core.ArtifactInfo{
				JobID:     jobID,
				Attempt:   attempt,
				OutPath:   filepath.Join(root, jobID, fmt.Sprintf("a%d.out", attempt)),
				ErrPath:   filepath.Join(root, jobID, fmt.Sprintf("a%d.err", attempt)),
				OutBytes:  outBytes,
				ErrBytes:  512,
				Truncated: truncated,
			},
		}
	}
	mustRecord := func(rec core.ArtifactIndexRecord) {
		t.Helper()
		require.NoError(t, idx.Record(rec))
	}

	// 两次尝试乱序写入，List 按 attempt 升序
	mustRecord(record("job-1", 2, 900, false))
	mustRecord(record("job-1", 1, 42, true))
	got, err := idx.List("job-1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, 1, got[0].Attempt)
	assert.Equal(t, 2, got[1].Attempt)
	assert.Equal(t, "job-1/a1.out", got[0].OutRel, "相对写法入库")
	assert.True(t, got[0].Info.Truncated)
	assert.False(t, got[0].Info.Truncated == false && got[1].Info.Truncated)

	// Exists 与 upsert 覆盖（同一次尝试二次登记：字节数与 truncated 更新）
	exists, err := idx.Exists("job-1", 1)
	require.NoError(t, err)
	assert.True(t, exists)
	exists, err = idx.Exists("job-1", 9)
	require.NoError(t, err)
	assert.False(t, exists)

	mustRecord(record("job-1", 1, 777, false))
	got, err = idx.List("job-1")
	require.NoError(t, err)
	require.Len(t, got, 2, "同 (job_id, attempt) 是覆盖不是新增")
	assert.Equal(t, int64(777), got[0].Info.OutBytes)

	// MarkPurged 只标这一次尝试；重复标注幂等（RowsAffected 为 0）
	require.NoError(t, idx.MarkPurged("job-1", 1))
	got, err = idx.List("job-1")
	require.NoError(t, err)
	assert.Equal(t, core.ArtifactPurged, got[0].State)
	assert.Equal(t, core.ArtifactAvailable, got[1].State)

	affected, err := idx.markPurged("job-1", nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected, "第一行已是 purged，只有第二行计数")
	affected, err = idx.markPurged("job-1", nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "重复标注不改动也不计数")

	// DeleteByJob 删干净
	require.NoError(t, idx.DeleteByJob("job-1"))
	got, err = idx.List("job-1")
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	// ReconcileMissing：目录不在的 available 行标成 purged，目录在的不动
	mustRecord(record("job-gone", 1, 10, false))
	mustRecord(record("job-here", 1, 10, false))
	marked, err := idx.ReconcileMissing(func(jobID string) bool { return jobID == "job-here" })
	require.NoError(t, err)
	assert.Equal(t, 1, marked)
	gone, err := idx.List("job-gone")
	require.NoError(t, err)
	assert.Equal(t, core.ArtifactPurged, gone[0].State)
	here, err := idx.List("job-here")
	require.NoError(t, err)
	assert.Equal(t, core.ArtifactAvailable, here[0].State)
}
