package core

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// DefaultConfigPath 是约定的配置文件位置。它已被 .gitignore 排除（装着本机凭据），
// 入库的模板是同目录的 config.example.yaml；文件不存在不是错误，进程按代码默认值启动。
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
	// Executors 执行层（脚本/二进制/HTTP 任务）；默认关闭，详见 ExecutorsConfig。
	Executors ExecutorsConfig `mapstructure:"executors"`
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

// 执行器配置的默认值。执行能力默认关闭（ExecutorsConfig.Enabled），
// 关闭时这些取值全部不参与行为，只在打开之后生效。
const (
	DefaultExecConcurrency    = 4
	DefaultExecInlinePreview  = 2048
	DefaultExecMaxOutputBytes = 262144
	DefaultExecOutputDir      = "./data/exec"
	DefaultExecOutputTTL      = 7 * 24 * time.Hour
	DefaultExecWorkspace      = "./exec-workspace"
	DefaultExecRequiredRole   = "admin"
	DefaultExecRestorePolicy  = "pause"
	DefaultExecDefaultTimeout = 5 * time.Minute
	DefaultExecMaxTimeout     = 30 * time.Minute
)

// ExecutorsConfig 是执行层（脚本 / 二进制 / HTTP 任务）的配置。
// 设计依据见 docs/design/executor-design.md：可执行内容由配置声明的档位决定，
// 不提供自由命令行，因此 Enabled 默认 false——打开它等于把"能提交任务"扩展成"能执行命令"。
type ExecutorsConfig struct {
	// Enabled 执行器总开关。false 时一个 exec.* 处理函数都不注册，本节其它取值全部不生效。
	Enabled bool `mapstructure:"enabled"`

	// RequiredRole 提交执行器任务所需的最低档位：operator|admin|ops，空表示 DefaultExecRequiredRole。
	// machine（静态 token 的身份）的档位等同 operator，所以默认值 admin 把脚本凭据挡在执行能力之外；
	// viewer 被 Validate 拒绝：只读身份不该有执行能力。
	RequiredRole string `mapstructure:"required_role"`

	// Workspace 档位里 script/program/cwd 的根目录：这些相对路径解析后必须仍落在它之内。
	// 留空表示 DefaultExecWorkspace，不是"不限制目录"。
	Workspace string `mapstructure:"workspace"`

	// RuntimeAllow 允许档位使用的解释器或程序名白名单；空表示内置默认列表。
	RuntimeAllow []string `mapstructure:"runtime_allow"`

	// EnvAllow 传给子进程的环境变量键名白名单；空表示内置默认列表。
	// GODELAYQ_ 前缀的键即使在列表里也会被强制排除（见 Validate）：服务凭据不进子进程。
	EnvAllow []string `mapstructure:"env_allow"`

	// Concurrency 执行器专用执行池的协程数；0 表示 DefaultExecConcurrency。
	// 与普通任务分池的原因见设计文档 §6.5：共池时分钟级的脚本会长时间占住执行名额。
	Concurrency int `mapstructure:"concurrency"`

	// QueueCapacity 执行器队列容量；0 表示与 Concurrency 相等。
	// 队列满时任务留在堆与存储里等空位，不会无限堆积在内存。
	QueueCapacity int `mapstructure:"queue_capacity"`

	// DefaultTimeout 档位未声明 timeout 时的单次执行超时。
	// 打开 Enabled 后必须是正值：执行器任务不存在"不限制"，否则挂死的进程会一直占住名额。
	DefaultTimeout time.Duration `mapstructure:"default_timeout"`

	// MaxTimeout 单次执行超时的上限。档位或任务请求里超过它的值会被拒绝，而不是静默夹取——
	// 让调用方在提交时就看到上限，比执行到一半被中止更好排查。0 表示 DefaultExecMaxTimeout。
	MaxTimeout time.Duration `mapstructure:"max_timeout"`

	// RestorePolicy 重启时如何处理"崩溃瞬间仍在执行"的执行器任务：
	// pause（默认）置为 paused 等人工确认——副作用结果未知时不替人决定；
	// replay 则照常重新入队。其它取值由 Validate 拒绝。
	RestorePolicy string `mapstructure:"restore_policy"`

	// LoaderAllow 是否允许目录任务加载器接受 exec. 前缀的任务文件，默认 false。
	// 注意当前服务端二进制并不启用加载器（LoaderOptions 只在库使用与 examples/demo2 中出现），
	// 因此本项约束的是自行接入 DirectoryLoader 的程序，见设计文档 §6.9。
	LoaderAllow bool `mapstructure:"loader_allow"`

	// Output 执行输出的截断与保留策略
	Output ExecutorOutputConfig `mapstructure:"output"`

	// Commands 档位列表。正式结构在 executor 包（TASK-E02）定义，
	// 这里先用宽松类型打通配置解码；代价是档位内部的键名拼错此时不会被发现
	// （UnmarshalExact 只保证本节顶层键合法）。
	Commands []map[string]any `mapstructure:"commands"`
}

// ExecutorOutputConfig 执行输出（stdout/stderr）的落盘与预览参数。
type ExecutorOutputConfig struct {
	// InlinePreview 事件与任务快照里携带的输出尾部预览字节数；0 表示 DefaultExecInlinePreview。
	// 完整输出落在 Dir 下的产物文件，不进 jobs.json——那里每次落盘都是整文件重写。
	InlinePreview int `mapstructure:"inline_preview"`

	// MaxBytes 单条流（stdout 或 stderr）的落盘上限；0 表示 DefaultExecMaxOutputBytes。
	// 达到上限即停止写入并标记截断，不静默丢弃也不报错中断执行。
	MaxBytes int `mapstructure:"max_bytes"`

	// Dir 产物目录；空表示 DefaultExecOutputDir。与 store.path 分开放，便于单独设权限与清理。
	Dir string `mapstructure:"dir"`

	// TTL 产物保留时长，如 168h；0 表示不按时间清理（只做启动时的孤儿清理）。
	// 默认 DefaultExecOutputTTL；建议与 store.history_ttl 一起配置，见设计文档 §6.4。
	TTL time.Duration `mapstructure:"ttl"`
}

// Validate 校验执行器配置自身的取值。
//
// 只有 Enabled 为 true 时才收紧"必须显式配置"这类约束：关闭状态下 0 值与留空是正常写法，
// 不该逼每个部署都抄一遍完整配置。反向的例外是 loader_allow：它开了却没开执行器，
// 只能是配置写错，静默接受会让人以为文件投递通道已经打通。
func (e ExecutorsConfig) Validate() error {
	if role := strings.TrimSpace(e.RequiredRole); role != "" {
		parsed, ok := ParseRole(role)
		if !ok {
			return fmt.Errorf("executors.required_role %q is invalid, use operator|admin|ops", role)
		}
		if !parsed.AtLeast(RoleOperator) {
			// machine 不是可填档位，ParseRole 已经把它拒了；这里拦的是只读档
			return fmt.Errorf("executors.required_role must be operator, admin or ops, got %q", role)
		}
	}

	switch strings.TrimSpace(e.RestorePolicy) {
	case "", "pause", "replay":
	default:
		return fmt.Errorf("executors.restore_policy %q is invalid, use pause or replay", e.RestorePolicy)
	}

	if e.Workspace != "" && strings.TrimSpace(e.Workspace) == "" {
		return fmt.Errorf("executors.workspace must not be whitespace")
	}
	if e.Output.Dir != "" && strings.TrimSpace(e.Output.Dir) == "" {
		return fmt.Errorf("executors.output.dir must not be whitespace")
	}

	if err := checkExecNameList("runtime_allow", e.RuntimeAllow); err != nil {
		return err
	}
	if err := checkExecNameList("env_allow", e.EnvAllow); err != nil {
		return err
	}
	for _, key := range e.EnvAllow {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(key)), "GODELAYQ_") {
			// 服务端的凭据与 JWT 密钥都以 GODELAYQ_ 前缀出现在进程环境里，
			// 把它们列进子进程白名单是配置错误，不是运维选择
			return fmt.Errorf("executors.env_allow must not contain %q: GODELAYQ_ prefixed keys hold server credentials", key)
		}
	}

	if e.Concurrency < 0 {
		return fmt.Errorf("executors.concurrency must not be negative, got %d", e.Concurrency)
	}
	if e.QueueCapacity < 0 {
		return fmt.Errorf("executors.queue_capacity must not be negative, got %d", e.QueueCapacity)
	}
	if e.DefaultTimeout < 0 {
		return fmt.Errorf("executors.default_timeout must not be negative, got %v", e.DefaultTimeout)
	}
	if e.MaxTimeout < 0 {
		return fmt.Errorf("executors.max_timeout must not be negative, got %v", e.MaxTimeout)
	}
	if e.Output.InlinePreview < 0 {
		return fmt.Errorf("executors.output.inline_preview must not be negative, got %d", e.Output.InlinePreview)
	}
	if e.Output.MaxBytes < 0 {
		return fmt.Errorf("executors.output.max_bytes must not be negative, got %d", e.Output.MaxBytes)
	}
	if e.Output.TTL < 0 {
		return fmt.Errorf("executors.output.ttl must not be negative, got %v", e.Output.TTL)
	}

	if !e.Enabled {
		if e.LoaderAllow {
			return fmt.Errorf("executors.loader_allow requires executors.enabled to be true")
		}
		return nil
	}

	// 开启执行器后，0 值不再按"用默认值"放过：写显式 0 的人想要的是"不配置"，
	// 而这两者在执行器上后果差别很大，宁可报错让他删掉这一项或填个真实值。
	if e.Concurrency < 1 {
		return fmt.Errorf("executors.concurrency must be at least 1 when executors.enabled is true, got %d (omit the key to use %d)", e.Concurrency, DefaultExecConcurrency)
	}
	if e.DefaultTimeout <= 0 {
		return fmt.Errorf("executors.default_timeout must be positive when executors.enabled is true, got %v (omit the key to use %v)", e.DefaultTimeout, DefaultExecDefaultTimeout)
	}
	if e.MaxTimeout <= 0 {
		return fmt.Errorf("executors.max_timeout must be positive when executors.enabled is true, got %v (omit the key to use %v)", e.MaxTimeout, DefaultExecMaxTimeout)
	}
	if e.DefaultTimeout > e.MaxTimeout {
		return fmt.Errorf("executors.default_timeout %v must not exceed executors.max_timeout %v", e.DefaultTimeout, e.MaxTimeout)
	}
	if e.Output.MaxBytes < 1024 {
		return fmt.Errorf("executors.output.max_bytes must be at least 1024 when executors.enabled is true, got %d", e.Output.MaxBytes)
	}

	return nil
}

// checkExecNameList 校验白名单列表的每一项：不能是空白项、不能带首尾空格或控制字符。
// 这三条都会让白名单出现"看起来配了但永远匹配不上"的项，属于必须启动即报的写法。
func checkExecNameList(field string, list []string) error {
	for _, item := range list {
		if strings.TrimSpace(item) == "" {
			return fmt.Errorf("executors.%s must not contain an empty entry", field)
		}
		if item != strings.TrimSpace(item) {
			return fmt.Errorf("executors.%s entries must not have surrounding spaces, got %q", field, item)
		}
		if strings.ContainsFunc(item, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("executors.%s entries must not contain control characters, got %q", field, item)
		}
	}
	return nil
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
		Executors: ExecutorsConfig{
			// 默认关闭：打开执行器等于把任务提交权限扩展成命令执行权限，
			// 必须由部署方显式决定（Enabled 为 true 时同时应配置 server.auth）。
			Enabled:        false,
			RequiredRole:   DefaultExecRequiredRole,
			Workspace:      DefaultExecWorkspace,
			RuntimeAllow:   []string{"bash", "sh", "cmd", "pwsh", "node", "php", "python", "java"},
			EnvAllow:       []string{"PATH", "LANG", "LC_ALL", "TZ", "HOME"},
			Concurrency:    DefaultExecConcurrency,
			QueueCapacity:  0, // 与 Concurrency 相等
			DefaultTimeout: DefaultExecDefaultTimeout,
			MaxTimeout:     DefaultExecMaxTimeout,
			RestorePolicy:  DefaultExecRestorePolicy,
			LoaderAllow:    false,
			Output: ExecutorOutputConfig{
				InlinePreview: DefaultExecInlinePreview,
				MaxBytes:      DefaultExecMaxOutputBytes,
				Dir:           DefaultExecOutputDir,
				TTL:           DefaultExecOutputTTL,
			},
			Commands: nil,
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
		// 执行器：只绑定标量与简单列表。executors.commands 是"名字+参数+路径"的嵌套列表，
		// 与 server.auth.users 同一条限制：逗号分隔 hook 无法表达，且把可执行内容塞进
		// 环境变量很容易被 `ps eww` 之类的旁路读到。
		"executors.enabled",
		"executors.required_role",
		"executors.workspace",
		"executors.runtime_allow",
		"executors.env_allow",
		"executors.concurrency",
		"executors.queue_capacity",
		"executors.default_timeout",
		"executors.max_timeout",
		"executors.restore_policy",
		"executors.loader_allow",
		"executors.output.inline_preview",
		"executors.output.max_bytes",
		"executors.output.dir",
		"executors.output.ttl",
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

	if err := c.Executors.Validate(); err != nil {
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

	// 执行器：0 值与留空一律回到默认（Enabled 为 false 时这些取值本身不生效，
	// 但归一化后传给 executor 包就不必再各自判空补默认）。
	if c.Executors.RequiredRole == "" {
		c.Executors.RequiredRole = defaults.Executors.RequiredRole
	}
	if c.Executors.Workspace == "" {
		c.Executors.Workspace = defaults.Executors.Workspace
	}
	if len(c.Executors.RuntimeAllow) == 0 {
		c.Executors.RuntimeAllow = defaults.Executors.RuntimeAllow
	}
	if len(c.Executors.EnvAllow) == 0 {
		c.Executors.EnvAllow = defaults.Executors.EnvAllow
	}
	if c.Executors.Concurrency == 0 {
		c.Executors.Concurrency = defaults.Executors.Concurrency
	}
	if c.Executors.DefaultTimeout == 0 {
		c.Executors.DefaultTimeout = defaults.Executors.DefaultTimeout
	}
	if c.Executors.MaxTimeout == 0 {
		c.Executors.MaxTimeout = defaults.Executors.MaxTimeout
	}
	if c.Executors.RestorePolicy == "" {
		c.Executors.RestorePolicy = defaults.Executors.RestorePolicy
	}
	if c.Executors.Output.InlinePreview == 0 {
		c.Executors.Output.InlinePreview = defaults.Executors.Output.InlinePreview
	}
	if c.Executors.Output.MaxBytes == 0 {
		c.Executors.Output.MaxBytes = defaults.Executors.Output.MaxBytes
	}
	if c.Executors.Output.Dir == "" {
		c.Executors.Output.Dir = defaults.Executors.Output.Dir
	}
	return c
}
