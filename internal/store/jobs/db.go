package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/jiujuan/godelayq/internal/store/engine"
	"github.com/uptrace/bun"
)

// bgCtx 供无 ctx 入参的接口方法内部使用：core.Store 的签名不带 ctx，
// 这里不发明新的传播方式，超时与取消由调用方（调度器/HTTP 层）在更外层控制。
var bgCtx = context.Background()

// Handle 是任务库的连接句柄：公共底座句柄嵌入。库文件级读数（行数、WAL 大小）
// 若日后要进运维端点，挂在本类型上。
type Handle struct {
	*engine.DB
}

// jobRow 是 jobs 表的 bun 模型。表名、列名是冻结契约（schema.go），tag 全部
// 显式给出，不依赖 bun 的命名推断。snapshot_json 是权威数据；其余四列是冗余，
// 只服务于运维排查与留痕淘汰的轻查询。
type jobRow struct {
	bun.BaseModel `bun:"table:jobs"`

	ID           string `bun:"column:id,pk"`
	Name         string `bun:"column:name"`
	Status       string `bun:"column:status"`
	UpdatedAt    int64  `bun:"column:updated_at"`
	SnapshotJSON string `bun:"column:snapshot_json"`
}

// metaRow 是 jobs_meta 表的 bun 模型：一次性导入标记的键值对。
type metaRow struct {
	bun.BaseModel `bun:"table:jobs_meta"`

	Key   string `bun:"column:key,pk"`
	Value string `bun:"column:value"`
}

// Store 是 jobs 表上的 core.Store 实现：直写、每变更一个事务，没有内存缓存
// 与后台合并协程。接口定义在 core（消费方声明能力），这里只提供隐式实现；
// 终态留痕的规则来自 core.TrimTerminal，本包不持有第二份淘汰语义。
type Store struct {
	h *Handle

	// 留痕策略写成原子值而不是普通字段：重载链会在运行期写它们（SetHistoryRetention），
	// 与 core.JSONFileStore 同一取舍。historyTTL 存纳秒。
	historyLimit atomic.Int64
	historyTTL   atomic.Int64

	closeOnce sync.Once
}

// 编译期钉住接口实现：core.Store 的方法集一旦改动，这里立刻报错。
var _ core.Store = (*Store)(nil)

// Open 建好父目录、打开连接、设置 PRAGMA、建表并应用任务库迁移。
// historyLimit/historyTTL/slowQueryThreshold 的取值口径与 core.StoreConfig 一条
// （limit==0 用 DefaultHistoryLimit、limit<0 不留痕、ttl<=0 不按时间淘汰、
// slowQuery<=0 不挂慢查询钩子），由装配方从 store.history_limit/history_ttl/
// slow_query_threshold 原样传入。Path/BusyTimeout/Synchronous 的
// 空值已在 core.Config.Normalized() 折成各自默认，这里不再兜底。
//
// 打不开任务库等于快照无处安放，任何一步失败都返回错误，由装配方拒绝启动——
// 与管理库、观测库同一口径。
func Open(cfg core.StoreSQLiteConfig, historyLimit int, historyTTL time.Duration,
	slowQueryThreshold time.Duration, logger *slog.Logger) (*Store, error) {
	db, err := engine.Open(engine.Options{
		Dialect:     engine.Dialect(cfg.Dialect),
		Path:        cfg.Path,
		DSN:         cfg.DSN,
		BusyTimeout: cfg.BusyTimeout,
		Synchronous: cfg.Synchronous,
		// 单连接：调度器是唯一的写方（API 的写都经它），读只有启动恢复与
		// 留痕轻查询；与观测库、管理库同一取舍，消除 SQLITE_BUSY 的主要来源。
		MaxOpenConns:       1,
		SlowQueryThreshold: slowQueryThreshold,
	}, logger)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(migrationTableName, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{h: &Handle{DB: db}}
	// 原子字段不能进结构体字面量，所以建好体之后一次性 Store（与 JSON 实现同构）。
	limit, ttl := historyRetentionValues(historyLimit, historyTTL)
	s.historyLimit.Store(limit)
	s.historyTTL.Store(ttl)
	return s, nil
}

// historyRetentionValues 把留痕的条数与时长折成存储取值（条数、纳秒）。
// 与 core.historyRetentionValues 的口径逐字一致——那边是私有函数，这里的这份
// 复制是跨包可见性的代价；两边由同一组测试钉住，改规则要一起改。
func historyRetentionValues(limit int, ttl time.Duration) (int64, int64) {
	if limit == 0 {
		limit = core.DefaultHistoryLimit
	}
	return int64(limit), int64(ttl)
}

// SetHistoryRetention 运行期调整终态快照的保留条数与时长。两项必须一起给，
// 取值口径与 StoreOptions 同一条；生效时机是下一次终态写入触发的 trim。
func (s *Store) SetHistoryRetention(limit int, ttl time.Duration) {
	storedLimit, storedTTL := historyRetentionValues(limit, ttl)
	s.historyLimit.Store(storedLimit)
	s.historyTTL.Store(storedTTL)
}

// DBRuntime 返回任务库的机制性读数（G4/Q05），/admin/runtime 的 db 段数据源。
// 接口断言注入（cmd/server 的 defaultNewServer），JSON 部署没有这个方法，
// db 段随之整键缺省。全部是瞬时值。
// 失败口径：任何一项查询失败都整体报错、不返回半截读数——SchemaVersion=0
// 与"迁移表读不出来"在数字上无法区分，误读成空库比缺席更糟（缺席 + error 日志
// 至少把故障摆到眼前）。
func (s *Store) DBRuntime() (core.DBRuntimeStats, error) {
	st := s.h.Stats()
	out := core.DBRuntimeStats{
		Dialect: string(s.h.Dialect()),
		Pool: core.DBPoolStats{
			MaxOpen:        st.MaxOpenConnections,
			Open:           st.OpenConnections,
			InUse:          st.InUse,
			Idle:           st.Idle,
			WaitCount:      st.WaitCount,
			WaitDurationNs: int64(st.WaitDuration),
		},
	}
	if v, err := s.h.MigrationVersion(migrationTableName); err == nil {
		out.SchemaVersion = v
	} else {
		return out, fmt.Errorf("jobs: read schema version for runtime stats: %w", err)
	}
	pending, err := pendingOutboxCount(s.h)
	if err != nil {
		return out, fmt.Errorf("jobs: read outbox backlog for runtime stats: %w", err)
	}
	out.OutboxPending = pending
	return out, nil
}

// Save 落一条快照并按需触发终态留痕。直写：成功返回即已持久化。
func (s *Store) Save(job *core.Job) error {
	if job == nil {
		return fmt.Errorf("jobs: save nil job")
	}
	return s.upsert(job.ToSnapshot())
}

// Update 同 Save，只是入参已经是快照形态。
func (s *Store) Update(snapshot core.JobSnapshot) error {
	return s.upsert(snapshot)
}

func (s *Store) upsert(snapshot core.JobSnapshot) error {
	if err := upsertSnapshot(s.h.DB.DB, snapshot); err != nil {
		return err
	}
	if core.JobStatus(snapshot.Status).IsTerminal() {
		return s.trimTerminal()
	}
	return nil
}

// upsertSnapshot 把快照 upsert 进 jobs 表。db 抽象掉句柄与事务：直写路径传句柄，
// UpdateWithEvents（outbox.go）传事务——两处必须共享同一条冲突更新链。
func upsertSnapshot(db bun.IDB, snapshot core.JobSnapshot) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("jobs: marshal snapshot %s: %w", snapshot.ID, err)
	}
	row := &jobRow{
		ID:           snapshot.ID,
		Name:         snapshot.Name,
		Status:       core.JobStatus(snapshot.Status).String(),
		UpdatedAt:    snapshot.UpdatedAt.UnixNano(),
		SnapshotJSON: string(payload),
	}
	if _, err := db.NewInsert().Model(row).
		On("CONFLICT (id) DO UPDATE").
		Set("name = EXCLUDED.name").
		Set("status = EXCLUDED.status").
		Set("updated_at = EXCLUDED.updated_at").
		Set("snapshot_json = EXCLUDED.snapshot_json").
		Exec(bgCtx); err != nil {
		return fmt.Errorf("jobs: upsert %s: %w", snapshot.ID, err)
	}
	return nil
}

// Delete 移除一条快照。ID 不存在不是错误：调用方（调度器的取消链）语义是
// "确保不在"，与 JSON 实现的 delete-缺失-无害一致。
// 账本行随任务一起删（TASK-RE02）：接口语义是"这条任务不存在了"。
func (s *Store) Delete(jobID string) error {
	if err := s.h.RunInTx(bgCtx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*jobRow)(nil)).
			Where("id = ?", jobID).
			Exec(ctx); err != nil {
			return fmt.Errorf("jobs: delete %s: %w", jobID, err)
		}
		if _, err := tx.NewDelete().Model((*attemptRow)(nil)).
			Where("job_id = ?", jobID).
			Exec(ctx); err != nil {
			return fmt.Errorf("jobs: delete attempts %s: %w", jobID, err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// LoadAll 返回全部快照，含终态留痕；按状态过滤是调用方的事（接口契约）。
func (s *Store) LoadAll() ([]core.JobSnapshot, error) {
	var payloads []string
	if err := s.h.NewSelect().Model((*jobRow)(nil)).
		ColumnExpr("snapshot_json").
		Scan(bgCtx, &payloads); err != nil {
		return nil, fmt.Errorf("jobs: load all: %w", err)
	}

	out := make([]core.JobSnapshot, 0, len(payloads))
	for _, payload := range payloads {
		var snap core.JobSnapshot
		if err := json.Unmarshal([]byte(payload), &snap); err != nil {
			return nil, fmt.Errorf("jobs: unmarshal snapshot: %w", err)
		}
		out = append(out, snap)
	}
	return out, nil
}

// Flush 直写下没有"未落盘的内存态"，恒为 no-op。接口契约里"无变更时不产生
// 写入"在这里恒真。
func (s *Store) Flush() error { return nil }

// Close 关闭连接，可安全重复调用。直写下没有收尾落盘这个环节。
func (s *Store) Close() error {
	s.closeOnce.Do(func() {})
	return s.h.Close()
}

// trimTerminal 执行终态留痕淘汰。轻查询只取判定需要的三个字段，淘汰规则
// 交给 core.TrimTerminal（用只填 ID/Status/UpdatedAt 的部分快照构造入参——
// 那个函数只读这三个字段）；行数受留痕上限约束（默认 1000），O(n) 判定
// 不值得在 SQL 端复刻一份语义。
func (s *Store) trimTerminal() error {
	return s.h.RunInTx(bgCtx, nil, func(ctx context.Context, tx bun.Tx) error {
		return trimTerminalTx(ctx, tx, s.historyLimit.Load(), time.Duration(s.historyTTL.Load()))
	})
}

// trimTerminalTx 在给定事务里执行终态留痕淘汰（含账本级联删除）。
// 独立成函数是因为 UpdateWithEvents（outbox.go）要在自己的事务里复用同一份
// 语义——淘汰规则只许有一处，事务边界由调用方决定。
func trimTerminalTx(ctx context.Context, tx bun.Tx, historyLimit int64, historyTTL time.Duration) error {
	var rows []jobRow
	if err := tx.NewSelect().Model(&rows).
		ColumnExpr("id, status, updated_at").
		Scan(ctx); err != nil {
		return fmt.Errorf("jobs: trim scan: %w", err)
	}
	views := make(map[string]core.JobSnapshot, len(rows))
	for _, row := range rows {
		status, ok := core.ParseJobStatus(row.Status)
		if !ok {
			return fmt.Errorf("jobs: trim scan: unknown status %q in row %s", row.Status, row.ID)
		}
		views[row.ID] = core.JobSnapshot{ID: row.ID, Status: int(status), UpdatedAt: time.Unix(0, row.UpdatedAt)}
	}

	stale := core.TrimTerminal(views, historyLimit, historyTTL)
	if len(stale) == 0 {
		return nil
	}
	for _, id := range stale {
		if _, execErr := tx.NewDelete().Model((*jobRow)(nil)).
			Where("id = ?", id).
			Exec(ctx); execErr != nil {
			return fmt.Errorf("jobs: trim delete %s: %w", id, execErr)
		}
	}
	// 账本随任务走（TASK-RE02）：任务快照被终态留痕淘汰，它的逐次尝试行
	// 一起删——留着一堆"任务已不存在"的账本行只会在读端点制造 404 悬案。
	// 与快照删除同事务：不存在"快照没了账本还在"的中间态。
	if _, execErr := tx.NewDelete().Model((*attemptRow)(nil)).
		Where("job_id IN (?)", bun.In(stale)).
		Exec(ctx); execErr != nil {
		return fmt.Errorf("jobs: trim delete attempts: %w", execErr)
	}
	return nil
}
