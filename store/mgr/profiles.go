package mgr

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"godelayq/core"
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

// List 按档位名字典序返回全部记录。排序在 SQL 里用 NOCASE 列，
// 记录自身的 Name 字段保留用户书写的大小写（与 JSON 存储同一读法）。
func (s *profileStore) List() ([]core.ExecutorProfileRecord, error) {
	rows, err := s.h.SQL().Query(`SELECT record_json FROM mgr_executor_profiles
ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]core.ExecutorProfileRecord, 0, 8)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var record core.ExecutorProfileRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// Get 忽略大小写查找一条档位。
func (s *profileStore) Get(name string) (core.ExecutorProfileRecord, bool, error) {
	row := s.h.SQL().QueryRow(`SELECT record_json FROM mgr_executor_profiles
WHERE name = ?`, strings.TrimSpace(name))
	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.ExecutorProfileRecord{}, false, nil
		}
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

	_, err = s.h.SQL().Exec(`INSERT INTO mgr_executor_profiles (name, record_json, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  record_json = excluded.record_json,
  updated_at  = excluded.updated_at`,
		record.Name, string(payload), record.UpdatedAt.Unix())
	return err
}

// Delete 忽略大小写删除；不存在返回 core.ErrProfileNotFound。
func (s *profileStore) Delete(name string) error {
	res, err := s.h.SQL().Exec(`DELETE FROM mgr_executor_profiles WHERE name = ?`, strings.TrimSpace(name))
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
