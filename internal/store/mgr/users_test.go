package mgr

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jiujuan/godelayq/internal/core"
	"golang.org/x/crypto/bcrypt"
)

// 打开一份指向临时目录的管理库。哈希用测试常量：本文件只测存储语义，
// 哈希格式与 cost 的校验由 import_test.go 与 api 层覆盖。
func openMgr(t *testing.T) *Handle {
	t.Helper()
	h, err := Open(core.MgrStoreConfig{
		Enabled: true,
		Path:    filepath.Join(t.TempDir(), "mgr.sqlite"),
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// testHash 是一条 shape 合法的占位哈希（cost 10 的 bcrypt），
// 生成一次、包内共享——测试不测 bcrypt 本身。
var testHash = func() string {
	// cost 10 一条约 60-100ms，包内只算一次
	h, _ := bcrypt.GenerateFromPassword([]byte("mgr-test-password"), 10)
	return string(h)
}()

func TestUserStore_CRUD(t *testing.T) {
	h := openMgr(t)
	store := NewUserStore(h)

	rec := core.UserRecord{Name: "alice", PasswordBcrypt: testHash, Role: "admin"}
	require.NoError(t, store.Save(rec))

	got, ok, err := store.Get("alice")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "alice", got.Name)
	assert.Equal(t, "admin", got.Role)
	assert.Equal(t, testHash, got.PasswordBcrypt)
	assert.False(t, got.CreatedAt.IsZero())
	assert.False(t, got.UpdatedAt.IsZero())

	// 大小写不敏感命中（与配置期查重口径一致）
	got, ok, err = store.Get("ALICE")
	require.NoError(t, err)
	assert.True(t, ok)

	// 更新：角色可改，CreatedAt 保留
	rec.Role = "viewer"
	require.NoError(t, store.Save(rec))
	got, _, err = store.Get("alice")
	require.NoError(t, err)
	assert.Equal(t, "viewer", got.Role)
	first, _, err := store.Get("alice")
	require.NoError(t, err)
	assert.Equal(t, got.CreatedAt, first.CreatedAt)

	// List 按名字字典序
	require.NoError(t, store.Save(core.UserRecord{Name: "bob", PasswordBcrypt: testHash, Role: "viewer"}))
	items, err := store.List()
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "alice", items[0].Name)
	assert.Equal(t, "bob", items[1].Name)

	count, err := store.Count()
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	// Delete：存在删掉，不存在返回 ErrUserNotFound
	require.NoError(t, store.Delete("BOB"))
	err = store.Delete("bob")
	require.True(t, errors.Is(err, core.ErrUserNotFound))

	_, ok, err = store.Get("alice")
	require.NoError(t, err)
	assert.True(t, ok, "删除 bob 不该影响 alice")
}

func TestUserStore_SaveRejectsInvalidRecords(t *testing.T) {
	h := openMgr(t)
	store := NewUserStore(h)

	cases := map[string]core.UserRecord{
		"空名字":  {Name: "  ", PasswordBcrypt: testHash, Role: "admin"},
		"空哈希":  {Name: "x", PasswordBcrypt: "", Role: "admin"},
		"非法角色": {Name: "x", PasswordBcrypt: testHash, Role: "machine"},
		"未知角色": {Name: "x", PasswordBcrypt: testHash, Role: "superuser"},
	}
	for name, rec := range cases {
		err := store.Save(rec)
		require.Error(t, err, name)
	}
}

func TestUserStore_ReopenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mgr.sqlite")
	cfg := core.MgrStoreConfig{Enabled: true, Path: path}

	h1, err := Open(cfg, nil)
	require.NoError(t, err)
	require.NoError(t, NewUserStore(h1).Save(core.UserRecord{Name: "carol", PasswordBcrypt: testHash, Role: "ops"}))
	require.NoError(t, h1.Close())

	// 重开：迁移幂等，数据还在
	h2, err := Open(cfg, nil)
	require.NoError(t, err)
	defer h2.Close()
	rec, ok, err := NewUserStore(h2).Get("carol")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ops", rec.Role)
}
