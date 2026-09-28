package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Security 接入层安全参数。零值等价于历史行为：不鉴权 + 接受任意来源。
type Security struct {
	// AuthToken 非空即启用 token 鉴权
	AuthToken string
	// AllowOrigins CORS 来源白名单；为空或含 "*" 表示任意来源
	AllowOrigins []string
	// AllowCredentials 是否允许携带凭据；与 "*" 互斥（core.Config.Validate 已拒绝该组合）
	AllowCredentials bool
}

// authEnabled 表示是否配置了 token
func (s Security) authEnabled() bool {
	return s.AuthToken != ""
}

// tokenQueryKey 是浏览器 WebSocket / EventSource 无法自定义请求头时的替代通道
const tokenQueryKey = "token"

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

// authMiddleware 校验静态 token，未配置 token 时直接放行。
func (s *Server) authMiddleware() gin.HandlerFunc {
	want := []byte(s.sec.AuthToken)

	return func(c *gin.Context) {
		if len(want) == 0 {
			c.Next()
			return
		}

		presented, ok := requestToken(c.Request)
		if !ok || subtle.ConstantTimeCompare([]byte(presented), want) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
				Code:    http.StatusUnauthorized,
				Message: "invalid or missing token",
			})
			return
		}
		c.Next()
	}
}

// requestToken 按 Authorization: Bearer、X-Auth-Token、?token= 的顺序取凭据。
// 出现 Authorization 头就只认它：混合通道会让配错的客户端静默走 query，掩盖问题。
func requestToken(r *http.Request) (string, bool) {
	if auth := r.Header.Get("Authorization"); auth != "" {
		token, isBearer := strings.CutPrefix(auth, "Bearer ")
		if !isBearer {
			return "", false
		}
		if token = strings.TrimSpace(token); token != "" {
			return token, true
		}
		return "", false
	}
	if token := r.Header.Get("X-Auth-Token"); token != "" {
		return token, true
	}
	if token := r.URL.Query().Get(tokenQueryKey); token != "" {
		return token, true
	}
	return "", false
}
