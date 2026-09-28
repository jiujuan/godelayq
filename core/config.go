package core

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// DefaultConfigPath 是约定的配置文件位置
const DefaultConfigPath = "configs/config.yaml"

// Config 是进程的可配置项。只收录当前真正生效的字段：
// 未实现的选项（存储后端类型、加载器、WebSocket 上限等）故意不出现，
// LoadConfig 使用精确解码，写入未知键会直接报错而不是被忽略。
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Scheduler SchedulerConfig `mapstructure:"scheduler"`
	Store     StoreConfig     `mapstructure:"store"`
	Logging   LoggingConfig   `mapstructure:"logging"`
}

// ServerConfig HTTP 接入层配置
type ServerConfig struct {
	// Port 监听端口，如 "8080"
	Port string `mapstructure:"port"`
	// Auth 静态 token 鉴权
	Auth AuthConfig `mapstructure:"auth"`
	// CORS 跨域来源白名单
	CORS CORSConfig `mapstructure:"cors"`
}

// AuthConfig 鉴权配置。Token 为空表示不启用鉴权。
type AuthConfig struct {
	// Token 访问受保护端点所需的静态 token，通过 GODELAYQ_SERVER_AUTH_TOKEN 注入更安全
	Token string `mapstructure:"token"`
}

// CORSConfig 跨域配置。AllowOrigins 为空等价于 ["*"]（历史行为）。
type CORSConfig struct {
	// AllowOrigins 允许的来源列表，元素为完整 origin（如 https://app.example.com）或 "*"
	AllowOrigins []string `mapstructure:"allow_origins"`
	// AllowCredentials 是否允许携带凭据；开启时不允许使用 "*" 来源
	AllowCredentials bool `mapstructure:"allow_credentials"`
}

// SchedulerConfig 调度与执行侧配置
type SchedulerConfig struct {
	// Workers 并发执行协程数，0 表示 DefaultConcurrency
	Workers int `mapstructure:"workers"`
	// QueueCapacity 执行队列容量，0 表示与 Workers 相等；队列满时调度循环阻塞入队
	QueueCapacity int `mapstructure:"queue_capacity"`
	// MaxRetryDelay 指数退避的单次重试延迟上限，0 表示不限制
	MaxRetryDelay time.Duration `mapstructure:"max_retry_delay"`
	// ShutdownTimeout 优雅关闭等待时长，必须为正
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

// StoreConfig 持久化配置
type StoreConfig struct {
	// Type 存储后端，目前仅支持 "json"
	Type string `mapstructure:"type"`
	// Path JSON 存储文件路径
	Path string `mapstructure:"path"`
	// FlushInterval 合并落盘周期，0 表示 DefaultFlushInterval
	FlushInterval time.Duration `mapstructure:"flush_interval"`
	// HistoryLimit 终态（成功/失败/取消）快照保留条数，
	// 0 表示 DefaultHistoryLimit，-1 表示不留痕（写入即删除）
	HistoryLimit int `mapstructure:"history_limit"`
	// HistoryTTL 终态快照保留时长，0 表示不按时间淘汰
	HistoryTTL time.Duration `mapstructure:"history_ttl"`
}

// LoggingConfig 日志输出配置，作用于调度器、存储、WebSocket 与 HTTP 访问日志
type LoggingConfig struct {
	// Level 最低输出级别：debug|info|warn|error
	Level string `mapstructure:"level"`
	// Format 输出格式：text|json
	Format string `mapstructure:"format"`
}

// DefaultConfig 返回与代码内置默认值一致的配置
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Port: "8080",
			Auth: AuthConfig{
				Token: "", // 空即不启用鉴权
			},
			CORS: CORSConfig{
				AllowOrigins:     []string{"*"},
				AllowCredentials: false,
			},
		},
		Scheduler: SchedulerConfig{
			Workers:         DefaultConcurrency,
			QueueCapacity:   0, // 与 Workers 相等
			MaxRetryDelay:   30 * time.Minute,
			ShutdownTimeout: 5 * time.Second,
		},
		Store: StoreConfig{
			Type:          "json",
			Path:          "./data/jobs.json",
			FlushInterval: DefaultFlushInterval,
			HistoryLimit:  DefaultHistoryLimit,
			HistoryTTL:    0,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// LoadConfig 读取配置：以默认值为底，叠加配置文件与环境变量。
// path 为空时按约定查找 configs/config.{yaml,yml}；该文件不存在不算错误。
// 显式指定的文件缺失或格式非法则返回错误。
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	v := viper.New()
	v.SetConfigType("yaml")

	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("config")
		v.AddConfigPath("configs")
	}

	// 环境变量覆盖：GODELAYQ_SERVER_PORT / GODELAYQ_SCHEDULER_WORKERS ...
	// 列表型（allow_origins）由 viper 默认的逗号分隔 hook 解析。
	for _, key := range []string{
		"server.port",
		"server.auth.token",
		"server.cors.allow_origins",
		"server.cors.allow_credentials",
		"scheduler.workers",
		"scheduler.queue_capacity",
		"scheduler.max_retry_delay",
		"scheduler.shutdown_timeout",
		"store.type",
		"store.path",
		"store.flush_interval",
		"store.history_limit",
		"store.history_ttl",
		"logging.level",
		"logging.format",
	} {
		if err := v.BindEnv(key, "GODELAYQ_"+strings.ToUpper(strings.ReplaceAll(key, ".", "_"))); err != nil {
			return cfg, fmt.Errorf("bind env for %q failed: %w", key, err)
		}
	}

	if err := v.ReadInConfig(); err != nil {
		// 没有配置文件是正常情况，用默认值继续；显式指定的文件缺失则报错
		var notFound viper.ConfigFileNotFoundError
		if !(path == "" && errors.As(err, &notFound)) {
			return cfg, fmt.Errorf("read config failed: %w", err)
		}
	} else if err := v.UnmarshalExact(&cfg); err != nil {
		// 精确解码：拼错的键应当报错，而不是静默失效
		return cfg, fmt.Errorf("parse config failed: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// Validate 校验取值范围，并把 0 值补齐为代码默认值
func (c Config) Validate() error {
	if c.Server.Port == "" {
		return fmt.Errorf("server.port must not be empty")
	}

	if c.Scheduler.Workers < 0 {
		return fmt.Errorf("scheduler.workers must not be negative, got %d", c.Scheduler.Workers)
	}
	if c.Scheduler.QueueCapacity < 0 {
		return fmt.Errorf("scheduler.queue_capacity must not be negative, got %d", c.Scheduler.QueueCapacity)
	}
	if c.Scheduler.MaxRetryDelay < 0 {
		return fmt.Errorf("scheduler.max_retry_delay must not be negative, got %v", c.Scheduler.MaxRetryDelay)
	}
	if c.Scheduler.ShutdownTimeout <= 0 {
		return fmt.Errorf("scheduler.shutdown_timeout must be positive, got %v", c.Scheduler.ShutdownTimeout)
	}

	switch c.Store.Type {
	case "json":
	case "":
		return fmt.Errorf("store.type must not be empty")
	default:
		return fmt.Errorf("unsupported store.type %q, only \"json\" is implemented", c.Store.Type)
	}

	if c.Store.Path == "" {
		return fmt.Errorf("store.path must not be empty")
	}
	if c.Store.FlushInterval < 0 {
		return fmt.Errorf("store.flush_interval must not be negative, got %v", c.Store.FlushInterval)
	}
	// -1 是"不留痕"的哨兵值，比 -1 更小没有含义；0 表示使用默认条数
	if c.Store.HistoryLimit < -1 {
		return fmt.Errorf("store.history_limit must be -1 (keep nothing), 0 (default) or positive, got %d", c.Store.HistoryLimit)
	}
	if c.Store.HistoryTTL < 0 {
		return fmt.Errorf("store.history_ttl must not be negative, got %v", c.Store.HistoryTTL)
	}

	if _, err := parseLogLevel(c.Logging.Level); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(c.Logging.Format)) {
	case "", "text", "json":
	default:
		return fmt.Errorf("invalid logging.format %q, use text or json", c.Logging.Format)
	}

	for _, origin := range c.Server.CORS.AllowOrigins {
		if strings.TrimSpace(origin) == "" {
			return fmt.Errorf("server.cors.allow_origins must not contain an empty entry")
		}
	}
	if c.Server.CORS.AllowCredentials && containsOrigin(c.Server.CORS.AllowOrigins, "*") {
		// 浏览器会拒绝 "*"+凭据 的组合，配置层面直接否掉而不是运行时静默降级
		return fmt.Errorf("server.cors.allow_credentials cannot be combined with the \"*\" origin")
	}

	return nil
}

// containsOrigin 判断列表里是否有指定来源（忽略首尾空白）
func containsOrigin(origins []string, want string) bool {
	for _, o := range origins {
		if strings.TrimSpace(o) == want {
			return true
		}
	}
	return false
}

// Normalized 把 0 值替换成代码默认值，便于直接传给各构造函数
func (c Config) Normalized() Config {
	defaults := DefaultConfig()

	if c.Server.Port == "" {
		c.Server.Port = defaults.Server.Port
	}
	if len(c.Server.CORS.AllowOrigins) == 0 {
		c.Server.CORS.AllowOrigins = defaults.Server.CORS.AllowOrigins
	}
	if c.Scheduler.Workers == 0 {
		c.Scheduler.Workers = defaults.Scheduler.Workers
	}
	if c.Scheduler.ShutdownTimeout <= 0 {
		c.Scheduler.ShutdownTimeout = defaults.Scheduler.ShutdownTimeout
	}
	if c.Store.Type == "" {
		c.Store.Type = defaults.Store.Type
	}
	if c.Store.Path == "" {
		c.Store.Path = defaults.Store.Path
	}
	if c.Store.FlushInterval == 0 {
		c.Store.FlushInterval = defaults.Store.FlushInterval
	}
	if c.Store.HistoryLimit == 0 {
		c.Store.HistoryLimit = defaults.Store.HistoryLimit
	}
	if c.Logging.Level == "" {
		c.Logging.Level = defaults.Logging.Level
	}
	if c.Logging.Format == "" {
		c.Logging.Format = defaults.Logging.Format
	}
	return c
}
