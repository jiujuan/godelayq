package engine

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// captureHandler 把 slog 记录收进内存，供钩子用例断言。线程安全：
// 生产句柄上的查询可能来自多个协程（任务库单连接，但测试里直接跑）。
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) slowQueryRecords() []slog.Record {
	var out []slog.Record
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == "slow query" {
			out = append(out, r)
		}
	}
	return out
}

// attrOf 取记录里某个字段的值（slog 的 Attr 只能遍历拿）。
func attrOf(r slog.Record, key string) (slog.Value, bool) {
	var found slog.Value
	var ok bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found, ok = a.Value, true
			return false
		}
		return true
	})
	return found, ok
}

// newHookWithCapture 建一个直连捕获器的钩子，供确定性单测用——
// 真跑 SQL 计时受时钟粒度影响（Windows 粗时钟下快速查询 elapsed 可能是 0），
// 钩子自身的判定与日志形状不依赖那次测量，构造 StartTime 即可稳定驱动。
func newHookWithCapture(threshold time.Duration) (*slowQueryHook, *captureHandler) {
	handler := &captureHandler{}
	return &slowQueryHook{
		threshold: threshold,
		maxBytes:  DefaultSlowQueryLogBytes,
		dialect:   DialectSQLite,
		logger:    slog.New(handler),
	}, handler
}

func newEvent(query string, start time.Time, err error) *bun.QueryEvent {
	return &bun.QueryEvent{Query: query, StartTime: start, Err: err}
}

// 阈值之上必记：msg/sql/dialect/op 齐全，op 由 bun 从语句得出。
func TestSlowQueryHook_FiresAboveThreshold(t *testing.T) {
	hook, handler := newHookWithCapture(time.Millisecond)

	hook.AfterQuery(context.Background(),
		newEvent("SELECT 1", time.Now().Add(-5*time.Millisecond), nil))

	records := handler.slowQueryRecords()
	require.Len(t, records, 1)
	r := records[0]
	assert.Equal(t, slog.LevelWarn, r.Level)
	assert.Equal(t, "slow query", r.Message)
	sqlVal, ok := attrOf(r, "sql")
	require.True(t, ok)
	assert.Equal(t, "SELECT 1", sqlVal.String())
	dialectVal, ok := attrOf(r, "dialect")
	require.True(t, ok)
	assert.Equal(t, string(DialectSQLite), dialectVal.String())
	opVal, ok := attrOf(r, "op")
	require.True(t, ok)
	assert.Equal(t, "SELECT", opVal.String())
}

// 阈值之下不记：快速查询不该出现在慢查询日志里。
func TestSlowQueryHook_SilentBelowThreshold(t *testing.T) {
	hook, handler := newHookWithCapture(time.Millisecond)

	hook.AfterQuery(context.Background(), newEvent("SELECT 1", time.Now(), nil))

	assert.Empty(t, handler.slowQueryRecords())
}

// 超长 SQL 截断到上限并在 rune 边界收口，带截断标记。
func TestSlowQueryHook_TruncatesLongSQL(t *testing.T) {
	hook, handler := newHookWithCapture(time.Millisecond)

	long := "SELECT '" + strings.Repeat("x", 3*DefaultSlowQueryLogBytes) + "'"
	hook.AfterQuery(context.Background(), newEvent(long, time.Now().Add(-time.Second), nil))

	records := handler.slowQueryRecords()
	require.Len(t, records, 1)
	sqlVal, ok := attrOf(records[0], "sql")
	require.True(t, ok)
	got := sqlVal.String()
	assert.Contains(t, got, slowQueryTruncatedMark, "超长语句要带截断标记")
	assert.Equal(t, DefaultSlowQueryLogBytes+utf8.RuneLen('…')+len("(truncated)"), len(got),
		"日志里的 SQL 恰好是上限字节 + 标记")
	assert.True(t, utf8.ValidString(got), "截断收口在 rune 边界上")
}

// 失败查询同样记录，并带 error 字段："慢且失败"也是事实。
func TestSlowQueryHook_FailedQueryLogged(t *testing.T) {
	hook, handler := newHookWithCapture(time.Millisecond)

	hook.AfterQuery(context.Background(),
		newEvent("SELECT * FROM no_such_table", time.Now().Add(-time.Millisecond), assert.AnError))

	records := handler.slowQueryRecords()
	require.Len(t, records, 1)
	errVal, ok := attrOf(records[0], "error")
	require.True(t, ok, "失败查询要带 error 字段")
	assert.Equal(t, assert.AnError.Error(), errVal.String())
}

// runHeavyQuery 跑一条真实耗时超过 10ms 的纯计算查询（递归 CTE，无 I/O 随机性）：
// Open 装配链路的"真的会记"用例靠它给出确定性的慢查询。
func runHeavyQuery(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.NewRaw(`WITH RECURSIVE seq(x) AS
		(SELECT 1 UNION ALL SELECT x+1 FROM seq WHERE x < 300000)
		SELECT COUNT(x) FROM seq`).Exec(context.Background())
	require.NoError(t, err)
}

func openSlowQueryDB(t *testing.T, threshold time.Duration, logger *slog.Logger) *DB {
	t.Helper()
	db, err := Open(Options{
		Path:               t.TempDir() + "/slow.sqlite",
		SlowQueryThreshold: threshold,
	}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Open 装配链路：阈值 >0 时钩子真的挂上了，真实慢查询（远超阈值的纯计算查询）出一条
// Warn，字段齐全。计时走真实时钟，但被测对象比阈值慢两个数量级，粒度无关。
func TestSlowQueryHook_WiredThroughOpen(t *testing.T) {
	handler := &captureHandler{}
	db := openSlowQueryDB(t, 10*time.Millisecond, slog.New(handler))

	runHeavyQuery(t, db)

	records := handler.slowQueryRecords()
	require.Len(t, records, 1)
	opVal, ok := attrOf(records[0], "op")
	require.True(t, ok)
	assert.Equal(t, "SELECT", opVal.String())
	dialectVal, ok := attrOf(records[0], "dialect")
	require.True(t, ok)
	assert.Equal(t, string(DialectSQLite), dialectVal.String())
}

// 默认关：不传阈值时钩子根本没挂（bun 无钩子时连 QueryEvent 都不建），
// 跑同样的重查询一条记录都没有。
func TestSlowQueryHook_SilentWhenDisabled(t *testing.T) {
	handler := &captureHandler{}
	db := openSlowQueryDB(t, 0, slog.New(handler))

	runHeavyQuery(t, db)

	assert.Empty(t, handler.slowQueryRecords(), "默认关闭时不该有任何慢查询日志")
}

// 与捕获型钩子并存互不污染（Q06 卡 §5）：两个库跑同一查询，捕获钩子看到的
// Query 文本逐字节一致——快照 golden 不会因为慢查询钩子的存在而变化。
// （慢查询钩子的 BeforeQuery 不改事件任何字段，捕获只发生在 BeforeQuery。）
func TestSlowQueryHook_CoexistsWithCaptureHook(t *testing.T) {
	db := openSlowQueryDB(t, time.Millisecond, slog.New(&captureHandler{}))
	rec := &queryRecorder{}
	db.AddQueryHook(rec)
	runSampleQuery(t, db)

	ref := openSlowQueryDB(t, 0, slog.New(&captureHandler{}))
	refRec := &queryRecorder{}
	ref.AddQueryHook(refRec)
	runSampleQuery(t, ref)

	require.Len(t, rec.queries, 1)
	require.Len(t, refRec.queries, 1)
	assert.Equal(t, refRec.queries[0], rec.queries[0], "快照钩子看到的文本必须与参照一致")
}

func runSampleQuery(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.NewRaw(`SELECT 1`).Exec(context.Background())
	require.NoError(t, err)
}

// queryRecorder 是快照门禁同形的捕获钩子（只读 Query，不改事件）。
type queryRecorder struct {
	queries []string
}

func (r *queryRecorder) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	r.queries = append(r.queries, event.Query)
	return ctx
}

func (r *queryRecorder) AfterQuery(context.Context, *bun.QueryEvent) {}
