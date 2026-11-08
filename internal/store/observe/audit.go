package observe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/uptrace/bun"
)

// AuditLogOptions 是台账写入器的参数。零值一律回到 core 的观测层默认，
// 只有 RetentionAge 的 0 例外——它是"不按时间淘汰"的有意取值（S01 的口径）。
type AuditLogOptions struct {
	// FlushInterval 批量落盘周期，<=0 用 core.DefaultObserveFlushInterval。
	FlushInterval time.Duration
	// QueueCapacity 有界队列容量，<=0 用 core.DefaultObserveQueueCapacity。
	QueueCapacity int
	// RetentionCount 保留条数上界，<=0 用 core.DefaultObserveAuditRetentionCount。
	RetentionCount int
	// RetentionAge 保留时长，0 表示不按时间淘汰，<0 同样按 0 处理。
	RetentionAge time.Duration
	// Now 是时间源，nil 时用 time.Now。台账行自带时间（请求到达的那一刻），
	// 注入的 Now 只用在按时间淘汰的截止点上。
	Now func() time.Time
}

// AuditLog 把每个写操作登记成一行台账，并回答 /admin/audit 的查询。
//
// 它与 api 的关系是"消费方声明能力"：接口（api.AuditRecorder / api.AuditReader）
// 定义在 api/audit.go，本包不 import api、只提供隐式实现——是否满足由装配方
// （cmd/server 的 newAuditLog 闭包把返回值交给 auditLogAPI）在编译期把关。
// 行结构是 core.AuditEntry：共享词汇放 core，本包与 api 各自依赖它，互不相认。
type AuditLog struct {
	db     *Handle
	batch  *batcher[auditRecord]
	logger *slog.Logger

	// 保留策略写成原子值：重载链会在运行期写它们（SetRetention），
	// 而读它们的是批量落盘协程，普通字段的读写并发会被 -race 抓住。
	// retentionAge 存纳秒，与 time.Duration 之间只在读写两处转换。
	retentionCount atomic.Int64
	retentionAge   atomic.Int64
	now            func() time.Time

	closeOnce sync.Once
	closeMu   sync.Mutex
	closed    bool
	closeErr  error
}

// NewAuditLog 开始接收台账行。返回的 AuditLog 必须在观测库关闭之前 Close：
// 剩下的批次要先落盘，连接才能关。
func NewAuditLog(db *Handle, opts AuditLogOptions, logger *slog.Logger) (*AuditLog, error) {
	if db == nil {
		return nil, errors.New("sqlite: NewAuditLog requires an observability database")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	retentionCount, retentionAge := auditRetentionValues(opts.RetentionCount, opts.RetentionAge)

	a := &AuditLog{
		db:     db,
		logger: logger,
		now:    opts.Now,
	}
	// 原子字段不能进结构体字面量，所以建好体之后一次性 Store。
	a.retentionCount.Store(retentionCount)
	a.retentionAge.Store(retentionAge)

	batch, err := newBatcher[auditRecord](opts.QueueCapacity, opts.FlushInterval, a.writeBatch, func(err error) {
		logger.Error("observability audit batch dropped", "error", err, "path", db.Path())
	})
	if err != nil {
		return nil, err
	}
	a.batch = batch

	return a, nil
}

// Append 把一行台账放进队列，不等 SQL、不等落盘。
//
// 队满时这条被丢弃并计入 Dropped，返回值仍是 nil：让一次丢弃变成请求路径上的 warn，
// 等于在高负载时把台账的问题转嫁给响应。真正需要立刻看见的只有一种——写入器已经关闭，
// 那说明关停顺序写错（台账还在收请求，连接已经关了），所以它返回错误。
func (a *AuditLog) Append(entry core.AuditEntry) error {
	a.closeMu.Lock()
	defer a.closeMu.Unlock()
	if a.closed {
		return errors.New("sqlite: audit log is closed")
	}
	a.batch.append(a.mapEntry(entry))
	return nil
}

// auditRetentionValues 把保留条数与保留时长折成入库的两个取值（条数、纳秒）。
//
// 口径只有一份，构造与 SetRetention 共用：count<=0 回 core.DefaultObserveAuditRetentionCount
// （0 不当成"不限量"，那会让一次漏配把表推成无界增长）、age<0 按 0 处理、
// age==0 表示不按时间淘汰。setter 里再抄一遍这两条判断，就出现了第二套规则。
func auditRetentionValues(count int, age time.Duration) (int64, int64) {
	if count <= 0 {
		count = core.DefaultObserveAuditRetentionCount
	}
	if age < 0 {
		age = 0
	}
	return int64(count), int64(age)
}

// SetRetention 运行期调整保留条数与时长，下一个批量周期的淘汰用新值。
// 取值口径与 NewAuditLog 里的补齐同一条（见 auditRetentionValues），不在这里另立一套。
func (a *AuditLog) SetRetention(count int, age time.Duration) {
	retentionCount, retentionAge := auditRetentionValues(count, age)
	a.retentionCount.Store(retentionCount)
	a.retentionAge.Store(retentionAge)
}

// mapEntry 把 api 侧的行折成库里的一行。空字符串的可空列写成 NULL，
// 读回来时也是空字符串，往返不丢信息。role 列不参与这个映射：它写原样
// 字符串（未认证记空串），与迁移前同一写法。
func (a *AuditLog) mapEntry(entry core.AuditEntry) auditRecord {
	timestamp := entry.Time
	if timestamp.IsZero() {
		timestamp = a.now()
	}

	record := auditRecord{
		TimestampUS: timestamp.UnixMicro(),
		Actor:       entry.Actor,
		ActorKind:   entry.ActorKind,
		Role:        entry.Role,
		Action:      entry.Action,
		Method:      entry.Method,
		Route:       entry.Route,
		Status:      entry.Status,
		LatencyUS:   entry.Latency.Microseconds(),
		Verdict:     entry.Verdict,
		ExecVerdict: optionalText(entry.ExecVerdict),
		ExecReason:  optionalText(entry.ExecReasonCode),
		HandlerKey:  optionalText(entry.HandlerKey),
		Profile:     optionalText(entry.Profile),
		JobID:       optionalText(entry.JobID),
		RemoteIP:    optionalText(entry.RemoteIP),
		UserAgent:   optionalText(entry.UserAgent),
	}
	return record
}

// optionalText 把空串映射成 NULL。
func optionalText(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

// writeBatch 在一个事务里写完这批台账，并按保留策略淘汰旧行。
func (a *AuditLog) writeBatch(rows []auditRecord) error {
	return a.db.RunInTx(bgCtx, nil, func(ctx context.Context, tx bun.Tx) error {
		// 一次多值插入替代 Prepare+逐行 Exec：同一事务、同一批数据，往返降到 1 次。
		// rows 恒非空（batcher 不调空批回调）。
		if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
			return fmt.Errorf("sqlite: insert audit batch: %w", err)
		}
		if err := a.prune(ctx, tx); err != nil {
			return err
		}
		return nil
	})
}

// prune 按保留条数与保留时长淘汰旧行，与插入同一个事务。形状与事件表那条相同（S03）：
// 条数淘汰切在 seq 上（seq 就是写入顺序），时间淘汰跳过 0 而不是写一条恒假谓词。
//
// 两个原子值在开头各读一次：一次淘汰里"跳过时间淘汰"的判断与算截止点必须用同一个时长，
// 中间插进一次 SetRetention 就会让两条判断对上两套值。
func (a *AuditLog) prune(ctx context.Context, tx bun.Tx) error {
	retentionCount := a.retentionCount.Load()
	retentionAge := time.Duration(a.retentionAge.Load())
	if _, err := tx.NewDelete().Model((*auditRecord)(nil)).
		Where("seq <= (SELECT COALESCE(MAX(seq)-?, 0) FROM write_audit)", retentionCount).
		Exec(ctx); err != nil {
		return fmt.Errorf("sqlite: prune audit by count: %w", err)
	}
	if retentionAge <= 0 {
		return nil
	}
	cutoff := a.now().Add(-retentionAge).UnixMicro()
	if _, err := tx.NewDelete().Model((*auditRecord)(nil)).
		Where("ts_us < ?", cutoff).
		Exec(ctx); err != nil {
		return fmt.Errorf("sqlite: prune audit by age: %w", err)
	}
	return nil
}

// auditReadLimitFallback 是 filter 没带 limit 时使用的行数上界。
// 端点那一侧有一个同值上限（api/handlers_admin.go 的 auditMaxLimit）：两处都要改，
// 少一处就等于给某条绕过端点的调用开了一个整表读取。
const auditReadLimitFallback = 500

// Query 按过滤条件读台账，顺序是 seq 降序（最新在前），并返回不带 limit/offset 的匹配总数。
//
// 总数用第二条 COUNT 查询而不是窗口函数：窗口函数省下的是一次往返、多付的是每行一个
// 重复值。台账的读频率是"运维查一次"，两条语句的形态更好读，也更不容易在改分页时被改错。
//
// Since/Until 都是闭区间（>= 与 <=），两端都含：运维圈一天的范围时不该纠结边界那条算不算。
func (a *AuditLog) Query(f core.AuditFilter) ([]core.AuditEntry, int, error) {
	total64, err := a.db.NewSelect().Model((*auditRecord)(nil)).
		Apply(auditScope(f)).
		Count(bgCtx)
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: count audit rows: %w", err)
	}
	total := int(total64)

	limit := f.Limit
	if limit <= 0 {
		limit = auditReadLimitFallback
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var rows []auditRecord
	if err := a.db.NewSelect().Model(&rows).
		Apply(auditScope(f)).
		OrderExpr("seq DESC").
		Limit(int64(limit)).
		Offset(int64(offset)).
		Scan(bgCtx); err != nil {
		return nil, 0, fmt.Errorf("sqlite: query audit rows: %w", err)
	}

	// 空列表而不是 nil：端点把它序列化出去是 []
	items := make([]core.AuditEntry, 0, min(limit, 256))
	for _, row := range rows {
		items = append(items, auditEntryOf(row))
	}
	return items, total, nil
}

// auditScope 把过滤条件折成构建器的 Where 链。每个值都走语句参数，
// 没有任何拼接进来的字符串；无过滤条件时原样返回。
func auditScope(f core.AuditFilter) func(*bun.SelectQuery) *bun.SelectQuery {
	return func(q *bun.SelectQuery) *bun.SelectQuery {
		if f.Actor != "" {
			q = q.Where("actor = ?", f.Actor)
		}
		if f.Action != "" {
			q = q.Where("action = ?", f.Action)
		}
		if f.Verdict != "" {
			q = q.Where("verdict = ?", f.Verdict)
		}
		if f.Since != nil {
			q = q.Where("ts_us >= ?", f.Since.UnixMicro())
		}
		if f.Until != nil {
			q = q.Where("ts_us <= ?", f.Until.UnixMicro())
		}
		return q
	}
}

// auditEntryOf 还原一行。ts_us 只到微秒，读回来的时间丢原来的纳秒与原始时区（这里是本地时区），
// 与事件表的读回完全同一口径（S04 卡 §9）。
func auditEntryOf(row auditRecord) core.AuditEntry {
	return core.AuditEntry{
		Time:           time.UnixMicro(row.TimestampUS),
		Actor:          row.Actor,
		ActorKind:      row.ActorKind,
		Role:           row.Role,
		Action:         row.Action,
		Method:         row.Method,
		Route:          row.Route,
		Status:         row.Status,
		Latency:        time.Duration(row.LatencyUS) * time.Microsecond,
		Verdict:        row.Verdict,
		ExecVerdict:    row.ExecVerdict.String,
		ExecReasonCode: row.ExecReason.String,
		HandlerKey:     row.HandlerKey.String,
		Profile:        row.Profile.String,
		JobID:          row.JobID.String,
		RemoteIP:       row.RemoteIP.String,
		UserAgent:      row.UserAgent.String,
	}
}

// Flush 立即落盘当前批次，空批次不产生事务。供关停路径与测试使用。
func (a *AuditLog) Flush() error {
	return a.batch.Flush()
}

// Dropped 返回累计丢弃条数（队满、关闭后写入、二次失败）。
func (a *AuditLog) Dropped() int64 {
	return a.batch.Dropped()
}

// Count 返回库内台账行数，给验收命令与查询端点的 total 对照用。
func (a *AuditLog) Count() (int64, error) {
	count, err := a.db.NewSelect().Model((*auditRecord)(nil)).Count(bgCtx)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count audit rows: %w", err)
	}
	return count, nil
}

// Close 停掉周期落盘并把剩余行写完。幂等：重复调用返回第一次的结果。
//
// 封口状态记在自己身上而不是只交给 batcher：Append 要在关闭之后返回错误，
// 让"关停顺序写错"这件事在第一次晚到的写入上就露出来。
func (a *AuditLog) Close() error {
	a.closeOnce.Do(func() {
		a.closeMu.Lock()
		a.closed = true
		a.closeMu.Unlock()
		a.closeErr = a.batch.Close()
	})
	return a.closeErr
}
