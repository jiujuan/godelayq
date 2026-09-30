package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"godelayq/core"
)

// insertEvent 是一行 job_events 的写入语句。seq 由 AUTOINCREMENT 给出，不由代码算：
// 同一次执行会在同一毫秒内连发 job.scheduled 与 job.started，只有 seq 能定出因果
// （设计文档 §6）。
const insertEvent = `INSERT INTO job_events (ts_us, type, job_id, job_name, status, data, metadata)
VALUES (?, ?, ?, ?, ?, ?, ?)`

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

// eventRecord 是 job_events 的一行，字段与列一一对应。
// data 与 metadata 用 sql.NullString：两列都可空，空与空字符串在库里是两回事。
type eventRecord struct {
	timestampUS int64
	typ         string
	jobID       string
	jobName     string
	status      int
	data        sql.NullString
	metadata    sql.NullString
}

// EventLog 是 EventBus 的第二个订阅者：把带任务归属的事件写进 job_events。
//
// 它与 api.EventHistory 并列而不是替代（设计文档 D5）：内存那份继续服务未启用库时的
// 读路径，这份负责跨重启留存。写入一律走 batcher，转发协程里不等任何 SQL。
type EventLog struct {
	db     *DB
	bus    *core.EventBus
	subID  string
	batch  *batcher[eventRecord]
	logger *slog.Logger

	retentionCount int
	retentionAge   time.Duration
	now            func() time.Time

	drainDone chan struct{}
	closeOnce sync.Once
	closeErr  error

	// writeRounds 记 writeBatch 被真正调用的次数（空批不算）。它是"空批不产生事务"
	// 这条口径唯一的观察办法：seq 由 AUTOINCREMENT 给出，一次空事务不会留下任何痕迹，
	// 而计数值不对使用者有意义，所以只在本包内可见。
	writeRounds atomic.Int64
}

// NewEventLog 订阅 bus 的全部事件并开始落库。
//
// 返回的 EventLog 必须在进程退出前 Close：撤订阅 → 等转发协程退出 → 最后一次落盘。
// 顺序颠倒（先关库再关它）就会在已关闭的连接上写入，cmd/server 的 defer 排列守的就是这条。
func NewEventLog(bus *core.EventBus, db *DB, opts EventLogOptions, logger *slog.Logger) (*EventLog, error) {
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
	retentionCount := opts.RetentionCount
	if retentionCount <= 0 {
		retentionCount = core.DefaultObserveEventRetentionCount
	}
	retentionAge := opts.RetentionAge
	if retentionAge < 0 {
		retentionAge = 0
	}

	e := &EventLog{
		db:             db,
		bus:            bus,
		logger:         logger,
		retentionCount: retentionCount,
		retentionAge:   retentionAge,
		now:            opts.Now,
		drainDone:      make(chan struct{}),
	}

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
		timestampUS: timestamp.UnixMicro(),
		typ:         string(event.Type),
		jobID:       event.JobID,
		jobName:     event.JobName,
		status:      int(event.Status),
		// Data 已是 json.RawMessage，原文入库：重新序列化会改键序，
		// 而排障时最常比对的正是"事件里写的那段错误原文"
		data: nullableText(event.Data),
	}
	if event.Metadata != nil {
		encoded, err := json.Marshal(event.Metadata)
		if err != nil {
			// 一条事件的元数据不该挡住整批：存 NULL 并记 warn，读侧把这列当缺失处理
			e.logger.Warn("observability event metadata is not serializable",
				"job_id", event.JobID, "type", record.typ, "error", err)
		} else {
			record.metadata = sql.NullString{String: string(encoded), Valid: true}
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
	tx, err := e.db.sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("sqlite: begin event batch: %w", err)
	}
	stmt, err := tx.Prepare(insertEvent)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sqlite: prepare event insert: %w", err)
	}
	for _, row := range rows {
		if _, err := stmt.Exec(row.timestampUS, row.typ, row.jobID, row.jobName, row.status, row.data, row.metadata); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: insert event %s/%s: %w", row.typ, row.jobID, err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sqlite: close event statement: %w", err)
	}
	if err := e.prune(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit event batch: %w", err)
	}
	return nil
}

// prune 按保留条数与保留时长淘汰旧行，与插入在同一个事务里。
//
// 频率是"每批一次"而不是"每秒一次"：MAX(seq) 走索引是常数时间，而每批一次已经能保证
// 上界不被突破（多攒几批只是让淘汰晚一点发生，不会让表无界增长）。
// 条数淘汰按 seq 而不是时间：seq 就是写入顺序，切在它上面等价于"留最后 N 条"，
// 与同一毫秒内连发几条无关。
func (e *EventLog) prune(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM job_events WHERE seq <= (SELECT COALESCE(MAX(seq)-?, 0) FROM job_events)`,
		e.retentionCount); err != nil {
		return fmt.Errorf("sqlite: prune events by count: %w", err)
	}
	if e.retentionAge <= 0 {
		// 0 是"不按时间淘汰"的有意取值，直接跳过而不是写一条恒假谓词
		return nil
	}
	cutoff := e.now().Add(-e.retentionAge).UnixMicro()
	if _, err := tx.Exec(`DELETE FROM job_events WHERE ts_us < ?`, cutoff); err != nil {
		return fmt.Errorf("sqlite: prune events by age: %w", err)
	}
	return nil
}

// eventColumns 是两条读路径共用的列序，顺序与 scanEvent 一一对应。
const eventColumns = `ts_us, type, job_id, job_name, status, data, metadata`

// eventsReadLimitFallback 是 limit 传了零或负数时使用的行数上界。
// 调用方里的读端点自带一个同值的响应上界（api/handlers_events.go 的 eventsQueryLimit）：
// 这里兜底是为了让"忘记传 limit"退化成少读，而不是把整张表读进内存。
const eventsReadLimitFallback = 1000

// Events 按任务读回最近 limit 条事件，结果按写入顺序升序。
//
// 读的是 seq 而不是 ts_us：同一微秒内连发的 scheduled 与 started 只有 seq 分得出先后，
// 而时间线把这两条的先后当作因果（设计文档 §6）。
func (e *EventLog) Events(jobID string, limit int) ([]core.Event, error) {
	return e.readEvents(
		`SELECT `+eventColumns+` FROM job_events WHERE job_id = ? ORDER BY seq DESC LIMIT ?`,
		limit, jobID)
}

// Recent 读回跨任务的最近 limit 条事件，结果按写入顺序升序。
func (e *EventLog) Recent(limit int) ([]core.Event, error) {
	return e.readEvents(
		`SELECT `+eventColumns+` FROM job_events ORDER BY seq DESC LIMIT ?`,
		limit)
}

// readEvents 执行一次降序查询，再把结果整体反转成升序。
//
// 降序 + 反转而不是升序 + 偏移：要拿的是"最后 N 条"，降序走 seq 索引只需读 N 行，
// 升序取尾必须先跳过前面所有行。args 是 WHERE 的参数，limit 单独追加。
func (e *EventLog) readEvents(query string, limit int, args ...any) ([]core.Event, error) {
	if limit <= 0 {
		limit = eventsReadLimitFallback
	}
	rows, err := e.db.sqlDB.Query(query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query events: %w", err)
	}
	defer rows.Close()

	// 端点把空列表序列化成了 "items": []，读侧也保持同一个形状：
	// 返回 nil 切片会让 JSON 变成 null，前端两处都按数组处理。
	items := make([]core.Event, 0, min(limit, 256))
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan event: %w", err)
		}
		items = append(items, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate events: %w", err)
	}

	// 库里按 seq 降序取回，反转为升序：调用方拿到的时间线方向与内存缓冲一致
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
	return items, nil
}

// rowScanner 是 *sql.Rows 的最小读面，让行还原能单独被测。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanEvent 把一行还原成 core.Event。
//
// ts_us 只到微秒，还原出的 Time 丢了原来的纳秒部分与原始时区偏移（这里是本地时区）：
// 两个时刻相同，精度界限见设计文档 §9 与本卡 §9 的第一条风险。
// data 存的是发布方的 JSON 原文，这里不重新序列化，读侧拿到的字节与写入时一致。
func scanEvent(row rowScanner) (core.Event, error) {
	var (
		timestampUS int64
		typ         string
		jobID       string
		jobName     string
		status      int
		data        sql.NullString
		metadata    sql.NullString
	)
	if err := row.Scan(&timestampUS, &typ, &jobID, &jobName, &status, &data, &metadata); err != nil {
		return core.Event{}, err
	}

	event := core.Event{
		Type:      core.EventType(typ),
		JobID:     jobID,
		JobName:   jobName,
		Status:    core.JobStatus(status),
		Timestamp: time.UnixMicro(timestampUS),
	}
	if data.Valid {
		event.Data = json.RawMessage(data.String)
	}
	if metadata.Valid {
		parsed := map[string]interface{}{}
		if err := json.Unmarshal([]byte(metadata.String), &parsed); err != nil {
			return core.Event{}, fmt.Errorf("metadata of job %s is not decodable: %w", jobID, err)
		}
		event.Metadata = parsed
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
	var count int64
	if err := e.db.sqlDB.QueryRow(`SELECT COUNT(*) FROM job_events`).Scan(&count); err != nil {
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
