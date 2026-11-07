package core

import (
	"errors"
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
	logger, _, err := NewLoggerWithLevelVar(level, format, w)
	if err != nil {
		return nil, err
	}
	return logger, nil
}

// NewLoggerWithLevelVar 与 NewLogger 走同一条解析与 handler 构造，额外把级别载体交回调用方。
// 需要运行期改级别的装配方（cmd/server 的重载链，R06）用它；其余调用方继续用 NewLogger。
//
// 载体随 logger 一起建成，因此装配方调用 slog.SetDefault(logger) 之后改级别，
// 经 slog.Default() 的那一路（第三方库桥接、示例 handler）会一起跟着变——
// 这是本节的设计而不是副作用：换级别不换 logger 实例，只换它 handler 里的级别读数。
func NewLoggerWithLevelVar(level, format string, w io.Writer) (*slog.Logger, *slog.LevelVar, error) {
	parsedLevel, err := parseLogLevel(level)
	if err != nil {
		return nil, nil, err
	}

	levelVar := &slog.LevelVar{}
	levelVar.Set(parsedLevel)
	handlerOpts := &slog.HandlerOptions{Level: levelVar}

	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, handlerOpts)), levelVar, nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, handlerOpts)), levelVar, nil
	default:
		return nil, nil, fmt.Errorf("invalid logging.format %q, use text or json", format)
	}
}

// SetLogLevel 把级别载体改成 level（debug|info|warn|error，解析规则复用 parseLogLevel）。
// 解析失败时载体保持原值——重载路径上"一条写错的级别名"不该把正在跑的日志级别也带走。
// nil 载体返回错误而不是 panic：载体只可能来自 NewLoggerWithLevelVar，传 nil 是接线疏漏，
// 用 error 返回让 R06 的重载链按失败收口，而不是在写日志的半路崩掉进程。
func SetLogLevel(v *slog.LevelVar, level string) error {
	if v == nil {
		return errors.New("SetLogLevel requires a level var from NewLoggerWithLevelVar")
	}

	parsedLevel, err := parseLogLevel(level)
	if err != nil {
		return err
	}
	v.Set(parsedLevel)
	return nil
}
