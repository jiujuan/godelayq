package engine

// slowquery.go 是慢查询日志钩子（G5/Q06）：bun QueryHook 按阈值打 Warn 日志。
//
// 取舍都定在设计文档（db-quality-gates-p1-write-fusion-design.md §4）：
//   - 默认关：Options.SlowQueryThreshold 为 0 时 Open 根本不挂钩子——
//     每查询零开销，而不是"钩子里先判一次开关"；
//   - 只记截断文本：bun v1.3 对构建器查询一律内联参数渲染，event.Query 带
//     数据值（任务 payload 可能含敏感内容），日志里只留认出语句形状所需的前缀；
//   - 语句类别与耗时随行：Operation 由 bun 从构建器或语句首关键字得出，
//     计时用 event.StartTime（bun 在 beforeQuery 打点，无钩子时事件根本不建）。

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// DefaultSlowQueryLogBytes 慢查询日志里 SQL 文本的字节上限。取向同
// core.DefaultExecInlinePreview（事件预览上限的口径）：日志要能认出语句形状，
// 不是全文存档——内联参数渲染的 SQL 会带数据值，截断既是排版也是安全闸。
const DefaultSlowQueryLogBytes = 1024

// slowQueryTruncatedMark 截断后缀：读日志的人必须能把"残句"与"完整语句"分开，
// 否则拿着半条 SQL 去比对 golden 或排障会得出错误结论。
const slowQueryTruncatedMark = "…(truncated)"

// slowQueryHook 按阈值把慢查询记成 Warn。多钩子并存安全：BeforeQuery 不改
// 事件的任何字段（bun 的 AfterQuery 逆序执行也不影响——本钩子无状态可污染），
// 快照门禁的捕获钩子（sql_snapshot_test）读到的是同一份 Query 文本。
type slowQueryHook struct {
	threshold time.Duration
	maxBytes  int
	dialect   Dialect
	logger    *slog.Logger
}

// BeforeQuery 原样传递上下文。计时不需要在这里动手脚：event.StartTime 由 bun 打点。
func (h *slowQueryHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	return ctx
}

// AfterQuery 在耗时超过阈值时记一条 Warn。失败查询同样记录（带 error 字段）：
// "慢且失败"与"慢且成功"对排障都是事实，丢哪一半都是账本说谎。
func (h *slowQueryHook) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	elapsed := time.Since(event.StartTime)
	if elapsed < h.threshold {
		return
	}
	attrs := []any{
		"sql", truncateSQL(event.Query, h.maxBytes),
		"elapsed", elapsed,
		"dialect", string(h.dialect),
		"op", event.Operation(),
	}
	if event.Err != nil {
		attrs = append(attrs, "error", event.Err)
	}
	h.logger.Warn("slow query", attrs...)
}

// truncateSQL 按字节上限截断到 rune 边界（标识符可能含非 ASCII），
// 截断时追加标记。
func truncateSQL(query string, maxBytes int) string {
	if maxBytes <= 0 || len(query) <= maxBytes {
		return query
	}
	// 字节切点可能切开多字节字符，ToValidUTF8 把不完整的尾部 rune 整段丢掉。
	return strings.ToValidUTF8(query[:maxBytes], "") + slowQueryTruncatedMark
}
