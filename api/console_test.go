package api

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeConsole 是一份最小的前端产物：一个入口 HTML 加一个带内容哈希的资源名。
// 用 MapFS 而不是真实 dist，测试因此不依赖 npm run build。
func fakeConsole() fstest.MapFS {
	return fstest.MapFS{
		"index.html":          &fstest.MapFile{Data: []byte("<!doctype html><title>console</title>")},
		"assets/index-abc.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
}

func browserAccept() http.Header {
	return http.Header{"Accept": {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"}}
}

func TestConsoleServesIndexAndAssets(t *testing.T) {
	srv := newSecurityServer(t, Security{}, WithConsole(fakeConsole()))

	index := doJSON(t, srv, http.MethodGet, "/", "", browserAccept())
	require.Equal(t, http.StatusOK, index.Code, index.Body.String())
	assert.Contains(t, index.Body.String(), "<title>console</title>")
	assert.Contains(t, index.Header().Get("Content-Type"), "text/html")
	// index.html 引用的是哈希文件名，缓存住它就会在升级后指向不存在的资源
	assert.Equal(t, "no-cache", index.Header().Get("Cache-Control"))

	asset := doJSON(t, srv, http.MethodGet, "/assets/index-abc.js", "", nil)
	require.Equal(t, http.StatusOK, asset.Code, asset.Body.String())
	assert.Equal(t, "console.log(1)", asset.Body.String())
	assert.Contains(t, asset.Header().Get("Content-Type"), "javascript")
	assert.Contains(t, asset.Header().Get("Cache-Control"), "immutable")

	// 资源缺失就是 404，不能用 index.html 兜：兜住会得到一个白屏 + 200 的 JS
	missing := doJSON(t, srv, http.MethodGet, "/assets/nope.js", "", nil)
	assert.Equal(t, http.StatusNotFound, missing.Code)
}

func TestConsoleFallbackIsScopedToBrowsersAndExcludesAPI(t *testing.T) {
	srv := newSecurityServer(t, Security{}, WithConsole(fakeConsole()))

	// SPA 深链：没有对应文件，交给 index.html，前端路由自己决定显示什么
	deep := doJSON(t, srv, http.MethodGet, "/jobs/01931f0a-0000-7000-8000-000000000000", "", browserAccept())
	require.Equal(t, http.StatusOK, deep.Code, deep.Body.String())
	assert.Contains(t, deep.Body.String(), "<!doctype html>")

	// REST 名字空间下的未知路径必须继续返回 JSON 404：给脚本一个 200 的网页，
	// 比 404 更难排查
	apiMiss := doJSON(t, srv, http.MethodGet, "/api/v1/does-not-exist", "", browserAccept())
	require.Equal(t, http.StatusNotFound, apiMiss.Code)
	assert.NotContains(t, apiMiss.Body.String(), "<!doctype html>")
	assert.Contains(t, apiMiss.Body.String(), "resource not found")

	// 不声明要 HTML 的客户端（curl、监控脚本）拿 404，而不是一个页面
	curl := doJSON(t, srv, http.MethodGet, "/jobs/whatever", "", http.Header{"Accept": {"*/*"}})
	assert.Equal(t, http.StatusNotFound, curl.Code)

	// 写方法永不兜底
	write := doJSON(t, srv, http.MethodPost, "/jobs/whatever", "{}", browserAccept())
	assert.Equal(t, http.StatusNotFound, write.Code)

	// 路径穿越在 clean 之后落回产物内部：这里收敛成 SPA 兜底，读不到外面的文件
	traversal := doJSON(t, srv, http.MethodGet, "/assets/../../secret.json", "", nil)
	assert.NotContains(t, strings.ToLower(traversal.Body.String()), "secret")
}

func TestConsoleAssetsStayPublicWhenAuthEnabled(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t), WithConsole(fakeConsole()))

	// 登录页本身也是产物的一部分：要求凭据就等于启用鉴权的部署连登录框都打不开
	assert.Equal(t, http.StatusOK, doJSON(t, srv, http.MethodGet, "/", "", browserAccept()).Code)
	assert.Equal(t, http.StatusOK, doJSON(t, srv, http.MethodGet, "/assets/index-abc.js", "", nil).Code)
	assert.Equal(t, http.StatusOK, doJSON(t, srv, http.MethodGet, "/login", "", browserAccept()).Code)

	// 豁免只到产物为止：REST、实时通道、以及任何写方法照旧要凭据
	assert.Equal(t, http.StatusUnauthorized, doJSON(t, srv, http.MethodGet, "/api/v1/stats", "", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, doJSON(t, srv, http.MethodGet, "/ws", "", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, doJSON(t, srv, http.MethodGet, "/sse/events", "", nil).Code)
	assert.Equal(t, http.StatusUnauthorized, doJSON(t, srv, http.MethodPost, "/", "", browserAccept()).Code)
}

func TestNoConsoleKeepsPlainAPIBehaviour(t *testing.T) {
	// 未注入产物（开发形态、或没带 -tags dashboard 的二进制）时行为与今天一致：
	// 不存在"页面能取到但接口被顺手放行"的缝隙
	plain := newSecurityServer(t, Security{})
	miss := doJSON(t, plain, http.MethodGet, "/", "", browserAccept())
	require.Equal(t, http.StatusNotFound, miss.Code)
	assert.Contains(t, miss.Body.String(), "resource not found")

	guarded := newSecurityServer(t, accountsSecurity(t))
	assert.Equal(t, http.StatusUnauthorized, doJSON(t, guarded, http.MethodGet, "/", "", browserAccept()).Code)
}
