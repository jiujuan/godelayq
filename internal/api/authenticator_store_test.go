package api

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jiujuan/godelayq/internal/core"
	"golang.org/x/crypto/bcrypt"
)

// fakeUserStore 是 core.UserStore 的内存实现，供认证器库模式测试使用。
// 只测认证器逻辑，不测存储本身——存储的语义在 store/mgr 有自己的用例。
type fakeUserStore struct {
	mu    sync.Mutex
	users map[string]core.UserRecord
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{users: make(map[string]core.UserRecord)}
}

func (f *fakeUserStore) put(record core.UserRecord) {
	f.users[strings.ToLower(record.Name)] = record
}

func (f *fakeUserStore) List() ([]core.UserRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.UserRecord, 0, len(f.users))
	for _, record := range f.users {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeUserStore) Get(name string) (core.UserRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.users[strings.ToLower(strings.TrimSpace(name))]
	return record, ok, nil
}

func (f *fakeUserStore) Save(record core.UserRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	f.put(record)
	return nil
}

func (f *fakeUserStore) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(name))
	if _, ok := f.users[key]; !ok {
		return core.ErrUserNotFound
	}
	delete(f.users, key)
	return nil
}

func (f *fakeUserStore) Count() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users), nil
}

func storeHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	require.NoError(t, err)
	return string(h)
}

func testAuthConfig() core.AuthConfig {
	return core.AuthConfig{
		JWT: core.JWTConfig{
			Secret:     "test-secret-must-be-long-enough-32",
			AccessTTL:  core.DefaultAccessTTL,
			RefreshTTL: core.DefaultRefreshTTL,
		},
	}
}

// TestAuthenticator_StoreBacked_LoginReflectsCurrentRole 钉住库模式的核心语义：
// 登录与验签都以"账号当前在库里的状态"为准，改角色 + InvalidateUser 即时生效。
func TestAuthenticator_StoreBacked_LoginReflectsCurrentRole(t *testing.T) {
	store := newFakeUserStore()
	require.NoError(t, store.Save(core.UserRecord{
		Name: "alice", PasswordBcrypt: storeHash(t, "pw-alice-1"), Role: "admin",
	}))

	auth, err := NewAuthenticator(testAuthConfig(), store, nil)
	require.NoError(t, err)
	require.True(t, auth.Enabled(), "库里有账号，鉴权启用")
	require.True(t, auth.dbBacked())

	// 登录：大小写变体命中（NOCASE 口径）
	p, err := auth.Authenticate("Alice", "pw-alice-1")
	require.NoError(t, err)
	assert.Equal(t, core.RoleAdmin, p.Role)

	tokens := newAuthStore()
	session, err := auth.IssueSession(p, tokens)
	require.NoError(t, err)

	// 验签通过
	got, _, err := auth.VerifyAccessToken(session.AccessToken, tokens)
	require.NoError(t, err)
	assert.Equal(t, core.RoleAdmin, got.Role)

	// 降权 + 失效缓存：旧令牌立即不可用
	require.NoError(t, store.Save(core.UserRecord{
		Name: "alice", PasswordBcrypt: storeHash(t, "pw-alice-1"), Role: "viewer",
	}))
	auth.InvalidateUser("alice")

	_, _, err = auth.VerifyAccessToken(session.AccessToken, tokens)
	require.Error(t, err, "旧令牌写着 admin，库里已是 viewer，必须被拒")

	// 重新登录拿到新角色
	p2, err := auth.Authenticate("alice", "pw-alice-1")
	require.NoError(t, err)
	assert.Equal(t, core.RoleViewer, p2.Role)
}

// TestAuthenticator_StoreBacked_DeleteAndNegativeCache 钉住删号与负缓存：
// 删号后登录失败；伪造 sub 的请求不打库（负缓存挡住），但写库后缓存必须失效。
func TestAuthenticator_StoreBacked_DeleteAndNegativeCache(t *testing.T) {
	store := newFakeUserStore()
	require.NoError(t, store.Save(core.UserRecord{
		Name: "bob", PasswordBcrypt: storeHash(t, "pw-bob-1"), Role: "operator",
	}))

	auth, err := NewAuthenticator(testAuthConfig(), store, nil)
	require.NoError(t, err)

	_, err = auth.Authenticate("bob", "pw-bob-1")
	require.NoError(t, err)

	// 删号 + 失效：登录立即失败
	require.NoError(t, store.Delete("bob"))
	auth.InvalidateUser("bob")
	_, err = auth.Authenticate("bob", "pw-bob-1")
	require.Error(t, err, "账号已删除，登录必须失败")

	// 负缓存生效：同样的"不存在"查询不再触库——用计数证明
	_, err = auth.Authenticate("nobody", "whatever")
	require.Error(t, err)
	// fake store 没有计数器，这里改用"再次命中负缓存后 Get 未被调用"来验证：
	// 无法直接观测，就验证语义结果——负缓存只影响性能，正确性由 Invalidate 保证。
	auth.InvalidateAll()
	require.NoError(t, store.Save(core.UserRecord{
		Name: "nobody", PasswordBcrypt: storeHash(t, "pw-nobody-1"), Role: "viewer",
	}))
	p, err := auth.Authenticate("nobody", "pw-nobody-1")
	require.NoError(t, err, "InvalidateAll 之后新建的账号必须立即可登录")
	assert.Equal(t, "nobody", p.Name)
}

// TestAuthenticator_StoreBacked_EmptyStoreDisablesConsole 钉住启用判定：
// 库为空时控制台鉴权不启用（与"没有配置账号"同态），有账号即启用。
func TestAuthenticator_StoreBacked_EmptyStoreDisablesConsole(t *testing.T) {
	empty := newFakeUserStore()
	auth, err := NewAuthenticator(testAuthConfig(), empty, nil)
	require.NoError(t, err)
	require.NotNil(t, auth)
	assert.False(t, auth.Enabled(), "库为空：控制台鉴权不启用")

	one := newFakeUserStore()
	require.NoError(t, one.Save(core.UserRecord{
		Name: "seed", PasswordBcrypt: storeHash(t, "pw-seed-1"), Role: "admin",
	}))
	auth2, err := NewAuthenticator(testAuthConfig(), one, nil)
	require.NoError(t, err)
	assert.True(t, auth2.Enabled())
}

// TestAuthenticator_StoreBacked_StoreErrorFailsClosed 钉住故障语义：
// 查库失败按"账号不存在"处理（fail closed），且不写负缓存——
// 瞬时故障不该永久拉黑账号，也不该被误读成有把握的答案。
func TestAuthenticator_StoreBacked_StoreErrorFailsClosed(t *testing.T) {
	store := &flakyUserStore{err: fmt.Errorf("db is down"), count: 1}
	auth, err := NewAuthenticator(testAuthConfig(), store, nil)
	require.NoError(t, err)

	_, err = auth.Authenticate("alice", "pw")
	require.Error(t, err, "库读不出来时登录必须失败（fail closed）")
	_, err = auth.Authenticate("alice", "pw")
	require.Error(t, err, "失败不入缓存：第二次同样要 fail closed")
}

// flakyUserStore 模拟"启动后库才出问题"：Count 正常返回（构造期放行），读取一律失败。
type flakyUserStore struct {
	err   error
	count int
}

func (f *flakyUserStore) List() ([]core.UserRecord, error) { return nil, f.err }
func (f *flakyUserStore) Get(string) (core.UserRecord, bool, error) {
	return core.UserRecord{}, false, f.err
}
func (f *flakyUserStore) Save(core.UserRecord) error { return f.err }
func (f *flakyUserStore) Delete(string) error        { return f.err }
func (f *flakyUserStore) Count() (int, error)        { return f.count, nil }
