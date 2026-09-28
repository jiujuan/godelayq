package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"godelayq/core"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "s3cret-token"

// newSecurityServer 构造只用于 ServeHTTP 的服务器，不监听端口
func newSecurityServer(t *testing.T, sec Security, opts ...Option) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	store, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	// 存储有后台合并协程，测试结束（含 t.TempDir 清理）前必须收口，
	// 否则协程会往已被删除的目录写临时文件并刷出 flush failed。
	t.Cleanup(func() { _ = store.Close() })

	return NewServer(core.NewScheduler(store, nil, nil), store, "0", sec, newTestLogger(), opts...)
}

func doGet(t *testing.T, srv *Server, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, target, nil)
	require.NoError(t, err)
	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	recorder := httptest.NewRecorder()
	srv.engine.ServeHTTP(recorder, req)

	return recorder
}

func TestAuthDisabledWhenTokenEmpty(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	assert.Equal(t, 200, doGet(t, srv, "/api/v1/health", nil).Code)
}

func TestAuthRejectsMissingOrWrongToken(t *testing.T) {
	srv := newSecurityServer(t, Security{Auth: core.AuthConfig{Token: testToken}})

	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{"no credential", nil},
		{"wrong token", http.Header{"Authorization": {"Bearer nope"}}},
		{"empty bearer", http.Header{"Authorization": {"Bearer "}}},
		{"non-bearer scheme", http.Header{"Authorization": {"Basic " + testToken}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doGet(t, srv, "/api/v1/health", tc.header)
			require.Equal(t, 401, recorder.Code)

			var body ErrorResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			assert.Equal(t, 401, body.Code)
			assert.Equal(t, "invalid or missing credentials", body.Message)
		})
	}
}

func TestAuthAcceptsEveryTokenChannel(t *testing.T) {
	srv := newSecurityServer(t, Security{Auth: core.AuthConfig{Token: testToken}})

	for _, tc := range []struct {
		name   string
		target string
		header http.Header
	}{
		{"bearer header", "/api/v1/health", http.Header{"Authorization": {"Bearer " + testToken}}},
		{"bearer with surrounding spaces", "/api/v1/health", http.Header{"Authorization": {"Bearer   " + testToken + " "}}},
		{"custom header", "/api/v1/health", http.Header{"X-Auth-Token": {testToken}}},
		{"query param", "/api/v1/health?token=" + testToken, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, 200, doGet(t, srv, tc.target, tc.header).Code)
		})
	}
}

// 浏览器发的 Authorization 优先于 query，配错 scheme 时不应回退到 query 蒙混过关
func TestAuthHeaderTakesPrecedenceOverQuery(t *testing.T) {
	srv := newSecurityServer(t, Security{Auth: core.AuthConfig{Token: testToken}})

	header := http.Header{"Authorization": {"Token " + testToken}}
	recorder := doGet(t, srv, "/api/v1/health?token="+testToken, header)

	assert.Equal(t, 401, recorder.Code)
}

func TestProtectedWritesAndStreamsNeedToken(t *testing.T) {
	srv := newSecurityServer(t, Security{Auth: core.AuthConfig{Token: testToken}})

	body, _ := json.Marshal(CreateJobRequest{Name: "payment_check", Delay: "1m"})
	req, err := http.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(string(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.engine.ServeHTTP(recorder, req)
	assert.Equal(t, 401, recorder.Code)

	assert.Equal(t, 401, doGet(t, srv, "/sse/events", nil).Code)
	assert.Equal(t, 401, doGet(t, srv, "/ws", nil).Code)
}

func TestPreflightSkipsAuth(t *testing.T) {
	srv := newSecurityServer(t, Security{Auth: core.AuthConfig{Token: testToken}})

	req, err := http.NewRequest(http.MethodOptions, "/api/v1/jobs", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	recorder := httptest.NewRecorder()
	srv.engine.ServeHTTP(recorder, req)

	assert.Equal(t, 204, recorder.Code)
	assert.Equal(t, "*", recorder.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, recorder.Header().Get("Access-Control-Allow-Headers"), "X-Auth-Token")
}

func TestCORSDefaultsToWildcard(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	recorder := doGet(t, srv, "/api/v1/health", http.Header{"Origin": {"https://anywhere.example"}})

	assert.Equal(t, 200, recorder.Code)
	assert.Equal(t, "*", recorder.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, recorder.Header().Get("Access-Control-Allow-Credentials"))
}

func TestCORSWhitelistEchoesMatchedOrigin(t *testing.T) {
	srv := newSecurityServer(t, Security{
		AllowOrigins:     []string{"https://app.example.com"},
		AllowCredentials: true,
	})

	matched := doGet(t, srv, "/api/v1/health", http.Header{"Origin": {"https://app.example.com"}})
	assert.Equal(t, "https://app.example.com", matched.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", matched.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, matched.Header().Values("Vary"), "Origin")

	// 白名单之外的来源拿不到 ACAO，浏览器随即拦截跨域响应
	other := doGet(t, srv, "/api/v1/health", http.Header{"Origin": {"https://evil.example"}})
	assert.Equal(t, 200, other.Code)
	assert.Empty(t, other.Header().Get("Access-Control-Allow-Origin"))
}

func TestWebsocketHandshakeHonoursTokenAndOrigin(t *testing.T) {
	srv := newSecurityServer(t, Security{
		Auth: core.AuthConfig{
			Token: testToken,
		},
		AllowOrigins: []string{"https://app.example.com"},
	})
	hs := httptest.NewServer(srv.engine)
	defer hs.Close()

	wsURL := func(path string) string {
		return "ws" + strings.TrimPrefix(hs.URL, "http") + path
	}

	for _, tc := range []struct {
		name       string
		url        string
		origin     string
		wantStatus int
	}{
		{"missing token", wsURL("/ws"), "https://app.example.com", http.StatusUnauthorized},
		{"wrong token", wsURL("/ws?token=nope"), "https://app.example.com", http.StatusUnauthorized},
		{"query token ok", wsURL("/ws?token=" + testToken), "", http.StatusSwitchingProtocols},
		{"foreign origin rejected", wsURL("/ws?token=" + testToken), "https://evil.example", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			if tc.origin != "" {
				header.Set("Origin", tc.origin)
			}

			conn, resp, err := websocket.DefaultDialer.Dial(tc.url, header)
			if tc.wantStatus == http.StatusSwitchingProtocols {
				require.NoError(t, err)
				require.NotNil(t, conn)
				assert.NoError(t, conn.Close())
				return
			}

			// gorilla 只报 "bad handshake"，状态码要从响应里取
			require.Error(t, err)
			require.NotNil(t, resp)
			assert.Equal(t, tc.wantStatus, resp.StatusCode)
		})
	}
}
