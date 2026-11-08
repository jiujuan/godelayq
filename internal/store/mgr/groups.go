package mgr

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
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

// List 按名称字典序返回全部分组（lower(name) 排序，与 users.List 同一口径）。
func (s *groupStore) List() ([]core.Group, error) {
	var rows []groupRow
	if err := s.h.NewSelect().Model(&rows).OrderExpr("lower(name)").Scan(bgCtx); err != nil {
		return nil, err
	}

	out := make([]core.Group, 0, len(rows))
	for _, row := range rows {
		out = append(out, groupOf(row))
	}
	return out, nil
}

// Get 忽略大小写查找一个分组。
func (s *groupStore) Get(name string) (core.Group, bool, error) {
	var row groupRow
	err := s.h.NewSelect().Model(&row).
		Where("lower(name) = lower(?)", strings.TrimSpace(name)).
		Scan(bgCtx)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Group{}, false, nil
	}
	if err != nil {
		return core.Group{}, false, err
	}
	return groupOf(row), true, nil
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

	_, err := s.h.NewInsert().Model(&groupRow{
		Name:        group.Name,
		Description: group.Description,
		Color:       group.Color,
		CreatedAt:   group.CreatedAt.Unix(),
		UpdatedAt:   group.UpdatedAt.Unix(),
	}).
		On(nameConflictClause(s.h.Dialect())).
		Set("description = EXCLUDED.description").
		Set("color = EXCLUDED.color").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(bgCtx)
	return err
}

// Delete 忽略大小写删除；不存在返回 core.ErrGroupNotFound。
func (s *groupStore) Delete(name string) error {
	res, err := s.h.NewDelete().Model((*groupRow)(nil)).
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
		return core.ErrGroupNotFound
	}
	return nil
}

// groupOf 把一行 bun 模型读成 core.Group。时间戳一律 Unix 秒。
func groupOf(row groupRow) core.Group {
	return core.Group{
		Name:        row.Name,
		Description: row.Description,
		Color:       row.Color,
		CreatedAt:   time.Unix(row.CreatedAt, 0),
		UpdatedAt:   time.Unix(row.UpdatedAt, 0),
	}
}
