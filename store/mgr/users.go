package mgr

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"godelayq/core"
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

// List 按名字字典序返回全部账号（NOCASE 排序，大小写变体相邻）。
func (s *userStore) List() ([]core.UserRecord, error) {
	rows, err := s.h.SQL().Query(`SELECT name, password_bcrypt, role, created_at, updated_at
FROM mgr_users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]core.UserRecord, 0, 8)
	for rows.Next() {
		rec, scanErr := scanUser(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Get 按名字查找（NOCASE 列，大小写不敏感命中）。
func (s *userStore) Get(name string) (core.UserRecord, bool, error) {
	row := s.h.SQL().QueryRow(`SELECT name, password_bcrypt, role, created_at, updated_at
FROM mgr_users WHERE name = ?`, strings.TrimSpace(name))
	rec, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.UserRecord{}, false, nil
	}
	if err != nil {
		return core.UserRecord{}, false, err
	}
	return rec, true, nil
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

	_, err := s.h.SQL().Exec(`INSERT INTO mgr_users (name, password_bcrypt, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
  password_bcrypt = excluded.password_bcrypt,
  role            = excluded.role,
  updated_at      = excluded.updated_at`,
		record.Name, record.PasswordBcrypt, record.Role,
		record.CreatedAt.Unix(), record.UpdatedAt.Unix())
	return err
}

// Delete 忽略大小写删除；不存在返回 core.ErrUserNotFound。
func (s *userStore) Delete(name string) error {
	res, err := s.h.SQL().Exec(`DELETE FROM mgr_users WHERE name = ?`, strings.TrimSpace(name))
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
	var count int
	err := s.h.SQL().QueryRow(`SELECT COUNT(*) FROM mgr_users`).Scan(&count)
	return count, err
}

// scanUser 把一行读成 UserRecord。rowScanner 同时覆盖 *sql.Row 与 *sql.Rows。
func scanUser(row rowScanner) (core.UserRecord, error) {
	var rec core.UserRecord
	var created, updated int64
	if err := row.Scan(&rec.Name, &rec.PasswordBcrypt, &rec.Role, &created, &updated); err != nil {
		return core.UserRecord{}, err
	}
	rec.CreatedAt = time.Unix(created, 0)
	rec.UpdatedAt = time.Unix(updated, 0)
	return rec, nil
}

// rowScanner 是 *sql.Row 与 *sql.Rows 的公共读面，与 observe 包同一手法。
type rowScanner interface {
	Scan(dest ...any) error
}
