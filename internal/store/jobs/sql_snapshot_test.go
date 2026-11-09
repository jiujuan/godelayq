package jobs

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/jiujuan/godelayq/internal/store/pgtest"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// SQL 快照门禁（db-quality-gates-design.md §5）：经 bun QueryHook 捕获生产 API
// 触发的全部语句文本，与 per-dialect golden 比对。行为断言（B06）证明"查出来的
// 数据对"，快照证明"生成的 SQL 没变"——bun 升级或有人改构建器链时，占位符风格、
// EXCLUDED 写法、冲突目标等文本漂移在这里逐行可见。
//
// 重写 golden：go test ./internal/store/jobs/ -run TestSQLSnapshot -update
// （PG 侧需 GODELAYQ_TEST_PG_DSN；未配置时 PG 侧跳过、golden 不动）。

var updateSnapshot = flag.Bool("update", false, "重写 SQL 快照 golden 文件")

// sqlRecorder 是包内私有的语句捕获器：BeforeQuery 阶段 event.Query 已是
// 带方言占位符的完整语句文本。三个 store 包各持一份 12 行的实现是有意的：
// 收进公共包要为一段 struct 造一个依赖点，不值。
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
		runSQLSnapshot(t, openSQLiteStore(t, withHistory(1, 0)), "sqlite")
	})
	t.Run("postgres", func(t *testing.T) {
		cfg := core.StoreSQLiteConfig{Dialect: "postgres", DSN: pgtest.Database(t, "snap_jobs")}
		// 阈值 0：快照 golden 在"慢查询钩子未挂"的默认态生成——
		// 两个钩子并存的文本一致性由 engine 的并存用例钉住。
		s, err := Open(cfg, 1, 0, 0, 0, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		runSQLSnapshot(t, s, "postgres")
	})
}

func runSQLSnapshot(t *testing.T, s *Store, dialect string) {
	t.Helper()
	rec := &sqlRecorder{}
	s.h.AddQueryHook(rec)

	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

	// 一次性导入：空库全链（标记查询 → 计数 → 批量插入 → 标记 upsert）+ 幂等重跑。
	// 单条目 map：迭代顺序确定。
	source := filepath.Join(t.TempDir(), "jobs.json")
	data, err := json.Marshal(map[string]core.JobSnapshot{"imp": snapshot("imp", core.StatusPending, now)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(source, data, 0o644))
	require.NoError(t, ImportOnce(s.h, source))
	require.NoError(t, ImportOnce(s.h, source))

	// 直写主链：新插、同 ID 覆盖（冲突更新）、Save 入口、全量读、删除。
	require.NoError(t, s.Update(snapshot("j1", core.StatusPending, now)))
	require.NoError(t, s.Update(snapshot("j1", core.StatusRunning, now.Add(time.Second))))
	require.NoError(t, s.Save(&core.Job{ID: "j2", Name: "job-j2", TriggerAt: now, Status: core.StatusPending}))
	loaded, err := s.LoadAll()
	require.NoError(t, err)
	require.NotEmpty(t, loaded)
	require.NoError(t, s.Delete("j2"))

	// 留痕淘汰：留痕上限 1，第二笔终态写入触发 trim 的扫描 + 删除（含账本级联）。
	require.NoError(t, s.Update(snapshot("t1", core.StatusSuccess, now)))
	require.NoError(t, s.Update(snapshot("t2", core.StatusSuccess, now)))

	// 账本链（TASK-RE02）：开始 → 收口（带结论列）→ 重试开始 → 放弃 → 读回 → 裁剪。
	// upsert 的冲突目标、UPDATE 的表达式列、裁剪子查询的形状都靠这段进 golden。
	require.NoError(t, s.BeginAttempt("j1", core.Attempt{
		JobID: "j1", Seq: 1, Status: core.AttemptRunning, Trigger: core.AttemptTriggerScheduled,
		StartedAt: now,
	}))
	require.NoError(t, s.FinishAttempt("j1", 1, core.Attempt{
		JobID: "j1", Seq: 1, Status: core.AttemptFailed, Trigger: core.AttemptTriggerRetry,
		StartedAt: now, FinishedAt: now.Add(time.Second), DurationMs: 1000,
		Class: "timeout", Retryable: true, RetryableSet: true,
		ExitCode: 0, ExitCodeSet: true,
	}))
	require.NoError(t, s.BeginAttempt("j1", core.Attempt{
		JobID: "j1", Seq: 2, Status: core.AttemptRunning, Trigger: core.AttemptTriggerRetry,
		StartedAt: now.Add(2 * time.Second),
	}))
	require.NoError(t, s.AbandonAttempts("j1", 2, now.Add(3*time.Second)))
	attempts, err := s.Attempts("j1", 10)
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	require.NoError(t, s.PruneAttempts("j1", 1))

	// 幂等链（TASK-RE03）：快照直查（SnapshotReader）+ 登记表的查/登（先到先得）/淘汰。
	_, _, _ = s.Get("j1")
	_, _, _, _ = s.LookupIdempotency("key-1")
	require.NoError(t, s.PutIdempotency("key-1", "j1", "fp-1"))
	require.NoError(t, s.PutIdempotency("key-1", "j2", "fp-2"))
	require.NoError(t, s.PruneExpiredIdempotency(time.Hour))

	// 出箱链（TASK-RE07）：UpdateWithEvents 同事务写、AppendOutbox 入队、
	// 中继器的取批/标记/淘汰。总线无订阅者，Publish 按既有契约丢弃。
	bus := core.NewEventBus(10)
	require.NoError(t, s.UpdateWithEvents(snapshot("ob-1", core.StatusPending, now), []core.Event{
		{Type: core.EventJobScheduled, JobID: "ob-1", JobName: "job-ob-1", Status: core.StatusPending},
	}))
	// W1/Q08：running 快照 + 出箱行 + 账本开始行同一事务——合并通道的语句序列
	// 靠这一步进 golden，"两条语句并入一个事务"的 diff 在两方言上逐行可见。
	require.NoError(t, s.UpdateWithEventsAndAttempt(snapshot("ob-2", core.StatusRunning, now), []core.Event{
		{Type: core.EventJobStarted, JobID: "ob-2", JobName: "job-ob-2", Status: core.StatusRunning},
	}, core.Attempt{
		JobID: "ob-2", Seq: 1, Status: core.AttemptRunning, Trigger: core.AttemptTriggerScheduled,
		StartedAt: now,
	}))
	require.NoError(t, s.AppendOutbox([]core.Event{
		{Type: core.EventJobStarted, JobID: "ob-1", JobName: "job-ob-1", Status: core.StatusRunning},
	}))
	relay, err := s.NewOutboxRelay(bus, OutboxRelayOptions{}, nil)
	require.NoError(t, err)
	require.NoError(t, relay.dispatchBatch())
	relay.prune()

	require.NotEmpty(t, rec.queries, "捕获不到语句：hook 是否挂上了？")
	compareGolden(t, dialect, strings.Join(rec.queries, ";\n"))
}

// compareGolden 比对 golden；-update 时重写。
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
