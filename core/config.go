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
}

// ServerConfig HTTP 接入层配置
type ServerConfig struct {
	// Port 监听端口，如 "8080"
	Port string `mapstructure:"port"`
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
}

// DefaultConfig 返回与代码内置默认值一致的配置
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Port: "8080",
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
	for _, key := range []string{
		"server.port",
		"scheduler.workers",
		"scheduler.queue_capacity",
		"scheduler.max_retry_delay",
		"scheduler.shutdown_timeout",
		"store.type",
		"store.path",
		"store.flush_interval",
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

	return nil
}

// Normalized 把 0 值替换成代码默认值，便于直接传给各构造函数
func (c Config) Normalized() Config {
	defaults := DefaultConfig()

	if c.Server.Port == "" {
		c.Server.Port = defaults.Server.Port
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
	return c
}
