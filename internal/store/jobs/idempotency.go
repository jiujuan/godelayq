package jobs

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/uptrace/bun"
)

// idempotency.go 实现幂等提交的登记表（TASK-RE03）：job_idempotency 表上的
// 键 → (job_id, fingerprint) 映射，供 api 层在 Schedule 之前查重。接口形状由
// 消费方（api）声明，这里只提供隐式实现；快照按 ID 直查（SnapshotReader）也在本文件。

// idempotencyRow 是 job_idempotency 表的 bun 模型。表名、列名是冻结契约
//（schema.go 迁移 v3），tag 全部显式给出。
type idempotencyRow struct {
	bun.BaseModel `bun:"table:job_idempotency"`

	Key         string `bun:"column:key,pk"`
	JobID       string `bun:"column:job_id"`
	Fingerprint string `bun:"column:fingerprint"`
	CreatedAt   int64  `bun:"column:created_at"`
}

// 编译期钉住接口实现：core.SnapshotReader 的方法集一旦改动，这里立刻报错。
var _ core.SnapshotReader = (*Store)(nil)

// Get 按 ID 直查快照（TASK-RE03）：主键查，替代调度器曾经的 LoadAll 全量扫描。
func (s *Store) Get(jobID string) (core.JobSnapshot, bool, error) {
	var payloads []string
	if err := s.h.NewSelect().Model((*jobRow)(nil)).
		ColumnExpr("snapshot_json").
		Where("id = ?", jobID).
		Limit(1).
		Scan(bgCtx, &payloads); err != nil {
		return core.JobSnapshot{}, false, fmt.Errorf("jobs: get %s: %w", jobID, err)
	}
	if len(payloads) == 0 {
		return core.JobSnapshot{}, false, nil
	}
	var snap core.JobSnapshot
	if err := json.Unmarshal([]byte(payloads[0]), &snap); err != nil {
		return core.JobSnapshot{}, false, fmt.Errorf("jobs: unmarshal snapshot %s: %w", jobID, err)
	}
	return snap, true, nil
}

// LookupIdempotency 查一个幂等键的登记：found=false 表示键未占用。
func (s *Store) LookupIdempotency(key string) (string, string, bool, error) {
	var rows []idempotencyRow
	if err := s.h.NewSelect().Model(&rows).
		Where("key = ?", key).
		Limit(1).
		Scan(bgCtx); err != nil {
		return "", "", false, fmt.Errorf("jobs: lookup idempotency key: %w", err)
	}
	if len(rows) == 0 {
		return "", "", false, nil
	}
	return rows[0].JobID, rows[0].Fingerprint, true, nil
}

// PutIdempotency 登记一个幂等键。先到先得（ON CONFLICT DO NOTHING）：
// 键一旦绑定某个任务就不再改写——并发首提交都通过查重的窗口里（登记与创建
// 尚未同事务，TASK-RE07 收口），让先到者赢比让后到者覆盖更接近幂等语义。
func (s *Store) PutIdempotency(key string, jobID string, fingerprint string) error {
	row := &idempotencyRow{Key: key, JobID: jobID, Fingerprint: fingerprint, CreatedAt: time.Now().UnixNano()}
	if _, err := s.h.NewInsert().Model(row).
		On("CONFLICT (key) DO NOTHING").
		Exec(bgCtx); err != nil {
		return fmt.Errorf("jobs: put idempotency key: %w", err)
	}
	return nil
}

// PruneExpiredIdempotency 淘汰超过 ttl 的登记。由 api 层在每次成功创建后驱动
//（带索引的删除，代价常数）；没有后台协程。
func (s *Store) PruneExpiredIdempotency(ttl time.Duration) error {
	if ttl <= 0 {
		ttl = core.DefaultIdempotencyTTL
	}
	cutoff := time.Now().Add(-ttl).UnixNano()
	if _, err := s.h.NewDelete().Model((*idempotencyRow)(nil)).
		Where("created_at < ?", cutoff).
		Exec(bgCtx); err != nil {
		return fmt.Errorf("jobs: prune expired idempotency keys: %w", err)
	}
	return nil
}
