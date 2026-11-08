package jobs

// 慢查询钩子的装配链路（G5/Q06）：Open 把阈值透传给 engine，钩子在生产句柄上
// 真的生效。钩子自身的判定与日志形状在 engine 包测过，这里只证"线接通了"：
// 真实慢查询（递归 CTE 纯计算，比阈值慢两个数量级，时钟粒度无关）出一条 Warn。
// 默认（0）不挂钩子由全部既有测试共同佐证——它们以 0 阈值跑完全量且无慢查询日志。

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jobsLogCapture 是 jobs 包内最小的 slog 捕获器（engine 的同名测试类型跨不了包）。
type jobsLogCapture struct {
	mu      sync.Mutex
	msgs    []string
	records []slog.Record
}

func (h *jobsLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *jobsLogCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *jobsLogCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *jobsLogCapture) WithGroup(string) slog.Handler      { return h }

func (h *jobsLogCapture) slowQueryCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.msgs {
		if m == "slow query" {
			n++
		}
	}
	return n
}

func TestOpen_WiresSlowQueryHook(t *testing.T) {
	handler := &jobsLogCapture{}
	s, err := Open(core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")},
		0, 0, 10*time.Millisecond, slog.New(handler))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.h.NewRaw(`WITH RECURSIVE seq(x) AS
		(SELECT 1 UNION ALL SELECT x+1 FROM seq WHERE x < 300000)
		SELECT COUNT(x) FROM seq`).Exec(bgCtx)
	require.NoError(t, err)

	assert.Equal(t, 1, handler.slowQueryCount(),
		"阈值经 Open 传进 engine 后，真实慢查询要记一条 Warn")
}
