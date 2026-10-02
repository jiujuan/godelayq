package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"godelayq/api"
	"godelayq/core"
)

// insertAudit 是一行 write_audit 的写入语句。seq 由 AUTOINCREMENT 给出：
// 台账的顺序就是发生顺序，而同一毫秒内的两次提交只有 seq 分得出先后。
const insertAudit = `INSERT INTO write_audit
	(ts_us, actor, actor_kind, role, action, method, route, status, latency_us, verdict,
	 exec_verdict, exec_reason_code, handler_key, profile, job_id, remote_ip, user_agent)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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

// auditRecord 是 write_audit 的一行。可空列用 sql.NullString：
// "没有执行器结论"与"执行器结论是空串"在库里必须是两回事，
// 否则普通任务提交看起来像一次被拒的执行器提交。
type auditRecord struct {
	timestampUS int64
	actor       string
	actorKind   string
	role        string
	action      string
	method      string
	route       string
	status      int
	latencyUS   int64
	verdict     string

	execVerdict sql.NullString
	execReason  sql.NullString
	handlerKey  sql.NullString
	profile     sql.NullString
	jobID       sql.NullString
	remoteIP    sql.NullString
	userAgent   sql.NullString
}

// AuditLog 把每个写操作登记成一行台账，并回答 /admin/audit 的查询。
//
// 它与 api 的关系是反过来的常见读法：接口（AuditRecorder / AuditReader）与行结构
// 定义在消费方 api/audit.go，这一侧 import 它并实现。方向与产物索引那张表一致
// （store/sqlite 已经 import executor），api 仍然不知道驱动在哪。
type AuditLog struct {
	db     *DB
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
func NewAuditLog(db *DB, opts AuditLogOptions, logger *slog.Logger) (*AuditLog, error) {
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
func (a *AuditLog) Append(entry api.AuditEntry) error {
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
// 读回来时也是空字符串，往返不丢信息。
func (a *AuditLog) mapEntry(entry api.AuditEntry) auditRecord {
	timestamp := entry.Time
	if timestamp.IsZero() {
		timestamp = a.now()
	}

	record := auditRecord{
		timestampUS: timestamp.UnixMicro(),
		actor:       entry.Actor,
		actorKind:   entry.ActorKind,
		role:        entry.Role,
		action:      entry.Action,
		method:      entry.Method,
		route:       entry.Route,
		status:      entry.Status,
		latencyUS:   entry.Latency.Microseconds(),
		verdict:     entry.Verdict,
		execVerdict: optionalText(entry.ExecVerdict),
		execReason:  optionalText(entry.ExecReasonCode),
		handlerKey:  optionalText(entry.HandlerKey),
		profile:     optionalText(entry.Profile),
		jobID:       optionalText(entry.JobID),
		remoteIP:    optionalText(entry.RemoteIP),
		userAgent:   optionalText(entry.UserAgent),
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
	tx, err := a.db.sqlDB.Begin()
	if err != nil {
		return fmt.Errorf("sqlite: begin audit batch: %w", err)
	}
	stmt, err := tx.Prepare(insertAudit)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sqlite: prepare audit insert: %w", err)
	}
	for _, row := range rows {
		if _, err := stmt.Exec(row.timestampUS, row.actor, row.actorKind, row.role, row.action,
			row.method, row.route, row.status, row.latencyUS, row.verdict,
			row.execVerdict, row.execReason, row.handlerKey, row.profile, row.jobID,
			row.remoteIP, row.userAgent); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: insert audit row %s %s: %w", row.method, row.action, err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("sqlite: close audit statement: %w", err)
	}
	if err := a.prune(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit audit batch: %w", err)
	}
	return nil
}

// prune 按保留条数与保留时长淘汰旧行，与插入同一个事务。形状与事件表那条相同（S03）：
// 条数淘汰切在 seq 上（seq 就是写入顺序），时间淘汰跳过 0 而不是写一条恒假谓词。
//
// 两个原子值在开头各读一次：一次淘汰里"跳过时间淘汰"的判断与算截止点必须用同一个时长，
// 中间插进一次 SetRetention 就会让两条判断对上两套值。
func (a *AuditLog) prune(tx *sql.Tx) error {
	retentionCount := a.retentionCount.Load()
	retentionAge := time.Duration(a.retentionAge.Load())
	if _, err := tx.Exec(`DELETE FROM write_audit WHERE seq <= (SELECT COALESCE(MAX(seq)-?, 0) FROM write_audit)`,
		retentionCount); err != nil {
		return fmt.Errorf("sqlite: prune audit by count: %w", err)
	}
	if retentionAge <= 0 {
		return nil
	}
	cutoff := a.now().Add(-retentionAge).UnixMicro()
	if _, err := tx.Exec(`DELETE FROM write_audit WHERE ts_us < ?`, cutoff); err != nil {
		return fmt.Errorf("sqlite: prune audit by age: %w", err)
	}
	return nil
}

// auditColumns 是读取路径的列序，与 scanAudit 一一对应。
const auditColumns = `ts_us, actor, actor_kind, role, action, method, route, status,
	latency_us, verdict, exec_verdict, exec_reason_code, handler_key, profile, job_id,
	remote_ip, user_agent`

// auditReadLimitFallback 是 filter 没带 limit 时使用的行数上界。
// 端点那一侧有一个同值上限（api/handlers_admin.go 的 auditMaxLimit）：两处都要改，
// 少一处就等于给某条绕过端点的调用开了一个整表读取。
const auditReadLimitFallback = 500

// Query 按过滤条件读台账，顺序是 seq 降序（最新在前），并返回不带 limit/offset 的匹配总数。
//
// 总数用第二条 COUNT 查询而不是窗口函数：SQLite 的 `COUNT(*) OVER ()` 要 3.25 以上，
// 而这里每条返回都带一份相同的数，窗口函数省下的是一次往返、多付的是每行一个重复值。
// 台账的读频率是"运维查一次"，两条语句的形态更好读，也更不容易在改分页时被改错。
//
// Since/Until 都是闭区间（>= 与 <=），两端都含：运维圈一天的范围时不该纠结边界那条算不算。
func (a *AuditLog) Query(f api.AuditFilter) ([]api.AuditEntry, int, error) {
	where, args := auditWhere(f)

	var total int
	if err := a.db.sqlDB.QueryRow(`SELECT COUNT(*) FROM write_audit`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("sqlite: count audit rows: %w", err)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = auditReadLimitFallback
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	rows, err := a.db.sqlDB.Query(
		`SELECT `+auditColumns+` FROM write_audit`+where+` ORDER BY seq DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("sqlite: query audit rows: %w", err)
	}
	defer rows.Close()

	// 空列表而不是 nil：端点把它序列化出去是 []
	items := make([]api.AuditEntry, 0, min(limit, 256))
	for rows.Next() {
		entry, err := scanAudit(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("sqlite: scan audit row: %w", err)
		}
		items = append(items, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("sqlite: iterate audit rows: %w", err)
	}
	return items, total, nil
}

// auditWhere 把过滤条件拼成参数化谓词。返回的空串表示不带 WHERE。
// 每个值都走语句参数，没有任何拼接进来的字符串。
func auditWhere(f api.AuditFilter) (string, []any) {
	var clauses []string
	var args []any

	if f.Actor != "" {
		clauses = append(clauses, `actor = ?`)
		args = append(args, f.Actor)
	}
	if f.Action != "" {
		clauses = append(clauses, `action = ?`)
		args = append(args, f.Action)
	}
	if f.Verdict != "" {
		clauses = append(clauses, `verdict = ?`)
		args = append(args, f.Verdict)
	}
	if f.Since != nil {
		clauses = append(clauses, `ts_us >= ?`)
		args = append(args, f.Since.UnixMicro())
	}
	if f.Until != nil {
		clauses = append(clauses, `ts_us <= ?`)
		args = append(args, f.Until.UnixMicro())
	}
	if len(clauses) == 0 {
		return "", args
	}
	return ` WHERE ` + strings.Join(clauses, ` AND `), args
}

// scanAudit 还原一行。ts_us 只到微秒，读回来的时间丢原来的纳秒与原始时区（这里是本地时区），
// 与事件表的读回完全同一口径（S04 卡 §9）。
func scanAudit(row rowScanner) (api.AuditEntry, error) {
	var (
		timestampUS int64
		actor       string
		actorKind   string
		role        sql.NullString
		action      string
		method      string
		route       string
		status      int
		latencyUS   int64
		verdict     string
		execVerdict sql.NullString
		execReason  sql.NullString
		handlerKey  sql.NullString
		profile     sql.NullString
		jobID       sql.NullString
		remoteIP    sql.NullString
		userAgent   sql.NullString
	)
	if err := row.Scan(&timestampUS, &actor, &actorKind, &role, &action, &method, &route,
		&status, &latencyUS, &verdict, &execVerdict, &execReason, &handlerKey, &profile,
		&jobID, &remoteIP, &userAgent); err != nil {
		return api.AuditEntry{}, err
	}

	return api.AuditEntry{
		Time:           time.UnixMicro(timestampUS),
		Actor:          actor,
		ActorKind:      actorKind,
		Role:           role.String,
		Action:         action,
		Method:         method,
		Route:          route,
		Status:         status,
		Latency:        time.Duration(latencyUS) * time.Microsecond,
		Verdict:        verdict,
		ExecVerdict:    execVerdict.String,
		ExecReasonCode: execReason.String,
		HandlerKey:     handlerKey.String,
		Profile:        profile.String,
		JobID:          jobID.String,
		RemoteIP:       remoteIP.String,
		UserAgent:      userAgent.String,
	}, nil
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
	var count int64
	if err := a.db.sqlDB.QueryRow(`SELECT COUNT(*) FROM write_audit`).Scan(&count); err != nil {
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

// 编译期把 api 侧声明的两个能力钉在这个类型上：装配方不需要读实现就知道两面都在。
var (
	_ api.AuditRecorder = (*AuditLog)(nil)
	_ api.AuditReader   = (*AuditLog)(nil)
)
