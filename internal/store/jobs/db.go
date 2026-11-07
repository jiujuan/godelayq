package jobs

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/jiujuan/godelayq/internal/store/sqlite"
)

// Handle 是任务库的连接句柄：公共底座句柄嵌入。库文件级读数（行数、WAL 大小）
// 若日后要进运维端点，挂在本类型上。
type Handle struct {
	*sqlite.DB
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
// historyLimit/historyTTL 的取值口径与 core.StoreOptions 一条（limit==0 用
// DefaultHistoryLimit、limit<0 不留痕、ttl<=0 不按时间淘汰），由装配方从
// store.history_limit/history_ttl 原样传入。Path/BusyTimeout/Synchronous 的
// 空值已在 core.Config.Normalized() 折成各自默认，这里不再兜底。
//
// 打不开任务库等于快照无处安放，任何一步失败都返回错误，由装配方拒绝启动——
// 与管理库、观测库同一口径。
func Open(cfg core.StoreSQLiteConfig, historyLimit int, historyTTL time.Duration, logger *slog.Logger) (*Store, error) {
	db, err := sqlite.OpenFile(sqlite.Options{
		Path:        cfg.Path,
		BusyTimeout: cfg.BusyTimeout,
		Synchronous: cfg.Synchronous,
		// 单连接：调度器是唯一的写方（API 的写都经它），读只有启动恢复与
		// 留痕轻查询；与观测库、管理库同一取舍，消除 SQLITE_BUSY 的主要来源。
		MaxOpenConns: 1,
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
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("jobs: marshal snapshot %s: %w", snapshot.ID, err)
	}
	if _, err := s.h.SQL().Exec(`INSERT INTO jobs (id, name, status, updated_at, snapshot_json)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name = excluded.name,
  status = excluded.status,
  updated_at = excluded.updated_at,
  snapshot_json = excluded.snapshot_json`,
		snapshot.ID, snapshot.Name, core.JobStatus(snapshot.Status).String(),
		snapshot.UpdatedAt.UnixNano(), string(payload)); err != nil {
		return fmt.Errorf("jobs: upsert %s: %w", snapshot.ID, err)
	}
	if core.JobStatus(snapshot.Status).IsTerminal() {
		return s.trimTerminal()
	}
	return nil
}

// Delete 移除一条快照。ID 不存在不是错误：调用方（调度器的取消链）语义是
// "确保不在"，与 JSON 实现的 delete-缺失-无害一致。
func (s *Store) Delete(jobID string) error {
	if _, err := s.h.SQL().Exec(`DELETE FROM jobs WHERE id = ?`, jobID); err != nil {
		return fmt.Errorf("jobs: delete %s: %w", jobID, err)
	}
	return nil
}

// LoadAll 返回全部快照，含终态留痕；按状态过滤是调用方的事（接口契约）。
func (s *Store) LoadAll() ([]core.JobSnapshot, error) {
	rows, err := s.h.SQL().Query(`SELECT snapshot_json FROM jobs`)
	if err != nil {
		return nil, fmt.Errorf("jobs: load all: %w", err)
	}
	defer rows.Close()

	out := make([]core.JobSnapshot, 0, 16)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("jobs: scan snapshot: %w", err)
		}
		var snap core.JobSnapshot
		if err := json.Unmarshal([]byte(payload), &snap); err != nil {
			return nil, fmt.Errorf("jobs: unmarshal snapshot: %w", err)
		}
		out = append(out, snap)
	}
	return out, rows.Err()
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
	rows, err := s.h.SQL().Query(`SELECT id, status, updated_at FROM jobs`)
	if err != nil {
		return fmt.Errorf("jobs: trim scan: %w", err)
	}
	views := make(map[string]core.JobSnapshot)
	for rows.Next() {
		var id, statusName string
		var updatedNano int64
		if scanErr := rows.Scan(&id, &statusName, &updatedNano); scanErr != nil {
			rows.Close()
			return fmt.Errorf("jobs: trim scan row: %w", scanErr)
		}
		status, ok := core.ParseJobStatus(statusName)
		if !ok {
			rows.Close()
			return fmt.Errorf("jobs: trim scan: unknown status %q in row %s", statusName, id)
		}
		views[id] = core.JobSnapshot{ID: id, Status: int(status), UpdatedAt: time.Unix(0, updatedNano)}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("jobs: trim scan: %w", err)
	}
	rows.Close()

	stale := core.TrimTerminal(views, s.historyLimit.Load(), time.Duration(s.historyTTL.Load()))
	if len(stale) == 0 {
		return nil
	}
	tx, err := s.h.SQL().Begin()
	if err != nil {
		return fmt.Errorf("jobs: trim begin: %w", err)
	}
	for _, id := range stale {
		if _, execErr := tx.Exec(`DELETE FROM jobs WHERE id = ?`, id); execErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("jobs: trim delete %s: %w", id, execErr)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("jobs: trim commit: %w", err)
	}
	return nil
}
