package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"godelayq/core"

	"github.com/gin-gonic/gin"
)

// Security 接入层安全参数。零值等价于历史行为：不鉴权 + 接受任意来源。
type Security struct {
	// Auth 是 server.auth 配置段：机器 token、JWT 参数与控制台账号。
	// 任一凭据非空即启用鉴权。
	Auth core.AuthConfig
	// AllowOrigins CORS 来源白名单；为空或含 "*" 表示任意来源
	AllowOrigins []string
	// AllowCredentials 是否允许携带凭据；与 "*" 互斥（core.Config.Validate 已拒绝该组合）
	AllowCredentials bool
}

// authEnabled 表示是否配置了任何凭据（机器 token 或控制台账号）。
func (s Security) authEnabled() bool {
	return s.Auth.Enabled()
}

// 浏览器 WebSocket / EventSource 无法自定义请求头，因此实时通道多一条 query 入口：
//   - ticket：一次性短期票据，控制台先登录再申领，日志里留下的是一次性随机串；
//   - token：静态机器凭据，历史行为，会进访问日志，只推荐给脚本用。
const (
	tokenQueryKey  = "token"
	ticketQueryKey = "ticket"
)

// ticketPathPrefixes 把 ticket 限定在实时通道。
// ticket 是"一次连接"的凭据，不该被当成可复用的 REST 授权头使用。
var ticketPathPrefixes = []string{"/ws", "/sse/"}

// gin 上下文里存放已认证身份与访问令牌元数据的键。
const (
	principalKey     = "godelayq.principal"
	tokenInfoKey     = "godelayq.token_info"
	anonymousName    = "anonymous"
	ticketTTLSeconds = int64(ticketTTL / time.Second)
)

// setPrincipal 写入已认证身份；info 对机器凭据与匿名请求为零值。
func setPrincipal(c *gin.Context, p Principal, info TokenInfo) {
	c.Set(principalKey, p)
	c.Set(tokenInfoKey, info)
}

// PrincipalFrom 取出当前请求的身份。未启用鉴权时返回 false，
// 调用方（审计日志等）应把这种情况理解为"匿名"，而不是"越权"。
func PrincipalFrom(c *gin.Context) (Principal, bool) {
	value, ok := c.Get(principalKey)
	if !ok {
		return Principal{}, false
	}
	p, ok := value.(Principal)
	return p, ok
}

// tokenInfoFrom 取出访问令牌元数据（jti 与过期时间），供 /auth/me 与登出使用。
func tokenInfoFrom(c *gin.Context) TokenInfo {
	value, ok := c.Get(tokenInfoKey)
	if !ok {
		return TokenInfo{}
	}
	info, _ := value.(TokenInfo)
	return info
}

// corsMiddleware 按白名单决定是否回显 Origin。
// 带凭据时必须回显具体来源，"*" 会被浏览器拒绝，因此这里做安全降级。
func corsMiddleware(sec Security) gin.HandlerFunc {
	wildcard := len(sec.AllowOrigins) == 0
	allowed := make(map[string]bool, len(sec.AllowOrigins))
	for _, origin := range sec.AllowOrigins {
		if origin == "*" {
			wildcard = true
			continue
		}
		allowed[strings.ToLower(origin)] = true
	}

	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")

		switch {
		case wildcard && !sec.AllowCredentials:
			c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		case origin != "" && (wildcard || allowed[strings.ToLower(origin)]):
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Add("Vary", "Origin")
		}

		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Auth-Token")
		if sec.AllowCredentials {
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		// 预检请求不会携带凭据，必须在鉴权之前答复
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// publicAuthEndpoints 是不需要凭据就能访问的端点（登录与刷新）。
//
// 用精确的 "METHOD path" 集合而不是前缀匹配：认证中间件是 engine.Use 全局挂的
// （api/server.go 的 setupMiddleware），一旦写成"以 /auth 开头就放行"，
// 将来在 /auth 下新增任何端点都会被静默免鉴权。
var publicAuthEndpoints = map[string]bool{
	http.MethodPost + " /api/v1/auth/login":   true,
	http.MethodPost + " /api/v1/auth/refresh": true,
}

// authMiddleware 把请求解析成 Principal 并放进上下文；未配置任何凭据时全部放行
// （等价于历史行为：本地自托管、单机自用，不区分身份）。
func (s *Server) authMiddleware() gin.HandlerFunc {
	want := []byte(s.sec.Auth.Token)

	return func(c *gin.Context) {
		if !s.sec.authEnabled() {
			// 未启用鉴权 = 单机自托管，所有请求视作同一个本机用户。
			// 给一个 ops 档的匿名身份是为了让角色判断与审计日志有值可读，
			// 不是因为"没配凭据就该谁都能调运维端点"——那种部署下根本没有需要隔离的对象。
			setPrincipal(c, Principal{Name: anonymousName, Role: core.RoleOps}, TokenInfo{})
			c.Next()
			return
		}

		if publicAuthEndpoints[c.Request.Method+" "+c.Request.URL.Path] {
			// 登录端点自己带凭据（用户名+密码），这里只负责不拦它
			c.Next()
			return
		}

		if s.consoleRequest(c) {
			// 嵌入的前端产物不要求凭据：登录页本身就是产物的一部分，这里拦下来
			// 等于启用鉴权的部署连登录框都打不开。豁免范围由 consoleRequest 收紧在
			// "GET/HEAD + 不在 /api、/ws、/sse 名字空间里"，任何写方法都不通过。
			// 不设置 Principal：这些路径上没有业务处理器，也就没有可判定的档位。
			c.Next()
			return
		}

		principal, info, ok := s.authenticate(c.Request, want)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
				Code:    http.StatusUnauthorized,
				Message: "invalid or missing credentials",
			})
			return
		}

		setPrincipal(c, principal, info)
		c.Next()
	}
}

// authenticate 依次尝试凭据通道：请求头 → ticket（仅实时通道）→ ?token=（仅机器凭据）。
//
// 出现 Authorization 头就只认它：混合通道会让配错的客户端静默降级到 query，掩盖问题。
func (s *Server) authenticate(r *http.Request, want []byte) (Principal, TokenInfo, bool) {
	if presented, headerSeen, ok := headerCredential(r); headerSeen {
		if !ok {
			return Principal{}, TokenInfo{}, false
		}
		return s.verifyPresented(presented, want)
	}

	if ticket := r.URL.Query().Get(ticketQueryKey); ticket != "" {
		if !ticketAllowedPath(r.URL.Path) {
			return Principal{}, TokenInfo{}, false
		}
		principal, err := s.tokens.consumeTicket(ticket)
		if err != nil {
			return Principal{}, TokenInfo{}, false
		}
		// ticket 是一次性的，用完就没有可查的元数据；过期时间给不出，交由
		// 调用方（实时通道）在断连时重新申领。
		return principal, TokenInfo{}, true
	}

	if token := r.URL.Query().Get(tokenQueryKey); token != "" {
		// query 通道只接受机器凭据：JWT 出现在 URL 里会被访问日志原样记下
		// （api/logging.go 同时打印 path 与 query），长令牌进日志等于泄露。
		principal, ok := s.verifyMachineToken(token, want)
		return principal, TokenInfo{}, ok
	}

	return Principal{}, TokenInfo{}, false
}

// verifyPresented 先按 JWT 验签（配了账号才有认证器），再回落到机器凭据比对。
func (s *Server) verifyPresented(presented string, want []byte) (Principal, TokenInfo, bool) {
	if s.auth != nil {
		if principal, info, err := s.auth.VerifyAccessToken(presented, s.tokens); err == nil {
			return principal, info, true
		}
	}
	principal, ok := s.verifyMachineToken(presented, want)
	return principal, TokenInfo{}, ok
}

// verifyMachineToken 对静态 token 做恒定时间比较，命中即 machine 身份。
func (s *Server) verifyMachineToken(presented string, want []byte) (Principal, bool) {
	if len(want) == 0 || subtle.ConstantTimeCompare([]byte(presented), want) != 1 {
		return Principal{}, false
	}
	return Principal{Name: machinePrincipalName, Role: core.RoleMachine}, true
}

// headerCredential 按 Authorization: Bearer、X-Auth-Token 的顺序取凭据。
// 返回的 ok 为 false 表示头存在但形态不对（此时不回退到 query）。
func headerCredential(r *http.Request) (presented string, headerSeen bool, ok bool) {
	if auth := r.Header.Get("Authorization"); auth != "" {
		token, isBearer := strings.CutPrefix(auth, "Bearer ")
		if !isBearer {
			return "", true, false
		}
		token = strings.TrimSpace(token)
		return token, true, token != ""
	}
	if token := r.Header.Get("X-Auth-Token"); token != "" {
		return token, true, true
	}
	return "", false, false
}

// ticketAllowedPath 判断当前路径能否使用 ticket。
func ticketAllowedPath(path string) bool {
	for _, prefix := range ticketPathPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// RequireRole 放行达到 min 档位的身份。
//
// 未启用鉴权时直接放行：那时所有请求都是同一个本机用户的请求，
// 区分角色没有意义，拒绝反而会让既有单用户部署升级后失能。
func (s *Server) RequireRole(min core.Role) gin.HandlerFunc {
	return s.require(func(p Principal) bool { return p.Role.AtLeast(min) })
}

// require 承载角色比较：认证已通过后，档位不够就是 403。
func (s *Server) require(check func(Principal) bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.sec.authEnabled() {
			c.Next()
			return
		}

		principal, ok := PrincipalFrom(c)
		if !ok {
			// 认证中间件正常会先拒绝；走到这里说明有人漏挂了 authMiddleware，
			// 按最小权限处理，绝不因为"内部约定"就默认放行。
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
				Code:    http.StatusUnauthorized,
				Message: "invalid or missing credentials",
			})
			return
		}
		if !check(principal) {
			s.logAccessRejection(principal, "route role requirement")
			c.AbortWithStatusJSON(http.StatusForbidden, ErrorResponse{
				Code:    http.StatusForbidden,
				Message: "insufficient role",
			})
			return
		}

		c.Next()
	}
}

// allowRole 判断当前请求是否达到 min 档位；未启用鉴权时视为达到。
//
// 只给"档位要求取决于请求体"的端点用（例如 batch-ops 里混进 force-pause）：
// 中间件在解析 body 之前就得决定放不放行，这类判断只能落在处理器里。
// 路由上写死的档位一律走 RequireRole，不要两处各判一套。
func (s *Server) allowRole(c *gin.Context, min core.Role) bool {
	if !s.sec.authEnabled() {
		return true
	}
	principal, ok := PrincipalFrom(c)
	return ok && principal.Role.AtLeast(min)
}

// logAccessRejection 记录被权限层挡下的请求，便于事后排查配错的角色。
func (s *Server) logAccessRejection(p Principal, why string) {
	if s.logger == nil {
		return
	}
	s.logger.Warn("access denied", "who", p.Name, "role", p.Role.String(), "need", why)
}
