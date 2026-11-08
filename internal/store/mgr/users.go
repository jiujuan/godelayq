package mgr

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
)

// userStore 是 mgr_users 表上的 core.UserStore 实现。
// 接口定义在 core（消费方声明能力），这里只提供隐式实现。
type userStore struct {
	h *Handle
}

// NewUserStore 构造账号存储。接口形状与 GroupStore/ExecutorProfileStore 同一取向：
// 每次改动同步落库，没有后台合并协程需要收尾，因此不设 Flush/Close。
func NewUserStore(h *Handle) core.UserStore {
	return &userStore{h: h}
}

// 编译期钉住接口实现：core.UserStore 的方法集一旦改动，这里立刻报错。
var _ core.UserStore = (*userStore)(nil)

// List 按名字字典序返回全部账号（lower(name) 排序：SQLite 上与 NOCASE 列同为
// ASCII 折叠，大小写变体相邻；PG 上与 lower(name) 唯一索引同一口径）。
func (s *userStore) List() ([]core.UserRecord, error) {
	var rows []userRow
	if err := s.h.NewSelect().Model(&rows).OrderExpr("lower(name)").Scan(bgCtx); err != nil {
		return nil, err
	}

	out := make([]core.UserRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, userRecordOf(row))
	}
	return out, nil
}

// Get 按名字查找（lower(name) 命中，大小写不敏感）。
func (s *userStore) Get(name string) (core.UserRecord, bool, error) {
	var row userRow
	err := s.h.NewSelect().Model(&row).
		Where("lower(name) = lower(?)", strings.TrimSpace(name)).
		Scan(bgCtx)
	if errors.Is(err, sql.ErrNoRows) {
		return core.UserRecord{}, false, nil
	}
	if err != nil {
		return core.UserRecord{}, false, err
	}
	return userRecordOf(row), true, nil
}

// Save 新建或整体覆盖一个账号；已存在时保留其 CreatedAt（除非调用方显式带了值，
// 与 JSON 存储同一口径）。入库前过 core.ValidateUserRecord：规则的唯一出处。
func (s *userStore) Save(record core.UserRecord) error {
	if err := core.ValidateUserRecord(record); err != nil {
		return err
	}

	record.Name = strings.TrimSpace(record.Name)
	now := time.Now()
	record.UpdatedAt = now
	if record.CreatedAt.IsZero() {
		// 覆盖式更新不该把创建时间冲掉，除非调用方显式带了值
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

	_, err := s.h.NewInsert().Model(&userRow{
		Name:           record.Name,
		PasswordBcrypt: record.PasswordBcrypt,
		Role:           record.Role,
		CreatedAt:      record.CreatedAt.Unix(),
		UpdatedAt:      record.UpdatedAt.Unix(),
	}).
		On(nameConflictClause(s.h.Dialect())).
		Set("password_bcrypt = EXCLUDED.password_bcrypt").
		Set("role = EXCLUDED.role").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(bgCtx)
	return err
}

// Delete 忽略大小写删除；不存在返回 core.ErrUserNotFound。
func (s *userStore) Delete(name string) error {
	res, err := s.h.NewDelete().Model((*userRow)(nil)).
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
		return core.ErrUserNotFound
	}
	return nil
}

// Count 返回账号总数。
func (s *userStore) Count() (int, error) {
	n, err := s.h.NewSelect().Model((*userRow)(nil)).Count(bgCtx)
	return int(n), err
}

// userRecordOf 把一行 bun 模型读成 UserRecord。时间戳一律 Unix 秒。
func userRecordOf(row userRow) core.UserRecord {
	return core.UserRecord{
		Name:           row.Name,
		PasswordBcrypt: row.PasswordBcrypt,
		Role:           row.Role,
		CreatedAt:      time.Unix(row.CreatedAt, 0),
		UpdatedAt:      time.Unix(row.UpdatedAt, 0),
	}
}
