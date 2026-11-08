package mgr

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
	"github.com/jiujuan/godelayq/internal/store/pgtest"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// SQL 快照门禁（db-quality-gates-design.md §5），与 jobs/sql_snapshot_test.go 同构。
// 重点钉住两样 mgr 特有的东西：lower(name) 的查询文本与 upsert 冲突目标的方言分支
// （SQLite `(name)` / PG `(lower(name))`）。
//
// 重写 golden：go test ./internal/store/mgr/ -run TestSQLSnapshot -update

var updateSnapshot = flag.Bool("update", false, "重写 SQL 快照 golden 文件")

// snapshotHash 是一条固定的 cost-10 bcrypt 哈希（口令 "mgr-test-password"）。
// 快照专用：testHash 每个测试进程生成一次、bcrypt 带随机盐，写进 golden 会
// 逐次不同。取值本身不承载语义——它只要求 shape 合法且过 cost 10-15 校验。
const snapshotHash = "$2a$10$CeOkGdzSwX6zs7hU5PdbFedCkhjXBfBi3vfwjwOtEz3XgKh4sbf8."

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
		path := filepath.Join(t.TempDir(), "mgr.sqlite")
		runSQLSnapshot(t, openWithFactory(func(*testing.T) core.MgrStoreConfig {
			return core.MgrStoreConfig{Enabled: true, Path: path}
		})(t), "sqlite")
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := pgtest.Database(t, "snap_mgr")
		runSQLSnapshot(t, openWithFactory(func(*testing.T) core.MgrStoreConfig {
			return core.MgrStoreConfig{Enabled: true, Dialect: "postgres", DSN: dsn}
		})(t), "postgres")
	})
}

func runSQLSnapshot(t *testing.T, h *Handle, dialect string) {
	t.Helper()
	rec := &sqlRecorder{}
	h.AddQueryHook(rec)

	// 一次性导入：metaGet → Count → Save（新插）→ metaSetNow；重跑 → metaGet 短路。
	require.NoError(t, ImportOnce(h, ImportSource{AuthUsers: []core.UserConfig{
		{Name: "admin", PasswordBcrypt: snapshotHash, Role: "admin"},
	}}, nil))
	require.NoError(t, ImportOnce(h, ImportSource{AuthUsers: []core.UserConfig{
		{Name: "admin", PasswordBcrypt: snapshotHash, Role: "admin"},
	}}, nil))

	users := NewUserStore(h)
	require.NoError(t, users.Save(core.UserRecord{Name: "alice", PasswordBcrypt: snapshotHash, Role: "operator"}))
	// 大小写变体 upsert：冲突目标按方言（本快照的存在意义之一）。
	require.NoError(t, users.Save(core.UserRecord{Name: "ALICE", PasswordBcrypt: snapshotHash, Role: "admin"}))
	_, _, err := users.Get("alice")
	require.NoError(t, err)
	_, _, err = users.Get("ALICE")
	require.NoError(t, err)
	_, err = users.List()
	require.NoError(t, err)
	_, err = users.Count()
	require.NoError(t, err)
	require.NoError(t, users.Delete("alice"))
	// 缺失路径：同文本再走一次，接口契约是返回 ErrUserNotFound（语句照常捕获）。
	require.ErrorIs(t, users.Delete("alice"), core.ErrUserNotFound)

	groups := NewGroupStore(h)
	require.NoError(t, groups.Save(core.Group{Name: "prod", Description: "生产环境", Color: "#ff0000"}))
	require.NoError(t, groups.Save(core.Group{Name: "prod", Description: "生产", Color: "#00ff00"}))
	_, _, err = groups.Get("PROD")
	require.NoError(t, err)
	_, err = groups.List()
	require.NoError(t, err)
	require.NoError(t, groups.Delete("prod"))

	profiles := NewExecutorProfileStore(h)
	// CreatedAt/UpdatedAt 都给固定值：UpdatedAt 缺省会被 Save 折成 time.Now，
	// record_json 进了 golden 就逐次不同了。
	fixedTime := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	require.NoError(t, profiles.Save(core.ExecutorProfileRecord{
		Name: "demo", Kind: "script", Runtime: "bash", Script: "echo hi",
		CreatedAt: fixedTime,
		UpdatedAt: fixedTime,
	}))
	_, _, err = profiles.Get("DEMO")
	require.NoError(t, err)
	_, err = profiles.List()
	require.NoError(t, err)
	require.NoError(t, profiles.Delete("demo"))

	require.NotEmpty(t, rec.queries, "捕获不到语句：hook 是否挂上了？")
	compareGolden(t, dialect, strings.Join(rec.queries, ";\n"))
}

// compareGolden 与 jobs 包同构；三包各持一份是快照测试自包含的代价。
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
