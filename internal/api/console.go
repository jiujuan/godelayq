package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// 嵌入形态（设计文档 §5.8 的方案 B）：前端产物由同一个进程、同一套路由提供，
// 不存在跨域，也不需要 vite 那条代理。文件本身在 web/embed_dashboard.go 里，
// 只有 -tags dashboard 的构建才带得动。

// consoleIndex 是 SPA 入口在产物根目录里的文件名。
const consoleIndex = "index.html"

// assetsPrefix 是 Vite 产物的资源前缀。文件名带内容哈希，因此可以放到最长缓存；
// 反过来说 index.html 绝对不能缓存，否则升级后旧页面还在引用已被换掉的哈希名。
const assetsPrefix = "/assets/"

// WithConsole 注入嵌入的前端产物根。传 nil 表示这次部署只跑 API：
// 开发形态下前端由 vite dev server 提供，同源托管这条路径整个关掉。
func WithConsole(dist fs.FS) Option {
	return func(s *Server) { s.console = dist }
}

// consoleRequest 判定这个请求是否应由前端产物应答。
//
// 鉴权豁免与 NoRoute 分派必须共用这一个判断：两边口径一旦不同，就会出现
// "中间件放行了却没有处理器接管"（未认证请求拿到 JSON 404，前端把它当页面不存在），
// 或者反过来的"页面能取到但先被 401 弹回"。
func (s *Server) consoleRequest(c *gin.Context) bool {
	if s.console == nil {
		return false
	}
	switch c.Request.Method {
	case http.MethodGet, http.MethodHead:
	default:
		return false
	}

	cleaned := cleanRequestPath(c.Request.URL.Path)
	if cleaned == "/" || strings.HasPrefix(cleaned, assetsPrefix) {
		return true
	}
	// REST 与实时通道前缀下的未知路径继续走 JSON 404 / 401：给脚本客户端
	// 返回一个 200 的网页，比 404 更难排查。
	if isProtectedPath(cleaned) {
		return false
	}
	// SPA 深链（/jobs/<id>、/groups）没有对应文件，只有浏览器导航会带 text/html；
	// 不带这个头的客户端拿到 404 才是诚实的回答。
	return strings.Contains(c.GetHeader("Accept"), "text/html")
}

// serveConsole 把请求映射到产物里的文件。静态资源缺失返回 404，
// 其余非受保护路径一律兜底到 index.html，由前端路由决定显示什么。
func (s *Server) serveConsole(c *gin.Context) {
	name := strings.TrimPrefix(cleanRequestPath(c.Request.URL.Path), "/")
	if name == "" || name == "." {
		name = consoleIndex
	}
	isAsset := strings.HasPrefix(name, strings.TrimPrefix(assetsPrefix, "/"))
	if !isAsset {
		name = consoleIndex
	}

	if isAsset {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		c.Header("Cache-Control", "no-cache")
	}
	// ServeFileFS 自己处理 Content-Type、Range、Last-Modified 与缺失文件的 404；
	// 路径已由 cleanRequestPath 收敛到 FS 内部，越不出产物目录。
	http.ServeFileFS(c.Writer, c.Request, s.console, name)
}

// cleanRequestPath 把请求路径收敛成 FS 可用的相对形态：连续的 .. 与 . 全部消解，
// 结果始终以 / 开头。
func cleanRequestPath(p string) string {
	cleaned := path.Clean(p)
	if !strings.HasPrefix(cleaned, "/") {
		return "/" + cleaned
	}
	return cleaned
}

// isProtectedPath 报告路径是否落在 REST / 实时通道的名字空间里。
// 判定与已注册的路由保持一致（见 api/server.go 的 setupRoutes）。
func isProtectedPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || p == "/ws" || strings.HasPrefix(p, "/sse/")
}
