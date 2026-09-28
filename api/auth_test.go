package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"godelayq/core"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

const (
	testJWTSecret    = "unit-test-secret-012345678901234567890"
	testPassword     = "correct horse battery staple"
	testViewerName   = "watcher"
	testOperatorName = "worker"
	testAdminName    = "rooter"
	testOpsName      = "keeper"
)

// bcrypt cost 10 每次约 60-100ms；整包测试会创建几十个服务器，
// 哈希只生成一次复用，把认证测试的耗时压在秒级。
var (
	testHashOnce  sync.Once
	testPasswordH string
)

func cachedTestHash(t *testing.T) string {
	t.Helper()

	testHashOnce.Do(func() {
		hash, err := bcryptHash(testPassword)
		if err != nil {
			panic(err)
		}
		testPasswordH = hash
	})
	return testPasswordH
}

// bcryptHash 生成配置里那种 bcrypt 串，供测试搭账号用。
func bcryptHash(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

// registerNopHandler 注册一个空实现的任务类型：POST /jobs 会校验类型是否已注册，
// 权限测试关心的是 403/201 的分界，不需要真的跑业务逻辑。
func registerNopHandler(srv *Server) {
	srv.RegisterJobHandler("payment_check", func(context.Context, *core.Job) error { return nil })
}

// accountsSecurity 构造带四个档位账号的安全参数。
func accountsSecurity(t *testing.T) Security {
	t.Helper()

	hash := cachedTestHash(t)
	return Security{
		Auth: core.AuthConfig{
			JWT: core.JWTConfig{Secret: testJWTSecret, AccessTTL: time.Hour, RefreshTTL: time.Hour},
			Users: []core.UserConfig{
				{Name: testViewerName, PasswordBcrypt: hash, Role: "viewer"},
				{Name: testOperatorName, PasswordBcrypt: hash, Role: "operator"},
				{Name: testAdminName, PasswordBcrypt: hash, Role: "admin"},
				{Name: testOpsName, PasswordBcrypt: hash, Role: "ops"},
			},
		},
	}
}

// doJSON 发一个带 JSON 体的请求，header 可为 nil。
func doJSON(t *testing.T, srv *Server, method, target, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()

	req, err := http.NewRequest(method, target, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	recorder := httptest.NewRecorder()
	srv.engine.ServeHTTP(recorder, req)
	return recorder
}

// login 走真实端点拿会话，返回令牌对。
func login(t *testing.T, srv *Server, username, password string) TokenSessionResponse {
	t.Helper()

	body, err := json.Marshal(LoginRequest{Username: username, Password: password})
	require.NoError(t, err)

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/auth/login", string(body), nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var session TokenSessionResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &session))
	return session
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

func TestLoginIssuesSessionAndMe(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))

	session := login(t, srv, testAdminName, testPassword)
	assert.Equal(t, "Bearer", session.TokenType)
	assert.True(t, session.ExpiresAt.After(time.Now().Add(50*time.Minute)))
	assert.Equal(t, testAdminName, session.User.Name)
	assert.Equal(t, "admin", session.User.Role)
	assert.NotEmpty(t, session.RefreshToken)

	recorder := doGet(t, srv, "/api/v1/auth/me", bearer(session.AccessToken))
	require.Equal(t, http.StatusOK, recorder.Code)

	var me WhoAmIResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &me))
	assert.Equal(t, testAdminName, me.Name)
	assert.Equal(t, "admin", me.Role)
	assert.True(t, me.ExpiresAt.After(time.Now()))
}

// 用户不存在与密码错误必须给出一模一样的响应，否则登录端点就是账号枚举器。
func TestLoginFailureIsIndistinguishable(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))

	bodies := make([]string, 0, 2)
	for _, username := range []string{"no-such-account", testViewerName} {
		body, err := json.Marshal(LoginRequest{Username: username, Password: "wrong-password"})
		require.NoError(t, err)

		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/auth/login", string(body), nil)
		require.Equal(t, http.StatusUnauthorized, recorder.Code, username)
		bodies = append(bodies, recorder.Body.String())
	}

	assert.JSONEq(t, bodies[0], bodies[1], "unknown account and wrong password must be indistinguishable")
}

func TestLoginRateLimitedAfterFailures(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))

	// 阈值内：都是 401
	for i := 0; i < 5; i++ {
		body, _ := json.Marshal(LoginRequest{Username: testViewerName, Password: "wrong"})
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/auth/login", string(body), nil)
		require.Equal(t, http.StatusUnauthorized, recorder.Code, "attempt %d", i)
	}

	body, _ := json.Marshal(LoginRequest{Username: testViewerName, Password: "wrong"})
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/auth/login", string(body), nil)
	assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
	assert.NotEmpty(t, recorder.Header().Get("Retry-After"))

	// 限流按 IP+账号两个维度：换一个账号不该被前面的失败拖累
	adminBody, _ := json.Marshal(LoginRequest{Username: testAdminName, Password: testPassword})
	assert.Equal(t, http.StatusOK, doJSON(t, srv, http.MethodPost, "/api/v1/auth/login", string(adminBody), nil).Code)
}

func TestRefreshRotatesRefreshToken(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))
	session := login(t, srv, testOperatorName, testPassword)

	refreshBody, _ := json.Marshal(RefreshRequest{RefreshToken: session.RefreshToken})
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/auth/refresh", string(refreshBody), nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var rotated TokenSessionResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &rotated))
	assert.NotEqual(t, session.RefreshToken, rotated.RefreshToken, "refresh token must rotate")
	assert.NotEqual(t, session.AccessToken, rotated.AccessToken)

	// 旧 refresh token 用过即废：重放必须失败
	replay := doJSON(t, srv, http.MethodPost, "/api/v1/auth/refresh", string(refreshBody), nil)
	assert.Equal(t, http.StatusUnauthorized, replay.Code)

	// 新访问令牌可用
	assert.Equal(t, http.StatusOK, doGet(t, srv, "/api/v1/auth/me", bearer(rotated.AccessToken)).Code)
}

// 退出必须是真退出：access token 在自然过期之前就要被拦下。
func TestLogoutRevokesAccessTokenAndRefreshToken(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))
	session := login(t, srv, testAdminName, testPassword)

	require.Equal(t, http.StatusOK, doGet(t, srv, "/api/v1/auth/me", bearer(session.AccessToken)).Code)

	logoutBody, _ := json.Marshal(LogoutRequest{RefreshToken: session.RefreshToken})
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/auth/logout", string(logoutBody), bearer(session.AccessToken))
	require.Equal(t, http.StatusNoContent, recorder.Code)

	assert.Equal(t, http.StatusUnauthorized, doGet(t, srv, "/api/v1/auth/me", bearer(session.AccessToken)).Code,
		"logged-out access token must stop working before its natural expiry")

	refreshBody, _ := json.Marshal(RefreshRequest{RefreshToken: session.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized,
		doJSON(t, srv, http.MethodPost, "/api/v1/auth/refresh", string(refreshBody), nil).Code)
}

func TestRolesGateWrites(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))
	registerNopHandler(srv)

	jobBody, _ := json.Marshal(CreateJobRequest{Name: "payment_check", Delay: "1m"})

	for _, tc := range []struct {
		username string
		want     int
	}{
		{testViewerName, http.StatusForbidden},
		{testOperatorName, http.StatusCreated},
		{testAdminName, http.StatusCreated},
		{testOpsName, http.StatusCreated},
	} {
		t.Run(tc.username, func(t *testing.T) {
			session := login(t, srv, tc.username, testPassword)

			created := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", string(jobBody), bearer(session.AccessToken))
			assert.Equal(t, tc.want, created.Code, created.Body.String())

			// 读权限对所有档位一致
			assert.Equal(t, http.StatusOK, doGet(t, srv, "/api/v1/jobs", bearer(session.AccessToken)).Code)
			assert.Equal(t, http.StatusOK, doGet(t, srv, "/api/v1/stats", bearer(session.AccessToken)).Code)
		})
	}
}

// 机器凭据继续能干活（脚本兼容），但它在权限模型里不是人：
// 拿不到 admin/ops 的能力，也不该出现在需要真实身份的端点上。
func TestMachineTokenKeepsLegacyWritesButIsNotAdmin(t *testing.T) {
	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	srv := newSecurityServer(t, sec)
	registerNopHandler(srv)

	jobBody, _ := json.Marshal(CreateJobRequest{Name: "payment_check", Delay: "1m"})
	created := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", string(jobBody), bearer(testToken))
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	recorder := doGet(t, srv, "/api/v1/auth/me", bearer(testToken))
	require.Equal(t, http.StatusOK, recorder.Code)
	var me WhoAmIResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &me))
	assert.Equal(t, core.RoleMachine.String(), me.Role)
	assert.Equal(t, machinePrincipalName, me.Name)
}

// JWT 不进 URL：query 通道只认静态机器凭据（api/logging.go 会把 query 原样写进日志）。
func TestAccessTokenIsRejectedInQueryString(t *testing.T) {
	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	srv := newSecurityServer(t, sec)

	session := login(t, srv, testAdminName, testPassword)

	assert.Equal(t, http.StatusUnauthorized,
		doGet(t, srv, "/api/v1/jobs?token="+session.AccessToken, nil).Code,
		"a JWT in ?token= must not authenticate")
	assert.Equal(t, http.StatusOK, doGet(t, srv, "/api/v1/jobs?token="+testToken, nil).Code,
		"the machine token still works over query")
}

// ticket 只服务实时通道，而且一次一用。
func TestWSTicketIsSingleUseAndScopedToRealtimeChannels(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))
	session := login(t, srv, testViewerName, testPassword)

	ticketOf := func() string {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/auth/ws-ticket", "{}", bearer(session.AccessToken))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

		var issued WsTicketResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &issued))
		require.NotEmpty(t, issued.Ticket)
		return issued.Ticket
	}

	// REST 不接受 ticket：它是"一次连接"的凭据，不是可复用的授权头
	assert.Equal(t, http.StatusUnauthorized, doGet(t, srv, "/api/v1/stats?ticket="+ticketOf(), nil).Code)

	hs := httptest.NewServer(srv.engine)
	defer hs.Close()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws"

	ticket := ticketOf()
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL+"?ticket="+ticket, nil)
	require.NoError(t, err, "fresh ticket should upgrade: %v", resp)
	require.NotNil(t, conn)
	assert.NoError(t, conn.Close())

	// 同一张 ticket 用第二次必须失败：一次一用是它短寿命之外唯一的保护
	reused, _, err := websocket.DefaultDialer.Dial(wsURL+"?ticket="+ticket, nil)
	if reused != nil {
		_ = reused.Close()
	}
	assert.Error(t, err, "a consumed ticket must not grant a second connection")
}

func TestAccessTokenExpiryIsEnforced(t *testing.T) {
	sec := accountsSecurity(t)
	sec.Auth.JWT.AccessTTL = time.Second
	srv := newSecurityServer(t, sec)

	session := login(t, srv, testAdminName, testPassword)
	require.Equal(t, http.StatusOK, doGet(t, srv, "/api/v1/auth/me", bearer(session.AccessToken)).Code)

	time.Sleep(1200 * time.Millisecond)
	assert.Equal(t, http.StatusUnauthorized, doGet(t, srv, "/api/v1/auth/me", bearer(session.AccessToken)).Code)
}

func TestAccessTokenFromAnotherKeyIsRejected(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))
	session := login(t, srv, testAdminName, testPassword)

	other := newSecurityServer(t, Security{Auth: core.AuthConfig{
		JWT:   core.JWTConfig{Secret: "a-completely-different-secret-0123456789", AccessTTL: time.Hour, RefreshTTL: time.Hour},
		Users: []core.UserConfig{{Name: testAdminName, PasswordBcrypt: cachedTestHash(t), Role: "admin"}},
	}})

	// 同名同角色，但签名密钥不同：必须当作伪造令牌
	assert.Equal(t, http.StatusUnauthorized, doGet(t, other, "/api/v1/auth/me", bearer(session.AccessToken)).Code)
}

func TestLoginWithoutAccountsConfigured(t *testing.T) {
	srv := newSecurityServer(t, Security{Auth: core.AuthConfig{Token: testToken}})

	body, _ := json.Marshal(LoginRequest{Username: "any", Password: "any"})
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/auth/login", string(body), nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "console accounts are not configured")
}

// 未启用鉴权时保持历史语义：全部放行，且不做角色区分。
func TestAuthDisabledStillAllowsWrites(t *testing.T) {
	srv := newSecurityServer(t, Security{})
	registerNopHandler(srv)

	jobBody, _ := json.Marshal(CreateJobRequest{Name: "payment_check", Delay: "1m"})
	assert.Equal(t, http.StatusCreated,
		doJSON(t, srv, http.MethodPost, "/api/v1/jobs", string(jobBody), nil).Code)
}

func TestNewAuthenticatorRejectsBrokenAccountConfig(t *testing.T) {
	hash := cachedTestHash(t)

	cases := []struct {
		name string
		auth core.AuthConfig
		want string
	}{
		{
			name: "users without secret",
			auth: core.AuthConfig{Users: []core.UserConfig{{Name: "a", PasswordBcrypt: hash, Role: "admin"}}},
			want: "jwt.secret is required",
		},
		{
			name: "malformed hash",
			auth: core.AuthConfig{JWT: core.JWTConfig{Secret: testJWTSecret},
				Users: []core.UserConfig{{Name: "a", PasswordBcrypt: "plaintext-password", Role: "admin"}}},
			want: "invalid bcrypt hash",
		},
		{
			name: "cheap hash",
			auth: core.AuthConfig{JWT: core.JWTConfig{Secret: testJWTSecret},
				Users: []core.UserConfig{{Name: "a", PasswordBcrypt: cheapHash(t), Role: "admin"}}},
			want: "bcrypt cost",
		},
		{
			name: "unknown role",
			auth: core.AuthConfig{JWT: core.JWTConfig{Secret: testJWTSecret},
				Users: []core.UserConfig{{Name: "a", PasswordBcrypt: hash, Role: "wizard"}}},
			want: "invalid role",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAuthenticator(tc.auth)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestNewAuthenticatorDisabledWithoutUsers(t *testing.T) {
	// 只配了机器凭据的部署不该被要求准备 JWT 密钥
	auth, err := NewAuthenticator(core.AuthConfig{Token: testToken})
	require.NoError(t, err)
	assert.Nil(t, auth)
	assert.False(t, auth.Enabled())
}

// 账号配置坏掉时必须挡住启动：否则进程带着"看似开启实则全灭"的鉴权上线。
func TestStartFailsWhenAuthMisconfigured(t *testing.T) {
	sec := accountsSecurity(t)
	sec.Auth.Users[0].PasswordBcrypt = "not-a-bcrypt-hash"
	srv := newSecurityServer(t, sec)

	err := srv.Start()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authentication is misconfigured")
	assert.Contains(t, err.Error(), "invalid bcrypt hash")
}

// cheapHash 生成 cost 4 的哈希，用来验证配置层拒绝"太便宜的哈希"。
func cheapHash(t *testing.T) string {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	require.NoError(t, err)
	return string(hash)
}

// 写操作审计（设计文档 §5.7.6）落在访问日志里：每条请求带 who/role。
func TestAccessLogCarriesAuthenticatedIdentity(t *testing.T) {
	buf := &bytes.Buffer{}
	gin.SetMode(gin.TestMode)

	store, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	srv := NewServer(core.NewScheduler(store, nil, nil), store, "0", accountsSecurity(t),
		slog.New(slog.NewTextHandler(buf, nil)))
	registerNopHandler(srv)

	session := login(t, srv, testOperatorName, testPassword)
	jobBody, _ := json.Marshal(CreateJobRequest{Name: "payment_check", Delay: "1m"})
	require.Equal(t, http.StatusCreated,
		doJSON(t, srv, http.MethodPost, "/api/v1/jobs", string(jobBody), bearer(session.AccessToken)).Code)

	// 越权请求同样要留痕：它比成功请求更值得事后翻
	viewerSession := login(t, srv, testViewerName, testPassword)
	require.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodPost, "/api/v1/jobs", string(jobBody), bearer(viewerSession.AccessToken)).Code)

	lines := buf.String()
	assert.Contains(t, lines, `who=`+testOperatorName)
	assert.Contains(t, lines, "role=operator")
	assert.Contains(t, lines, `who=`+testViewerName)
	assert.Contains(t, lines, "role=viewer")
	assert.Contains(t, lines, "status=403")
}
