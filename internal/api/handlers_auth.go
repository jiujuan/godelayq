package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// errAccountsNotConfigured 让"没有账号体系"成为一种可诊断的失败，
// 而不是让登录页对着 401 猜自己密码错了。
var errAccountsNotConfigured = errors.New("console accounts are not configured")

// Login POST /api/v1/auth/login
//
// 失败原因一律折叠成同一条 401 文案：区分"用户不存在"与"密码错误"等于
// 把账号枚举的活儿交给攻击者。限流独立成 429，因为它要告诉客户端何时重试。
func (s *Server) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid request body",
			Details: err.Error(),
		})
		return
	}

	if s.auth == nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "console accounts are not configured",
			Details: "set server.auth.users (and server.auth.jwt.secret) to enable the web console",
		})
		return
	}

	ip := c.ClientIP()
	allowed, retryAfter := s.loginLimit.Allow(ip, req.Username)
	if !allowed {
		seconds := int64(retryAfter.Round(time.Second).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		c.Header("Retry-After", strconv.FormatInt(seconds, 10))
		c.JSON(http.StatusTooManyRequests, ErrorResponse{
			Code:    http.StatusTooManyRequests,
			Message: "too many failed login attempts",
			Details: "retry after " + strconv.FormatInt(seconds, 10) + "s",
		})
		return
	}

	principal, err := s.auth.Authenticate(req.Username, req.Password)
	if err != nil {
		s.loginLimit.Reject(ip, req.Username)
		s.logger.Warn("login failed", "who", req.Username, "client_ip", ip)
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
			Code:    http.StatusUnauthorized,
			Message: "invalid username or password",
		})
		return
	}
	s.loginLimit.Reset(ip, req.Username)

	session, err := s.auth.IssueSession(principal, s.tokens)
	if err != nil {
		s.logger.Error("failed to issue session", "who", principal.Name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to create session",
		})
		return
	}

	s.logger.Info("login succeeded", "who", principal.Name, "role", principal.Role.String())
	c.JSON(http.StatusOK, sessionResponse(session))
}

// Refresh POST /api/v1/auth/refresh
// refresh token 是一次性的：每次刷新都签发新的一对，旧的立即作废。
func (s *Server) Refresh(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: "invalid request body",
			Details: err.Error(),
		})
		return
	}

	if s.auth == nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    http.StatusBadRequest,
			Message: errAccountsNotConfigured.Error(),
		})
		return
	}

	session, err := s.auth.Refresh(req.RefreshToken, s.tokens)
	if err != nil {
		// 未知/已用/过期的 refresh token 与密码错误同级别敏感，回同一句模糊文案
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
			Code:    http.StatusUnauthorized,
			Message: "invalid refresh token",
		})
		return
	}

	c.JSON(http.StatusOK, sessionResponse(session))
}

// Logout POST /api/v1/auth/logout
// 吊销 refresh token，并把当前 access token 记入拒绝表，让它立刻（而不只是 15 分钟后）失效。
func (s *Server) Logout(c *gin.Context) {
	var req LogoutRequest
	// 登出体可选：机器凭据或旧客户端不带 refresh_token 也要能退出
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Code:    http.StatusBadRequest,
				Message: "invalid request body",
				Details: err.Error(),
			})
			return
		}
	}

	if s.auth != nil {
		presented, _, ok := headerCredential(c.Request)
		access := ""
		if ok {
			access = presented
		}
		s.auth.Logout(access, req.RefreshToken, s.tokens)
	}

	c.Status(http.StatusNoContent)
}

// Me GET /api/v1/auth/me
// 前端以这里为权威身份，不去解析 JWT 的 claims：
// 令牌里写着 admin 而账号已被降权时，正确答案来自服务端当前配置。
func (s *Server) Me(c *gin.Context) {
	principal, ok := PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, ErrorResponse{
			Code:    http.StatusUnauthorized,
			Message: "invalid or missing credentials",
		})
		return
	}

	response := WhoAmIResponse{
		UserInfoResponse: UserInfoResponse{Name: principal.Name, Role: principal.Role.String()},
	}
	if info := tokenInfoFrom(c); !info.ExpiresAt.IsZero() {
		response.ExpiresAt = info.ExpiresAt
	}
	c.JSON(http.StatusOK, response)
}

// WsTicket POST /api/v1/auth/ws-ticket
// 浏览器的 WebSocket / EventSource 不能自定义请求头，而把访问令牌写进 URL
// 会连着 query 一起进访问日志（api/logging.go）。这里换成一次一用、5 秒过期的票据。
func (s *Server) WsTicket(c *gin.Context) {
	principal, ok := PrincipalFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, ErrorResponse{
			Code:    http.StatusUnauthorized,
			Message: "invalid or missing credentials",
		})
		return
	}

	ticket, err := s.tokens.issueTicket(principal)
	if err != nil {
		s.logger.Error("failed to issue websocket ticket", "who", principal.Name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to issue ticket",
		})
		return
	}

	c.JSON(http.StatusOK, WsTicketResponse{Ticket: ticket, ExpiresInSeconds: ticketTTLSeconds})
}

// sessionResponse 把内部 Session 转成对外结构。
func sessionResponse(session Session) TokenSessionResponse {
	return TokenSessionResponse{
		AccessToken:  session.AccessToken,
		RefreshToken: session.RefreshToken,
		TokenType:    "Bearer",
		ExpiresAt:    session.ExpiresAt,
		User:         UserInfoResponse{Name: session.Principal.Name, Role: session.Principal.Role.String()},
	}
}
