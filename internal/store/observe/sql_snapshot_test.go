package observe

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// SQL 快照门禁（db-quality-gates-design.md §5），与 jobs/mgr 的快照测试同构。
// 观测库钉住的是批量写形态（多值 INSERT）、两条 prune 的子查询文本、audit 动态
// 过滤与分页、artifact 的复合键 upsert 与对账查询。
//
// 重写 golden：go test ./internal/store/observe/ -run TestSQLSnapshot -update

var updateSnapshot = flag.Bool("update", false, "重写 SQL 快照 golden 文件")

type sqlRecorder struct {
	queries []string
}

// 两个掩码让 golden 在"生产代码自己产生数据值"的前提下仍然稳定：
//   - epochDigits：纪元量级数字（>=10 位）——markImported/metaSetNow 的写入时刻、
//     19 位纳秒时间戳参数，来自生产代码的 time.Now，逐次不同；
//   - rfc3339：record_json/snapshot_json 载荷里的 RFC3339 时间戳——Save 无条件
//     用 time.Now 覆盖 UpdatedAt（生产语义），载荷必然带墙钟。
//
// 快照钉的是 SQL 形状（表/列/冲突目标/别名/多值形态/载荷字段结构），不是数据值；
// 字段名与键序变了照样会红。
var (
	epochDigits = regexp.MustCompile(`\d{10,}`)
	rfc3339     = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
)

func (r *sqlRecorder) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	text := event.Query
	text = rfc3339.ReplaceAllString(text, "<ts>")
	text = epochDigits.ReplaceAllString(text, "<n>")
	r.queries = append(r.queries, text)
	return ctx
}

func (r *sqlRecorder) AfterQuery(context.Context, *bun.QueryEvent) {}

func TestSQLSnapshot(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		runSQLSnapshot(t, db, "sqlite")
	})
	t.Run("postgres", func(t *testing.T) {
		runSQLSnapshot(t, openPostgresHandle(t), "postgres")
	})
}

func runSQLSnapshot(t *testing.T, db *Handle, dialect string) {
	t.Helper()
	rec := &sqlRecorder{}
	db.AddQueryHook(rec)

	// 事件写入器：两条（一条带 data+metadata、一条不带）→ 多值插入 + 两条 prune，
	// 再走三条读路径与计数。落盘经显式 Flush 同步，语句顺序确定。
	log, bus := newEventLogOn(t, db)
	bus.Publish(core.Event{Type: core.EventJobScheduled, JobID: "job-0", JobName: "demo",
		Status: core.StatusPending, Timestamp: fixedTime, Data: []byte(`{"k":1}`),
		Metadata: map[string]interface{}{"m": "v"}})
	bus.Publish(core.Event{Type: core.EventJobStarted, JobID: "job-1", JobName: "demo",
		Status: core.StatusRunning, Timestamp: fixedTime})
	waitQueued(t, log, 2)
	require.NoError(t, log.Flush())
	_, err := log.Events("job-0", 10)
	require.NoError(t, err)
	_, err = log.Recent(5)
	require.NoError(t, err)
	_, err = log.Count()
	require.NoError(t, err)

	// 台账：三条 → 插入 + prune；查询三形态（无过滤 / actor+分页 / 多条件+时间区间）。
	audit, err := NewAuditLog(db, AuditLogOptions{
		FlushInterval: time.Hour,
		QueueCapacity: 4096,
		Now:           func() time.Time { return auditFixedTime },
	}, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = audit.Close() })
	for i := 1; i <= 3; i++ {
		require.NoError(t, audit.Append(auditEntry(i)))
	}
	mustFlush(t, audit)
	_, _, err = audit.Query(core.AuditFilter{})
	require.NoError(t, err)
	_, _, err = audit.Query(core.AuditFilter{Actor: "operator01", Limit: 2, Offset: 1})
	require.NoError(t, err)
	since := auditFixedTime.Add(-time.Hour)
	until := auditFixedTime
	_, _, err = audit.Query(core.AuditFilter{Action: "job.create", Verdict: "ok", Since: &since, Until: &until})
	require.NoError(t, err)
	_, err = audit.Count()
	require.NoError(t, err)

	// 工件：复合键 upsert（新插 + 覆盖同文本）、列表、存在性、标注（幂等重标）、
	// 清理删除、对账（DISTINCT + 标注）。
	idx, err := NewArtifactIndex(db, ArtifactIndexOptions{
		RootDir: t.TempDir(),
		Now:     func() time.Time { return fixedTime },
	}, testLogger())
	require.NoError(t, err)
	record := func(jobID string, attempt int, outBytes int64) core.ArtifactIndexRecord {
		return core.ArtifactIndexRecord{
			JobID: jobID, Attempt: attempt, Kind: "script", Profile: "hello",
			Info: core.ArtifactInfo{
				JobID: jobID, Attempt: attempt,
				OutPath:   filepath.Join(idx.rootDir, jobID, "a.out"),
				ErrPath:   filepath.Join(idx.rootDir, jobID, "a.err"),
				OutBytes:  outBytes,
				ErrBytes:  512,
				Truncated: attempt == 1,
			},
		}
	}
	require.NoError(t, idx.Record(record("job-1", 1, 100)))
	require.NoError(t, idx.Record(record("job-1", 1, 200)))
	require.NoError(t, idx.Record(record("job-1", 2, 300)))
	_, err = idx.List("job-1")
	require.NoError(t, err)
	_, err = idx.Exists("job-1", 1)
	require.NoError(t, err)
	require.NoError(t, idx.MarkPurged("job-1", 1))
	require.NoError(t, idx.MarkPurged("job-1", 1))
	require.NoError(t, idx.MarkAllPurged("job-1"))
	require.NoError(t, idx.DeleteByJob("job-1"))
	require.NoError(t, idx.Record(record("job-2", 1, 400)))
	marked, err := idx.ReconcileMissing(func(string) bool { return false })
	require.NoError(t, err)
	require.Equal(t, 1, marked)

	require.NotEmpty(t, rec.queries, "捕获不到语句：hook 是否挂上了？")
	compareGolden(t, dialect, strings.Join(rec.queries, ";\n"))
}

// compareGolden 与 jobs/mgr 包同构；三包各持一份是快照测试自包含的代价。
func compareGolden(t *testing.T, dialect, got string) {
	t.Helper()
	golden := filepath.Join("testdata", "sql_snapshot_"+dialect+".golden")
	if *updateSnapshot {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(golden, []byte(got+"\n"), 0o644))
		return
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err, "golden 缺失：先跑 go test -run TestSQLSnapshot -update 生成")
	require.Equal(t, string(want), got+"\n", "SQL 文本与 golden 不一致：升级 bun 或改动了查询？diff 后按预期更新 golden")
}
