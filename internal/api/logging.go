package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
)

// requestLogger 记录访问日志：4xx 记 warn、5xx 记 error，其余 info。
// 取代 gin.LoggerWithFormatter，让 HTTP 日志与进程内其他日志同格式、同级别。
func requestLogger(l *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		attrs := []any{
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			// JSON 格式会把 Duration 编码成纳秒整数，改用毫秒更利于观测与索引
			"latency_ms", float64(time.Since(start).Microseconds()) / 1000.0,
			"client_ip", c.ClientIP(),
		}
		if c.Request.URL.RawQuery != "" {
			attrs = append(attrs, "query", c.Request.URL.RawQuery)
		}
		// 审计：谁、以什么身份、动了哪个端点。设计文档 §5.7.6 只要求写操作留痕，
		// 这里对所有请求都记 —— 认证中间件在下游，c.Next() 之后身份一定已就位，
		// 多记一份读操作的开销远小于"事后发现查不到是谁看过"的代价。
		if principal, ok := PrincipalFrom(c); ok {
			attrs = append(attrs, "who", principal.Name, "role", principal.Role.String())
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "error", c.Errors.String())
		}

		status := c.Writer.Status()
		switch {
		case status >= 500:
			l.Error("http request", attrs...)
		case status >= 400:
			l.Warn("http request", attrs...)
		default:
			l.Info("http request", attrs...)
		}
	}
}

// recovery 捕获处理器 panic，记入结构化日志后返回 500。
// 响应头已下发时只中断链路，避免再写一次状态码。
func recovery(l *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				l.Error("panic recovered",
					"error", fmt.Sprint(err),
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"stack", string(debug.Stack()))

				if c.Writer.Written() {
					c.Abort()
					return
				}
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()

		c.Next()
	}
}
