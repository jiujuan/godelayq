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

// 控制台令牌的默认有效期。访问令牌取短，靠 refresh 续期，缩短被盗用窗口。
const (
	DefaultAccessTTL  = 15 * time.Minute
	DefaultRefreshTTL = 12 * time.Hour
)

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
	// Auth 控制台账号与机器凭据
	Auth AuthConfig `mapstructure:"auth"`
	// CORS 跨域来源白名单
	CORS CORSConfig `mapstructure:"cors"`
}

// AuthConfig 鉴权配置。Token 为空且 Users 为空表示不启用鉴权。
type AuthConfig struct {
	// Token 静态机器凭据，通过 GODELAYQ_SERVER_AUTH_TOKEN 注入更安全。
	// 它不再是控制台凭据：以它进来的调用方身份是 core.RoleMachine，
	// 能读写任务但不能强制暂停、删组或使用运维端点。
	Token string `mapstructure:"token"`
	// JWT 控制台令牌的签发参数；Users 非空时必填 Secret。
	JWT JWTConfig `mapstructure:"jwt"`
	// Users 控制台账号。账号只在此声明，改动需重启进程。
	Users []UserConfig `mapstructure:"users"`
}

// JWTConfig 访问令牌参数。密钥只走环境变量注入更安全。
type JWTConfig struct {
	// Secret HS256 签名密钥；轮换会使全部已发令牌立即失效
	Secret string `mapstructure:"secret"`
	// AccessTTL 访问令牌有效期，0 表示 DefaultAccessTTL
	AccessTTL time.Duration `mapstructure:"access_ttl"`
	// RefreshTTL 刷新令牌有效期，0 表示 DefaultRefreshTTL
	RefreshTTL time.Duration `mapstructure:"refresh_ttl"`
}

// UserConfig 一个控制台账号。密码只存 bcrypt 哈希，明文禁止入配置。
type UserConfig struct {
	// Name 登录名，配置内唯一
	Name string `mapstructure:"name"`
	// PasswordBcrypt bcrypt 哈希（可用 cmd/hashpassword 生成）
	PasswordBcrypt string `mapstructure:"password_bcrypt"`
	// Role viewer|operator|admin|ops
	Role string `mapstructure:"role"`
}

// Enabled 表示是否配置了任何凭据（静态 token 或账号）。
func (a AuthConfig) Enabled() bool {
	return a.Token != "" || len(a.Users) > 0
}

// Validate 检查账号列表自身的一致性。
// 哈希格式是否真是 bcrypt 留给 api 层在启动时用 bcrypt.Cost 校验：
// core 不该为了校验一个字符串而依赖 crypto 库。
func (a AuthConfig) Validate() error {
	if len(a.Users) == 0 {
		return nil
	}
	// 有账号却没有签名密钥，等于登录页能开但永远签不出令牌
	if a.JWT.Secret == "" {
		return fmt.Errorf("server.auth.jwt.secret must not be empty when server.auth.users is configured")
	}
	if len(a.JWT.Secret) < minJWTSecretLen {
		return fmt.Errorf("server.auth.jwt.secret must be at least %d bytes, got %d", minJWTSecretLen, len(a.JWT.Secret))
	}
	if a.JWT.AccessTTL < 0 {
		return fmt.Errorf("server.auth.jwt.access_ttl must not be negative, got %v", a.JWT.AccessTTL)
	}
	if a.JWT.RefreshTTL < 0 {
		return fmt.Errorf("server.auth.jwt.refresh_ttl must not be negative, got %v", a.JWT.RefreshTTL)
	}

	seen := make(map[string]bool, len(a.Users))
	for i, user := range a.Users {
		name := strings.TrimSpace(user.Name)
		if name == "" {
			return fmt.Errorf("server.auth.users[%d].name must not be empty", i)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("server.auth.users contains duplicate name %q (case-insensitive)", name)
		}
		seen[key] = true

		if user.PasswordBcrypt == "" {
			return fmt.Errorf("server.auth.users[%d].password_bcrypt must not be empty for user %q", i, name)
		}
		if _, ok := ParseRole(user.Role); !ok {
			return fmt.Errorf("server.auth.users[%d].role %q for user %q is invalid, use viewer|operator|admin|ops", i, user.Role, name)
		}
	}
	return nil
}

// minJWTSecretLen 是 HS256 密钥的最小长度；短密钥容易被离线爆破。
const minJWTSecretLen = 32

// ResolveRole 解析账号角色；非法取值由 Validate 拦截，调用方仍需处理 false。
func (u UserConfig) ResolveRole() (Role, bool) {
	return ParseRole(u.Role)
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
	// GroupsPath 分组元数据文件路径，与任务快照分文件存放：
	// 分组是低频实体，不该被 jobs.json 的高频合并写盘拖着一起重写。
	GroupsPath string `mapstructure:"groups_path"`
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
				Token: "", // 空即不启用静态凭据
				JWT: JWTConfig{
					Secret:     "", // 无账号时不需要密钥
					AccessTTL:  DefaultAccessTTL,
					RefreshTTL: DefaultRefreshTTL,
				},
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
			GroupsPath:    DefaultGroupsPath,
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
	// server.auth.users 故意不绑定环境变量：它是"名字+哈希+角色"的嵌套列表，
	// 逗号分隔 hook 无法表达，且把账号塞进环境变量很容易被 `ps eww` 之类的旁路读到。
	for _, key := range []string{
		"server.port",
		"server.auth.token",
		"server.auth.jwt.secret",
		"server.auth.jwt.access_ttl",
		"server.auth.jwt.refresh_ttl",
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
		"store.groups_path",
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
	if c.Store.GroupsPath == "" {
		return fmt.Errorf("store.groups_path must not be empty")
	}

	if _, err := parseLogLevel(c.Logging.Level); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(c.Logging.Format)) {
	case "", "text", "json":
	default:
		return fmt.Errorf("invalid logging.format %q, use text or json", c.Logging.Format)
	}

	if err := c.Server.Auth.Validate(); err != nil {
		return err
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
	if c.Store.GroupsPath == "" {
		c.Store.GroupsPath = defaults.Store.GroupsPath
	}
	if c.Server.Auth.JWT.AccessTTL == 0 {
		c.Server.Auth.JWT.AccessTTL = defaults.Server.Auth.JWT.AccessTTL
	}
	if c.Server.Auth.JWT.RefreshTTL == 0 {
		c.Server.Auth.JWT.RefreshTTL = defaults.Server.Auth.JWT.RefreshTTL
	}
	if c.Logging.Level == "" {
		c.Logging.Level = defaults.Logging.Level
	}
	if c.Logging.Format == "" {
		c.Logging.Format = defaults.Logging.Format
	}
	return c
}
