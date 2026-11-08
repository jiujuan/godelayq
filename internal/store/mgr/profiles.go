package mgr

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
)

// profileStore 是 mgr_executor_profiles 表上的 core.ExecutorProfileStore 实现。
// 整条记录序列化进 record_json：字段随 core.ExecutorCommand 演进，整存最稳，
// 也与 JSON 文件格式同构——迁移零转换，新增字段不需要改表结构。
// 语义与 JSONFileExecutorProfileStore 逐条对齐：Save 校验记录、覆盖保留 CreatedAt。
type profileStore struct {
	h *Handle
}

// NewExecutorProfileStore 构造档位存储。低频实体同步落库，不设 Flush/Close。
func NewExecutorProfileStore(h *Handle) core.ExecutorProfileStore {
	return &profileStore{h: h}
}

// 编译期钉住接口实现。
var _ core.ExecutorProfileStore = (*profileStore)(nil)

// List 按档位名字典序返回全部记录。排序用 lower(name)（与 users/groups 同一口径），
// 记录自身的 Name 字段保留用户书写的大小写（与 JSON 存储同一读法）。
func (s *profileStore) List() ([]core.ExecutorProfileRecord, error) {
	var raws []string
	if err := s.h.NewSelect().Model((*profileRow)(nil)).
		ColumnExpr("record_json").
		OrderExpr("lower(name)").
		Scan(bgCtx, &raws); err != nil {
		return nil, err
	}

	out := make([]core.ExecutorProfileRecord, 0, len(raws))
	for _, raw := range raws {
		var record core.ExecutorProfileRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

// Get 忽略大小写查找一条档位。
func (s *profileStore) Get(name string) (core.ExecutorProfileRecord, bool, error) {
	var raw string
	err := s.h.NewSelect().Model((*profileRow)(nil)).
		ColumnExpr("record_json").
		Where("lower(name) = lower(?)", strings.TrimSpace(name)).
		Scan(bgCtx, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ExecutorProfileRecord{}, false, nil
	}
	if err != nil {
		return core.ExecutorProfileRecord{}, false, err
	}
	var record core.ExecutorProfileRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return core.ExecutorProfileRecord{}, false, err
	}
	return record, true, nil
}

// Save 新建或整体覆盖一条档位；已存在时保留其 CreatedAt（除非调用方显式带了值）。
// 入库前过 core.ValidateProfileRecord：与 JSON 存储同一份规则，唯一出处。
func (s *profileStore) Save(record core.ExecutorProfileRecord) error {
	if err := core.ValidateProfileRecord(record); err != nil {
		return err
	}

	record.Name = strings.TrimSpace(record.Name)
	now := time.Now()
	record.UpdatedAt = now
	if record.CreatedAt.IsZero() {
		existing, ok, err := s.Get(record.Name)
		if err != nil {
			return err
		}
		if ok {
			record.CreatedAt = existing.CreatedAt
		} else {
			record.CreatedAt = now
		}
	}

	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}

	_, err = s.h.NewInsert().Model(&profileRow{
		Name:       record.Name,
		RecordJSON: string(payload),
		UpdatedAt:  record.UpdatedAt.Unix(),
	}).
		On(nameConflictClause(s.h.Dialect())).
		Set("record_json = EXCLUDED.record_json").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(bgCtx)
	return err
}

// Delete 忽略大小写删除；不存在返回 core.ErrProfileNotFound。
func (s *profileStore) Delete(name string) error {
	res, err := s.h.NewDelete().Model((*profileRow)(nil)).
		Where("lower(name) = lower(?)", strings.TrimSpace(name)).
		Exec(bgCtx)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return core.ErrProfileNotFound
	}
	return nil
}
