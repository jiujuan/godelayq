package engine

// 语句超时（G6/Q07）——DSN 注入与合并语义的纯函数用例（不碰真库）。
// "真的会杀 / 迁移豁免 / 缺省 Warn"走真 PG，在 statement_timeout_pg_test.go
//（外部测试包：pgtest 依赖本包，包内测试引它成环）。

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// URL 形态：statement_timeout 注入查询串（pgx 把未知参数当运行时参数逐会话应用）。
func TestWithStatementTimeout_URLForm(t *testing.T) {
	got, err := withStatementTimeout("postgres://u:p@localhost:5432/db?sslmode=disable", 5*time.Second)
	require.NoError(t, err)
	assert.Contains(t, got, "statement_timeout=5000")
	assert.Contains(t, got, "sslmode=disable", "既有参数不得丢失")
	assert.True(t, strings.HasPrefix(got, "postgres://u:p@localhost:5432/db?"), "URL 形态保持可解析")
}

// keyword/value 形态：追加为空格分隔的键值对（pgx 同样接受）。
func TestWithStatementTimeout_KeywordForm(t *testing.T) {
	got, err := withStatementTimeout("host=localhost user=u password=p dbname=db", 100*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(got, "statement_timeout=100"), got)
	assert.Contains(t, got, "host=localhost", "既有参数不得丢失")
}

// 合并语义（卡内定）：DSN 已带同名参数时报错，不猜取哪一边。
func TestWithStatementTimeout_ConflictIsRejected(t *testing.T) {
	_, err := withStatementTimeout("postgres://u:p@h/db?statement_timeout=999", time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already sets statement_timeout")

	_, err = withStatementTimeout("host=localhost statement_timeout=999 dbname=db", time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already sets statement_timeout")
}

// 亚毫秒上限等价于"每条语句都失败"，直接拒绝而不是注入一个必然超时的值。
func TestWithStatementTimeout_TooSmallRejected(t *testing.T) {
	_, err := withStatementTimeout("postgres://u:p@h/db", 500*time.Microsecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too small")
}

// sqlite 方言上配置了 PG 专属键：不拒绝（机制层不做配置校验），但要能看见。
func TestStatementTimeout_WarnedOnSQLite(t *testing.T) {
	handler := &captureHandler{}
	db, err := Open(Options{
		Path:             t.TempDir() + "/st.sqlite",
		StatementTimeout: time.Second,
	}, slog.New(handler))
	require.NoError(t, err)
	defer db.Close()

	found := handler.countMsg("statement_timeout is postgres-only")
	assert.True(t, found > 0, "sqlite 上写 PG 专属键要有 Warn 留痕")
}
