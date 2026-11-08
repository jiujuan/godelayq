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

// mgrStoreFactory 返回一次测试用的管理库配置节。同一测试内多次调用必须拿到
// 同一个库（Reopen 用例依赖这一点）。
type mgrStoreFactory func(t *testing.T) core.MgrStoreConfig

// mgrOpener 开一个管理库句柄并登记关闭；断言函数一律经它拿句柄，
// 同一批断言才能在 SQLite 与 PostgreSQL（B06 挂载）两个后端上复用。
type mgrOpener func(t *testing.T) *Handle

// runWithMgrBackends 把同一批断言跑在每个可用后端上。
// SQLite 恒跑；PostgreSQL 后端自 B06 起经 GODELAYQ_TEST_PG_DSN 挂载。
func runWithMgrBackends(t *testing.T, run func(t *testing.T, open mgrOpener, factory mgrStoreFactory)) {
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mgr.sqlite")
		factory := func(*testing.T) core.MgrStoreConfig {
			return core.MgrStoreConfig{Enabled: true, Path: path}
		}
		run(t, openWithFactory(factory), factory)
	})
}

// openWithFactory 把配置节工厂包成句柄 opener。
func openWithFactory(factory mgrStoreFactory) mgrOpener {
	return func(t *testing.T) *Handle {
		t.Helper()
		h, err := Open(factory(t), nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
}

// testHash 是一条 shape 合法的占位哈希（cost 10 的 bcrypt），
// 生成一次、包内共享——测试不测 bcrypt 本身。
var testHash = func() string {
	// cost 10 一条约 60-100ms，包内只算一次
	h, _ := bcrypt.GenerateFromPassword([]byte("mgr-test-password"), 10)
	return string(h)
}()

func TestUserStore_CRUD(t *testing.T) {
	runWithMgrBackends(t, testUserStoreCRUD)
}

func testUserStoreCRUD(t *testing.T, open mgrOpener, _ mgrStoreFactory) {
	h := open(t)
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

// TestUserStore_CaseVariantSemantics 把"大小写变体视作同一实体"的语义逐条钉住：
// 查询走 lower(name)，upsert 冲突目标随方言（helper nameConflictClause），
// 两个方言都必须过同一组断言。
func TestUserStore_CaseVariantSemantics(t *testing.T) {
	runWithMgrBackends(t, testUserStoreCaseVariantSemantics)
}

func testUserStoreCaseVariantSemantics(t *testing.T, open mgrOpener, _ mgrStoreFactory) {
	h := open(t)
	store := NewUserStore(h)

	// 大小写变体 upsert：Save("Foo") 落库后 Save("FOO") 必须命中同一行并更新，
	// 而不是插入出第二个实体。upsert 只覆盖 password_bcrypt/role/updated_at，
	// 名字保留首次写入的书写形式（与迁移前行为逐字一致）。
	require.NoError(t, store.Save(core.UserRecord{Name: "Foo", PasswordBcrypt: testHash, Role: "operator"}))
	require.NoError(t, store.Save(core.UserRecord{Name: "FOO", PasswordBcrypt: testHash, Role: "admin"}))
	count, err := store.Count()
	require.NoError(t, err)
	assert.Equal(t, 1, count, "大小写变体必须命中同一行")
	got, ok, err := store.Get("foo")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "admin", got.Role, "变体 upsert 覆盖角色")
	assert.Equal(t, "Foo", got.Name, "名字保留首次写入的书写形式")

	// 变体删除：Delete("fOo") 删掉唯一一行
	require.NoError(t, store.Delete("fOo"))
	count, err = store.Count()
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// 排序与大小写书写形式无关：lower(name) 同序，名字原样返回
	for _, name := range []string{"carol", "Bob", "alice"} {
		require.NoError(t, store.Save(core.UserRecord{Name: name, PasswordBcrypt: testHash, Role: "viewer"}))
	}
	items, err := store.List()
	require.NoError(t, err)
	require.Len(t, items, 3)
	assert.Equal(t, []string{"alice", "Bob", "carol"}, []string{items[0].Name, items[1].Name, items[2].Name})
}

func TestUserStore_SaveRejectsInvalidRecords(t *testing.T) {
	runWithMgrBackends(t, testUserStoreSaveRejectsInvalidRecords)
}

func testUserStoreSaveRejectsInvalidRecords(t *testing.T, open mgrOpener, _ mgrStoreFactory) {
	h := open(t)
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
	runWithMgrBackends(t, testUserStoreReopenPersists)
}

func testUserStoreReopenPersists(t *testing.T, _ mgrOpener, factory mgrStoreFactory) {
	cfg := factory(t)

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
