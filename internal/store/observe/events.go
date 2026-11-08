package observe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/uptrace/bun"
)

// EventLogOptions 是事件写入器的参数。零值一律回到 core 的观测层默认，
// 只有 RetentionAge 的 0 例外——它是"不按时间淘汰"的有意取值（S01 的口径）。
type EventLogOptions struct {
	// FlushInterval 批量落盘周期，<=0 用 core.DefaultObserveFlushInterval。
	FlushInterval time.Duration
	// QueueCapacity 有界队列容量，<=0 用 core.DefaultObserveQueueCapacity。
	QueueCapacity int
	// RetentionCount 保留条数上界，<=0 用 core.DefaultObserveEventRetentionCount。
	// 这里不把 0 解释成"不限量"：那会让一个漏配把表推成无界增长。
	RetentionCount int
	// RetentionAge 保留时长，0 表示不按时间淘汰，<0 同样按 0 处理。
	RetentionAge time.Duration
	// Now 是时间源，nil 时用 time.Now。事件自带时间戳，注入的 Now 只在两个地方生效：
	// 时间戳为零值的事件（测试与第三方发布者）补上它，以及按时间淘汰的截止点。
	Now func() time.Time
}

// EventLog 是 EventBus 的第二个订阅者：把带任务归属的事件写进 job_events。
//
// 它与 api.EventHistory 并列而不是替代（设计文档 D5）：内存那份继续服务未启用库时的
// 读路径，这份负责跨重启留存。写入一律走 batcher，转发协程里不等任何 SQL。
type EventLog struct {
	db     *Handle
	bus    *core.EventBus
	subID  string
	batch  *batcher[eventRecord]
	logger *slog.Logger

	// 保留策略写成原子值：重载链会在运行期写它们（SetRetention），
	// 而读它们的是批量落盘协程，普通字段的读写并发会被 -race 抓住。
	// retentionAge 存纳秒，与 time.Duration 之间只在读写两处转换。
	retentionCount atomic.Int64
	retentionAge   atomic.Int64
	now            func() time.Time

	drainDone chan struct{}
	closeOnce sync.Once
	closeErr  error

	// writeRounds 记 writeBatch 被真正调用的次数（空批不算）。它是"空批不产生事务"
	// 这条口径唯一的观察办法：seq 由自增列给出，一次空事务不会留下任何痕迹，
	// 而计数值不对使用者有意义，所以只在本包内可见。
	writeRounds atomic.Int64
}

// NewEventLog 订阅 bus 的全部事件并开始落库。
//
// 返回的 EventLog 必须在进程退出前 Close：撤订阅 → 等转发协程退出 → 最后一次落盘。
// 顺序颠倒（先关库再关它）就会在已关闭的连接上写入，cmd/server 的 defer 排列守的就是这条。
func NewEventLog(bus *core.EventBus, db *Handle, opts EventLogOptions, logger *slog.Logger) (*EventLog, error) {
	if bus == nil {
		return nil, errors.New("sqlite: NewEventLog requires an event bus")
	}
	if db == nil {
		return nil, errors.New("sqlite: NewEventLog requires an observability database")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	retentionCount, retentionAge := eventRetentionValues(opts.RetentionCount, opts.RetentionAge)

	e := &EventLog{
		db:        db,
		bus:       bus,
		logger:    logger,
		now:       opts.Now,
		drainDone: make(chan struct{}),
	}
	// 原子字段不能进结构体字面量，所以建好体之后一次性 Store。
	e.retentionCount.Store(retentionCount)
	e.retentionAge.Store(retentionAge)

	batch, err := newBatcher[eventRecord](opts.QueueCapacity, opts.FlushInterval, e.writeBatch, func(err error) {
		logger.Error("observability event batch dropped", "error", err, "path", db.Path())
	})
	if err != nil {
		return nil, err
	}
	e.batch = batch

	subID, events := bus.SubscribeAll()
	e.subID = subID
	// 这条协程只做"取出事件 + 非阻塞入队"。总线的 Publish 本身就是非阻塞的
	// （core/event.go 的 select/default），所以两头都不会回压调度。
	go func() {
		defer close(e.drainDone)
		for event := range events {
			record, ok := e.mapEvent(event)
			if !ok {
				continue
			}
			e.batch.append(record)
		}
	}()

	return e, nil
}

// eventRetentionValues 把保留条数与保留时长折成入库的两个取值（条数、纳秒）。
//
// 口径只有一份，构造与 SetRetention 共用：count<=0 回 core.DefaultObserveEventRetentionCount
// （0 不当成"不限量"，那会让一次漏配把表推成无界增长）、age<0 按 0 处理、
// age==0 表示不按时间淘汰。setter 里再抄一遍这两条判断，就出现了第二套规则。
func eventRetentionValues(count int, age time.Duration) (int64, int64) {
	if count <= 0 {
		count = core.DefaultObserveEventRetentionCount
	}
	if age < 0 {
		age = 0
	}
	return int64(count), int64(age)
}

// SetRetention 运行期调整保留条数与时长，下一个批量周期的淘汰用新值。
// 取值口径与 NewEventLog 里的补齐同一条（见 eventRetentionValues），不在这里另立一套。
func (e *EventLog) SetRetention(count int, age time.Duration) {
	retentionCount, retentionAge := eventRetentionValues(count, age)
	e.retentionCount.Store(retentionCount)
	e.retentionAge.Store(retentionAge)
}

// mapEvent 把一条事件折成一行记录；第二个返回值为 false 表示这条该跳过。
//
// 跳过的是没有任务归属的事件（heap.updated 这类）：与 api/history.go 的 record 同一判断，
// 理由也相同——没有归属的事件塞进任何任务的时间线都是噪音。
func (e *EventLog) mapEvent(event core.Event) (eventRecord, bool) {
	if event.JobID == "" {
		return eventRecord{}, false
	}

	timestamp := event.Timestamp
	if timestamp.IsZero() {
		// 时间戳缺失时补当前时间：库里 ts_us 是 NOT NULL，而按 seq 定序仍然成立
		timestamp = e.now()
	}

	record := eventRecord{
		TimestampUS: timestamp.UnixMicro(),
		Type:        string(event.Type),
		JobID:       event.JobID,
		JobName:     event.JobName,
		Status:      int(event.Status),
		// Data 已是 json.RawMessage，原文入库：重新序列化会改键序，
		// 而排障时最常比对的正是"事件里写的那段错误原文"
		Data: nullableText(event.Data),
	}
	// 出箱序号（TASK-RE07）：中继器投递的事件带值，直发事件为 0 → NULL。
	// 读侧按它去重（部分唯一索引 + 冲突忽略），at-least-once 重放不产生重复行。
	if event.OutboxSeq > 0 {
		record.OutboxSeq = sql.NullInt64{Int64: event.OutboxSeq, Valid: true}
	}
	if event.Metadata != nil {
		encoded, err := json.Marshal(event.Metadata)
		if err != nil {
			// 一条事件的元数据不该挡住整批：存 NULL 并记 warn，读侧把这列当缺失处理
			e.logger.Warn("observability event metadata is not serializable",
				"job_id", event.JobID, "type", record.Type, "error", err)
		} else {
			record.Metadata = sql.NullString{String: string(encoded), Valid: true}
		}
	}
	return record, true
}

// nullableText 把一段可选的 JSON 原文转成可空列值。nil 与空切片都是 NULL，
// 而不是字符串 "null"——后者会在读侧被当成一段合法 JSON。
func nullableText(raw []byte) sql.NullString {
	if len(raw) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(raw), Valid: true}
}

// writeBatch 在一个事务里写完这批事件，并按保留策略淘汰旧行。
// 这是 batcher 的落盘回调，不做重试以外的事情：失败就返回错误，由 batcher 决定再试一次还是放弃。
func (e *EventLog) writeBatch(rows []eventRecord) error {
	e.writeRounds.Add(1)
	err := e.db.RunInTx(bgCtx, nil, func(ctx context.Context, tx bun.Tx) error {
		// 一次多值插入替代 Prepare+逐行 Exec：同一事务、同一批数据，
		// 往返从 N 次降到 1 次。rows 恒非空（batcher 不调空批回调）。
		// 冲突忽略（TASK-RE07）：outbox_seq 命中部分唯一索引的行是中继器
		// at-least-once 重放的重复事件，跳过而不是报错——直发行（NULL）不进
		// 索引、永不冲突。两方言的冲突子句同形，不需要分支点。
		if _, err := tx.NewInsert().Model(&rows).
			On("CONFLICT (outbox_seq) WHERE outbox_seq IS NOT NULL DO NOTHING").
			Exec(ctx); err != nil {
			return fmt.Errorf("sqlite: insert event batch: %w", err)
		}
		if err := e.prune(ctx, tx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// prune 按保留条数与保留时长淘汰旧行，与插入在同一个事务里。
//
// 频率是"每批一次"而不是"每秒一次"：MAX(seq) 走索引是常数时间，而每批一次已经能保证
// 上界不被突破（多攒几批只是让淘汰晚一点发生，不会让表无界增长）。
// 条数淘汰按 seq 而不是时间：seq 就是写入顺序，切在它上面等价于"留最后 N 条"，
// 与同一毫秒内连发几条无关。
//
// 两个原子值在开头各读一次：一次淘汰里"跳过时间淘汰"的判断与算截止点必须用同一个时长，
// 中间插进一次 SetRetention 就会让两条判断对上两套值。
func (e *EventLog) prune(ctx context.Context, tx bun.Tx) error {
	retentionCount := e.retentionCount.Load()
	retentionAge := time.Duration(e.retentionAge.Load())
	if _, err := tx.NewDelete().Model((*eventRecord)(nil)).
		Where("seq <= (SELECT COALESCE(MAX(seq)-?, 0) FROM job_events)", retentionCount).
		Exec(ctx); err != nil {
		return fmt.Errorf("sqlite: prune events by count: %w", err)
	}
	if retentionAge <= 0 {
		// 0 是"不按时间淘汰"的有意取值，直接跳过而不是写一条恒假谓词
		return nil
	}
	cutoff := e.now().Add(-retentionAge).UnixMicro()
	if _, err := tx.NewDelete().Model((*eventRecord)(nil)).
		Where("ts_us < ?", cutoff).
		Exec(ctx); err != nil {
		return fmt.Errorf("sqlite: prune events by age: %w", err)
	}
	return nil
}

// eventsReadLimitFallback 是 limit 传了零或负数时使用的行数上界。
// 调用方里的读端点自带一个同值的响应上界（api/handlers_events.go 的 eventsQueryLimit）：
// 这里兜底是为了让"忘记传 limit"退化成少读，而不是把整张表读进内存。
const eventsReadLimitFallback = 1000

// Events 按任务读回最近 limit 条事件，结果按写入顺序升序。
//
// 读的是 seq 而不是 ts_us：同一微秒内连发的 scheduled 与 started 只有 seq 分得出先后，
// 而时间线把这两条的先后当作因果（设计文档 §6）。
func (e *EventLog) Events(jobID string, limit int) ([]core.Event, error) {
	return e.readEvents(func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("job_id = ?", jobID)
	}, limit)
}

// Recent 读回跨任务的最近 limit 条事件，结果按写入顺序升序。
func (e *EventLog) Recent(limit int) ([]core.Event, error) {
	return e.readEvents(func(q *bun.SelectQuery) *bun.SelectQuery {
		return q
	}, limit)
}

// readEvents 执行一次降序查询，再把结果整体反转成升序。
//
// 降序 + 反转而不是升序 + 偏移：要拿的是"最后 N 条"，降序走 seq 索引只需读 N 行，
// 升序取尾必须先跳过前面所有行。scope 是调用方附加的过滤条件（按任务或全表）。
func (e *EventLog) readEvents(scope func(*bun.SelectQuery) *bun.SelectQuery, limit int) ([]core.Event, error) {
	if limit <= 0 {
		limit = eventsReadLimitFallback
	}
	if scope == nil {
		scope = func(q *bun.SelectQuery) *bun.SelectQuery { return q }
	}

	var rows []eventRecord
	if err := scope(e.db.NewSelect().Model(&rows)).
		OrderExpr("seq DESC").
		Limit(int64(limit)).
		Scan(bgCtx); err != nil {
		return nil, fmt.Errorf("sqlite: query events: %w", err)
	}

	// 端点把空列表序列化成了 "items": []，读侧也保持同一个形状：
	// 返回 nil 切片会让 JSON 变成 null，前端两处都按数组处理。
	items := make([]core.Event, 0, min(limit, 256))
	for _, row := range rows {
		event, err := eventOf(row)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}
		items = append(items, event)
	}

	// 库里按 seq 降序取回，反转为升序：调用方拿到的时间线方向与内存缓冲一致
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
	return items, nil
}

// eventOf 把一行还原成 core.Event。
//
// ts_us 只到微秒，还原出的 Time 丢了原来的纳秒部分与原始时区偏移（这里是本地时区）：
// 两个时刻相同，精度界限见设计文档 §9 与本卡 §9 的第一条风险。
// data 存的是发布方的 JSON 原文，这里不重新序列化，读侧拿到的字节与写入时一致。
func eventOf(row eventRecord) (core.Event, error) {
	event := core.Event{
		Type:      core.EventType(row.Type),
		JobID:     row.JobID,
		JobName:   row.JobName,
		Status:    core.JobStatus(row.Status),
		Timestamp: time.UnixMicro(row.TimestampUS),
	}
	if row.Data.Valid {
		event.Data = json.RawMessage(row.Data.String)
	}
	if row.Metadata.Valid {
		parsed := map[string]interface{}{}
		if err := json.Unmarshal([]byte(row.Metadata.String), &parsed); err != nil {
			return core.Event{}, fmt.Errorf("metadata of job %s is not decodable: %w", row.JobID, err)
		}
		event.Metadata = parsed
	}
	if row.OutboxSeq.Valid {
		event.OutboxSeq = row.OutboxSeq.Int64
	}
	return event, nil
}

// Flush 立即落盘当前批次，空批次不产生事务。供关停路径与测试使用。
func (e *EventLog) Flush() error {
	return e.batch.Flush()
}

// Dropped 返回累计丢弃条数（队满、二次失败、Close 后写入）。
// 接口层与 /admin/runtime 要把它报出来：读端点看着完整但实际缺页是最坏的结局。
func (e *EventLog) Dropped() int64 {
	return e.batch.Dropped()
}

// Count 返回库内事件行数，主要是给读端点与验收命令对照用。
func (e *EventLog) Count() (int64, error) {
	count, err := e.db.NewSelect().Model((*eventRecord)(nil)).Count(bgCtx)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count events: %w", err)
	}
	return count, nil
}

// Close 撤订阅、等转发协程退出、把剩余批次落盘。幂等：重复调用返回第一次的结果。
//
// 顺序不能换：先 Unsubscribe（总线会关掉这个订阅的通道，协程随即从 range 退出），
// 再等它退出，最后关写入器。反过来就会有一次 append 落在已经关掉的 batcher 上，
// 那条记录被计入丢弃而不是落库。
func (e *EventLog) Close() error {
	e.closeOnce.Do(func() {
		e.bus.Unsubscribe(e.subID)
		<-e.drainDone
		e.closeErr = e.batch.Close()
	})
	return e.closeErr
}
