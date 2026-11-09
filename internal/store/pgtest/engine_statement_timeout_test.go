package pgtest_test

// 语句超时（G6/Q07）——engine 对真 PG 的用例。放在本包而不是 engine：
// pgtest 依赖 engine（engine 的包守卫禁止 engine 目录下任何文件引仓内包，
// 测试文件也算），而"真库验证"本来就是本包的职责。
// 未配 GODELAYQ_TEST_PG_DSN 时全部干净跳过。

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/store/engine"
	"github.com/jiujuan/godelayq/internal/store/pgtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureHandler 是本包内的 slog 捕获器（engine 的同名类型不导出）。
type stCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *stCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *stCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *stCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *stCapture) WithGroup(string) slog.Handler      { return h }

func (h *stCapture) countMsg(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if strings.Contains(r.Message, substr) {
			n++
		}
	}
	return n
}

func openPG(t *testing.T, timeout time.Duration, logger *slog.Logger) *engine.DB {
	t.Helper()
	db, err := engine.Open(engine.Options{
		Dialect:          engine.DialectPostgres,
		DSN:              pgtest.Database(t, "stmt_to"),
		StatementTimeout: timeout,
	}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// 真超时：业务连接的语句超过上限被服务端取消。
// 断言 sqlstate 57014 而不是报错文本——服务端消息随 locale 走（本机 PG 是中文
// "由于语句执行超时"，CI 是英文），sqlstate 是 pgx 拼进错误串的稳定部分。
func TestStatementTimeout_KillsLongQuery(t *testing.T) {
	db := openPG(t, 100*time.Millisecond, slog.Default())

	_, err := db.NewRaw(`SELECT pg_sleep(2)`).Exec(context.Background())
	require.Error(t, err, "2 秒的语句在 100ms 上限内必须被取消")
	assert.Contains(t, err.Error(), "57014", "必须是 statement_timeout 取消（sqlstate 57014）")
}

// 迁移豁免：同一个 100ms 上限，迁移事务里的 pg_sleep(2) 照常完成——
// SET LOCAL 只豁免本事务，业务上限不背启动期长 DDL 的锅。
func TestStatementTimeout_MigrationExempt(t *testing.T) {
	db := openPG(t, 100*time.Millisecond, slog.Default())

	err := db.Migrate("st_schema_migrations", []engine.Migration{
		{Version: 1, Up: []string{`SELECT pg_sleep(2)`}},
	})
	require.NoError(t, err, "迁移事务内的语句被 SET LOCAL 豁免")

	v, err := db.MigrationVersion("st_schema_migrations")
	require.NoError(t, err)
	assert.Equal(t, 1, v)
}

// 缺省（0）：启动日志恰好一条 Warn，把"没有语句上限"说在明处。
func TestStatementTimeout_WarnWhenUnset(t *testing.T) {
	handler := &stCapture{}
	_ = openPG(t, 0, slog.New(handler))

	assert.Equal(t, 1, handler.countMsg("no statement_timeout"), "未配置时恰一条 Warn")

	// 配置后不再提示：同一个捕获器换一个带上限的库，Warn 消失。
	handler2 := &stCapture{}
	_ = openPG(t, 100*time.Millisecond, slog.New(handler2))
	assert.Equal(t, 0, handler2.countMsg("no statement_timeout"),
		"配置了上限就不该再 Warn")
}
