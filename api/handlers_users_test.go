package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"godelayq/core"
)

// usersTestServer 建一台带账号存储的安全服务器：库模式认证器（配置里没有 users 节，
// 账号只认库里的 root/guest 种子），登录走真实端点。
func usersTestServer(t *testing.T) (*Server, *fakeUserStore) {
	t.Helper()

	store := newFakeUserStore()
	require.NoError(t, store.Save(core.UserRecord{
		Name: "root", PasswordBcrypt: storeHash(t, "pw-root-1"), Role: "admin",
	}))
	require.NoError(t, store.Save(core.UserRecord{
		Name: "guest", PasswordBcrypt: storeHash(t, "pw-guest-1"), Role: "viewer",
	}))

	sec := Security{Auth: core.AuthConfig{
		JWT: core.JWTConfig{Secret: testJWTSecret, AccessTTL: time.Hour, RefreshTTL: time.Hour},
	}}
	srv := newSecurityServer(t, sec, WithUserStore(store))
	return srv, store
}

func TestUsersEndpoints_503WithoutStore(t *testing.T) {
	// 遗留配置模式（没注入账号存储）：管理端点整组 503，而不是 404 或空列表
	srv := newSecurityServer(t, Security{})

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/admin/users"},
		{http.MethodPost, "/api/v1/admin/users"},
		{http.MethodPut, "/api/v1/admin/users/x"},
		{http.MethodDelete, "/api/v1/admin/users/x"},
	} {
		rec := doJSON(t, srv, tc.method, tc.target, "{}", nil)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, tc.method+" "+tc.target)
	}
}

func TestUsersEndpoints_CRUDAndProtections(t *testing.T) {
	srv, store := usersTestServer(t)
	adminSession := login(t, srv, "root", "pw-root-1")
	adminHeader := bearer(adminSession.AccessToken)

	// 列表：不含任何哈希字段
	rec := doGet(t, srv, "/api/v1/admin/users", adminHeader)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "password_bcrypt", "哈希绝不出列表")
	var views []UserView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &views))
	require.Len(t, views, 2)

	// 建号：201；重复 409；坏角色 400
	rec = doJSON(t, srv, http.MethodPost, "/api/v1/admin/users",
		`{"name":"harry","password":"pw-harry-1","role":"operator"}`, adminHeader)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, srv, http.MethodPost, "/api/v1/admin/users",
		`{"name":"HARRY","password":"pw-harry-1","role":"viewer"}`, adminHeader)
	assert.Equal(t, http.StatusConflict, rec.Code, "大小写变体视作同一账号（NOCASE 口径）")

	rec = doJSON(t, srv, http.MethodPost, "/api/v1/admin/users",
		`{"name":"bad","password":"pw-bad-1","role":"machine"}`, adminHeader)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// 改角色：200；不存在的账号 404；空载荷 400
	rec = doJSON(t, srv, http.MethodPut, "/api/v1/admin/users/harry",
		`{"role":"viewer"}`, adminHeader)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doJSON(t, srv, http.MethodPut, "/api/v1/admin/users/ghost", `{"role":"viewer"}`, adminHeader)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = doJSON(t, srv, http.MethodPut, "/api/v1/admin/users/harry", `{}`, adminHeader)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// 改密码后旧密码立即失效、新密码可登录（缓存失效线的端到端证明）
	rec = doJSON(t, srv, http.MethodPut, "/api/v1/admin/users/guest",
		`{"password":"pw-guest-2"}`, adminHeader)
	require.Equal(t, http.StatusOK, rec.Code)
	_, err := srv.auth.Authenticate("guest", "pw-guest-1")
	assert.Error(t, err, "改密后旧密码必须立即失效")
	_, err = srv.auth.Authenticate("guest", "pw-guest-2")
	assert.NoError(t, err)

	// 删除：204；不存在 404
	rec = doJSON(t, srv, http.MethodDelete, "/api/v1/admin/users/harry", "", adminHeader)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	rec = doJSON(t, srv, http.MethodDelete, "/api/v1/admin/users/ghost", "", adminHeader)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// 自删保护：root 删自己 → 409
	rec = doJSON(t, srv, http.MethodDelete, "/api/v1/admin/users/ROOT", "", adminHeader)
	assert.Equal(t, http.StatusConflict, rec.Code, "自删判断按大小写不敏感比较")

	// 最后一个 admin 的两条保护线
	rec = doJSON(t, srv, http.MethodPut, "/api/v1/admin/users/root",
		`{"role":"viewer"}`, adminHeader)
	assert.Equal(t, http.StatusConflict, rec.Code, "最后一个 admin 不能降权")

	rec = doJSON(t, srv, http.MethodDelete, "/api/v1/admin/users/root", "", adminHeader)
	// 自删保护先命中（同一请求两条线都成立时，自删的文案更贴切）
	assert.Equal(t, http.StatusConflict, rec.Code)

	// 建第二个 admin 后，第一条线解除
	rec = doJSON(t, srv, http.MethodPost, "/api/v1/admin/users",
		`{"name":"backup","password":"pw-backup-1","role":"admin"}`, adminHeader)
	require.Equal(t, http.StatusCreated, rec.Code)
	rec = doJSON(t, srv, http.MethodPut, "/api/v1/admin/users/root",
		`{"role":"viewer"}`, adminHeader)
	assert.Equal(t, http.StatusOK, rec.Code, "还有别的 admin 时可以降权")

	// 非 admin 删 admin：backup 现在是唯一 admin——先验证档位门槛，再验证最后一条线
	guestSession := login(t, srv, "guest", "pw-guest-2")
	rec = doJSON(t, srv, http.MethodPost, "/api/v1/admin/users",
		`{"name":"nope","password":"pw-nope-1","role":"viewer"}`, bearer(guestSession.AccessToken))
	assert.Equal(t, http.StatusForbidden, rec.Code, "viewer 不能管理账号")

	// backup（唯一 admin）被 root 之外的身份删除：非自删但会清空 admin → 409
	// （此时 root 已是 viewer，令牌还是旧的 admin——验签会拒；直接用库状态验证保护线）
	backupRecord, ok, err := store.Get("backup")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "admin", backupRecord.Role)
	err = srv.checkLastAdmin(backupRecord, "")
	assert.Error(t, err, "删除唯一 admin 必须被拦")
}

func TestUsersEndpoints_AuditActionsCovered(t *testing.T) {
	// 审计动作映射的守卫：三个新路由必须有封闭动作词，
	// 漏配会让台账落进 other，查询端点按动作过滤时就成了哑弹。
	for _, route := range []string{
		http.MethodPost + " /api/v1/admin/users",
		http.MethodPut + " /api/v1/admin/users/:name",
		http.MethodDelete + " /api/v1/admin/users/:name",
	} {
		action, ok := auditActions[route]
		require.True(t, ok, "missing audit action for %s", route)
		assert.True(t, strings.HasPrefix(action, "user."), route)
	}
}
