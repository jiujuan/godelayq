package mgr

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"godelayq/core"
)

// groupStore 是 mgr_groups 表上的 core.GroupStore 实现。
// 语义与 JSONFileGroupStore 逐条对齐：Save 校验名字与颜色、覆盖保留 CreatedAt
// （除非调用方显式带了值）、Delete 对不存在的名字返回 ErrGroupNotFound。
type groupStore struct {
	h *Handle
}

// NewGroupStore 构造分组存储。低频实体同步落库，不设 Flush/Close。
func NewGroupStore(h *Handle) core.GroupStore {
	return &groupStore{h: h}
}

// 编译期钉住接口实现。
var _ core.GroupStore = (*groupStore)(nil)

// List 按名称字典序返回全部分组。
func (s *groupStore) List() ([]core.Group, error) {
	rows, err := s.h.SQL().Query(`SELECT name, description, color, created_at, updated_at
FROM mgr_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]core.Group, 0, 8)
	for rows.Next() {
		group, scanErr := scanGroup(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, group)
	}
	return out, rows.Err()
}

// Get 忽略大小写查找一个分组。
func (s *groupStore) Get(name string) (core.Group, bool, error) {
	row := s.h.SQL().QueryRow(`SELECT name, description, color, created_at, updated_at
FROM mgr_groups WHERE name = ?`, strings.TrimSpace(name))
	group, err := scanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Group{}, false, nil
	}
	if err != nil {
		return core.Group{}, false, err
	}
	return group, true, nil
}

// Save 新建或整体覆盖一个分组；已存在时保留其 CreatedAt（除非调用方显式带了值）。
func (s *groupStore) Save(group core.Group) error {
	if err := core.ValidateGroupName(group.Name); err != nil {
		return err
	}
	// 颜色给前端色板用，格式错了会渲染成不可见的样式，宁可在写入时就拒绝
	if err := core.ValidateGroupColor(group.Color); err != nil {
		return err
	}

	group.Name = strings.TrimSpace(group.Name)
	now := time.Now()
	group.UpdatedAt = now
	if group.CreatedAt.IsZero() {
		existing, ok, err := s.Get(group.Name)
		if err != nil {
			return err
		}
		if ok {
			group.CreatedAt = existing.CreatedAt
		} else {
			group.CreatedAt = now
		}
	}

	_, err := s.h.SQL().Exec(`INSERT INTO mgr_groups (name, description, color, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  description = excluded.description,
  color       = excluded.color,
  updated_at  = excluded.updated_at`,
		group.Name, group.Description, group.Color,
		group.CreatedAt.Unix(), group.UpdatedAt.Unix())
	return err
}

// Delete 忽略大小写删除；不存在返回 core.ErrGroupNotFound。
func (s *groupStore) Delete(name string) error {
	res, err := s.h.SQL().Exec(`DELETE FROM mgr_groups WHERE name = ?`, strings.TrimSpace(name))
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return core.ErrGroupNotFound
	}
	return nil
}

// scanGroup 把一行读成 core.Group。
func scanGroup(row rowScanner) (core.Group, error) {
	var group core.Group
	var created, updated int64
	if err := row.Scan(&group.Name, &group.Description, &group.Color, &created, &updated); err != nil {
		return core.Group{}, err
	}
	group.CreatedAt = time.Unix(created, 0)
	group.UpdatedAt = time.Unix(updated, 0)
	return group, nil
}
