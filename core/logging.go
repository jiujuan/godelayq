package core

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Option 是组件构造时的可选配置，供 NewScheduler、NewWSServer 等共用。
type Option func(*componentOptions)

// componentOptions 汇集可选项的当前取值
type componentOptions struct {
	logger         *slog.Logger
	allowedOrigins []string
}

// WithLogger 为组件注入日志器；传 nil 表示沿用 slog.Default()，
// 这样既有测试与调用方无需改动即可获得输出。
func WithLogger(l *slog.Logger) Option {
	return func(o *componentOptions) { o.logger = l }
}

// WithAllowedOrigins 限制 WebSocket 握手的浏览器来源。
// 为空或含 "*" 表示接受任意来源；不带 Origin 头的客户端（Go/curl）始终放行。
func WithAllowedOrigins(origins ...string) Option {
	return func(o *componentOptions) { o.allowedOrigins = origins }
}

func newComponentOptions(opts ...Option) componentOptions {
	var o componentOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// resolveLogger 把 nil 归一化为进程默认日志器
func resolveLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

// parseLogLevel 解析日志级别名，仅接受 debug/info/warn/error
func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid logging.level %q, use debug, info, warn or error", level)
	}
}

// NewLogger 构建写 w 的 slog.Logger。
// level 取 debug|info|warn|error（空为 info），format 取 text|json（空为 text）；
// 其余取值返回错误，让配置问题在启动时即暴露。
func NewLogger(level, format string, w io.Writer) (*slog.Logger, error) {
	parsedLevel, err := parseLogLevel(level)
	if err != nil {
		return nil, err
	}

	handlerOpts := &slog.HandlerOptions{Level: parsedLevel}

	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, handlerOpts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, handlerOpts)), nil
	default:
		return nil, fmt.Errorf("invalid logging.format %q, use text or json", format)
	}
}
