package core

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.Equal(t, "8080", cfg.Server.Port)
	assert.Equal(t, DefaultConcurrency, cfg.Scheduler.Workers)
	assert.Equal(t, 0, cfg.Scheduler.QueueCapacity)
	assert.Equal(t, 30*time.Minute, cfg.Scheduler.MaxRetryDelay)
	assert.Equal(t, 5*time.Second, cfg.Scheduler.ShutdownTimeout)
	assert.Equal(t, "json", cfg.Store.Type)
	assert.Equal(t, "./data/jobs.json", cfg.Store.Path)
	assert.Equal(t, DefaultFlushInterval, cfg.Store.FlushInterval)
	assert.Equal(t, DefaultHistoryLimit, cfg.Store.HistoryLimit)
	assert.Zero(t, cfg.Store.HistoryTTL)
	assert.Empty(t, cfg.Server.Auth.Token, "auth must stay off until a token is configured")
	assert.Equal(t, []string{"*"}, cfg.Server.CORS.AllowOrigins)
	assert.False(t, cfg.Server.CORS.AllowCredentials)

	assert.NoError(t, cfg.Validate())
}

func TestLoadConfig_FromFile(t *testing.T) {
	path := writeConfigFile(t, `
server:
  port: "9090"
scheduler:
  workers: 32
  queue_capacity: 500
  max_retry_delay: 1h
  shutdown_timeout: 12s
store:
  type: json
  path: /var/lib/godelayq/jobs.json
  flush_interval: 1s
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "9090", cfg.Server.Port)
	assert.Equal(t, 32, cfg.Scheduler.Workers)
	assert.Equal(t, 500, cfg.Scheduler.QueueCapacity)
	assert.Equal(t, time.Hour, cfg.Scheduler.MaxRetryDelay)
	assert.Equal(t, 12*time.Second, cfg.Scheduler.ShutdownTimeout)
	assert.Equal(t, "/var/lib/godelayq/jobs.json", cfg.Store.Path)
	assert.Equal(t, time.Second, cfg.Store.FlushInterval)
}

func TestLoadConfig_AuthAndCORS(t *testing.T) {
	path := writeConfigFile(t, `
server:
  port: "9090"
  auth:
    token: "s3cret"
  cors:
    allow_origins:
      - https://app.example.com
      - http://localhost:5173
    allow_credentials: true
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "s3cret", cfg.Server.Auth.Token)
	assert.Equal(t, []string{"https://app.example.com", "http://localhost:5173"}, cfg.Server.CORS.AllowOrigins)
	assert.True(t, cfg.Server.CORS.AllowCredentials)
}

func TestLoadConfig_StoreHistory(t *testing.T) {
	path := writeConfigFile(t, "store:\n  type: json\n  path: ./data/jobs.json\n  history_limit: 5\n  history_ttl: 24h\n")

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 5, cfg.Store.HistoryLimit)
	assert.Equal(t, 24*time.Hour, cfg.Store.HistoryTTL)

	// 关闭留痕的哨兵值必须能通过校验，并在归一化后保持原样
	off := writeConfigFile(t, "store:\n  type: json\n  path: ./data/jobs.json\n  history_limit: -1\n")
	offCfg, err := LoadConfig(off)
	require.NoError(t, err)
	assert.Equal(t, -1, offCfg.Normalized().Store.HistoryLimit, "0 才表示默认值，-1 不能被改写")
}

func TestLoadConfig_Logging(t *testing.T) {
	path := writeConfigFile(t, "logging:\n  level: warn\n  format: json\n")

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "warn", cfg.Logging.Level)
	assert.Equal(t, "json", cfg.Logging.Format)

	// 留空即回到默认级别与格式
	empty := writeConfigFile(t, "logging:\n  level: \"\"\n  format: \"\"\n")
	emptyCfg, err := LoadConfig(empty)
	require.NoError(t, err)
	assert.Equal(t, "info", emptyCfg.Normalized().Logging.Level)
	assert.Equal(t, "text", emptyCfg.Normalized().Logging.Format)
}

func TestLoadConfig_PartialFileKeepsDefaults(t *testing.T) {
	path := writeConfigFile(t, "scheduler:\n  workers: 5\n")

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, 5, cfg.Scheduler.Workers)
	assert.Equal(t, "8080", cfg.Server.Port, "untouched keys must keep defaults")
	assert.Equal(t, DefaultFlushInterval, cfg.Store.FlushInterval)
}

func TestLoadConfig_MissingFileFallsBackToDefaults(t *testing.T) {
	// 在空目录中按约定查找，找不到配置文件不算错误
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	defer os.Chdir(cwd)

	cfg, err := LoadConfig("")
	require.NoError(t, err)
	assert.Equal(t, DefaultConfig(), cfg)
}

func TestLoadConfig_ExplicitMissingFileErrors(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read config failed")
}

func TestLoadConfig_RejectsUnknownKeys(t *testing.T) {
	path := writeConfigFile(t, "server:\n  port: \"9090\"\nloader:\n  enabled: true\n")

	_, err := LoadConfig(path)
	require.Error(t, err, "unknown keys must not be silently ignored")
	assert.Contains(t, err.Error(), "parse config failed")
}

func TestLoadConfig_RejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"unsupported store type":           "store:\n  type: redis\n",
		"negative workers":                 "scheduler:\n  workers: -1\n",
		"negative queue capacity":          "scheduler:\n  queue_capacity: -5\n",
		"empty port":                       "server:\n  port: \"\"\n",
		"empty store path":                 "store:\n  path: \"\"\n",
		"empty groups path":                "store:\n  groups_path: \"\"\n",
		"non-positive shutdown":            "scheduler:\n  shutdown_timeout: 0s\n",
		"credentials with wildcard":        "server:\n  cors:\n    allow_origins: [\"*\"]\n    allow_credentials: true\n",
		"credentials with default origins": "server:\n  auth:\n    token: x\n  cors:\n    allow_credentials: true\n",
		"empty origin entry":               "server:\n  cors:\n    allow_origins: [\"https://a.example\", \"\"]\n",
		"history limit below sentinel":     "store:\n  history_limit: -2\n",
		"negative history ttl":             "store:\n  history_ttl: -1s\n",
		"unknown logging level":            "logging:\n  level: verbose\n",
		"unknown logging format":           "logging:\n  format: yaml\n",
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, content))
			assert.Error(t, err)
		})
	}
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	path := writeConfigFile(t, "server:\n  port: \"9090\"\n")

	t.Setenv("GODELAYQ_SERVER_PORT", "7777")
	t.Setenv("GODELAYQ_SCHEDULER_WORKERS", "13")
	t.Setenv("GODELAYQ_SERVER_AUTH_TOKEN", "env-token")
	t.Setenv("GODELAYQ_SERVER_CORS_ALLOW_ORIGINS", "https://a.example,https://b.example")
	t.Setenv("GODELAYQ_STORE_HISTORY_LIMIT", "7")
	t.Setenv("GODELAYQ_STORE_HISTORY_TTL", "6h")
	t.Setenv("GODELAYQ_STORE_GROUPS_PATH", "./data/groups-alt.json")
	t.Setenv("GODELAYQ_LOGGING_LEVEL", "error")
	t.Setenv("GODELAYQ_LOGGING_FORMAT", "json")
	// 观测层：最后一项是嵌套键，用来证明点号路径的绑定对子节同样生效——
	// BindEnv 列表里漏一项或写错一个点号都不会让别的东西失败，只有这里会没断言到。
	t.Setenv("GODELAYQ_OBSERVABILITY_ENABLED", "true")
	t.Setenv("GODELAYQ_OBSERVABILITY_PATH", "./data/observe-env.sqlite")
	t.Setenv("GODELAYQ_OBSERVABILITY_FLUSH_INTERVAL", "1s")
	t.Setenv("GODELAYQ_OBSERVABILITY_AUDIT_RETENTION_AGE", "168h")

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "7777", cfg.Server.Port)
	assert.Equal(t, 13, cfg.Scheduler.Workers)
	assert.Equal(t, "env-token", cfg.Server.Auth.Token)
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, cfg.Server.CORS.AllowOrigins,
		"list keys come from a comma-separated env value")
	assert.Equal(t, 7, cfg.Store.HistoryLimit)
	assert.Equal(t, 6*time.Hour, cfg.Store.HistoryTTL)
	assert.Equal(t, "./data/groups-alt.json", cfg.Store.GroupsPath)
	assert.Equal(t, "error", cfg.Logging.Level)
	assert.Equal(t, "json", cfg.Logging.Format)
	assert.True(t, cfg.Observability.Enabled)
	assert.Equal(t, "./data/observe-env.sqlite", cfg.Observability.Path)
	assert.Equal(t, time.Second, cfg.Observability.FlushInterval)
	assert.Equal(t, 168*time.Hour, cfg.Observability.Audit.RetentionAge,
		"observability.audit.retention_age 的嵌套键绑定")
	assert.Equal(t, DefaultObserveEventRetentionAge, cfg.Observability.Events.RetentionAge, "未被覆盖的键保持默认值")
}

func TestConfig_Normalized(t *testing.T) {
	zero := Config{}
	cfg := zero.Normalized()
	defaults := DefaultConfig()

	assert.Equal(t, defaults.Server.Port, cfg.Server.Port)
	assert.Equal(t, defaults.Server.CORS.AllowOrigins, cfg.Server.CORS.AllowOrigins,
		"an omitted origin list means wildcard, not \"block everything\"")
	assert.Equal(t, defaults.Scheduler.Workers, cfg.Scheduler.Workers)
	assert.Equal(t, defaults.Scheduler.ShutdownTimeout, cfg.Scheduler.ShutdownTimeout)
	assert.Equal(t, defaults.Store.Type, cfg.Store.Type)
	assert.Equal(t, defaults.Store.Path, cfg.Store.Path)
	assert.Equal(t, defaults.Store.FlushInterval, cfg.Store.FlushInterval)
	assert.Equal(t, defaults.Store.HistoryLimit, cfg.Store.HistoryLimit, "0 表示使用默认留痕条数")
	assert.Equal(t, defaults.Store.GroupsPath, cfg.Store.GroupsPath, "分组文件路径同样有约定默认值")
	assert.Equal(t, defaults.Logging.Level, cfg.Logging.Level, "空级别回落到 info")
	assert.Equal(t, defaults.Logging.Format, cfg.Logging.Format)

	// 显式设置的值不被覆盖
	kept := Config{}
	kept.Scheduler.QueueCapacity = 0
	kept.Scheduler.Workers = 4
	assert.Equal(t, 4, kept.Normalized().Scheduler.Workers)
}

func TestConfig_ThreadsIntoComponents(t *testing.T) {
	// 配置最终落到调度器与存储上的实际行为
	cfg := DefaultConfig()
	cfg.Scheduler.Workers = 3
	cfg.Scheduler.QueueCapacity = 9
	cfg.Store.FlushInterval = 15 * time.Millisecond
	cfg.Store.HistoryLimit = 2
	cfg.Store.HistoryTTL = time.Hour

	storePath := filepath.Join(t.TempDir(), "jobs.json")
	cfg.Store.Path = storePath

	store, err := NewJSONFileStoreWithOptions(cfg.Store.Path, StoreOptions{
		Interval:     cfg.Store.FlushInterval,
		HistoryLimit: cfg.Store.HistoryLimit,
		HistoryTTL:   cfg.Store.HistoryTTL,
	})
	require.NoError(t, err)
	defer store.Close()
	assert.Equal(t, 15*time.Millisecond, store.interval)
	assert.Equal(t, 2, store.historyLimit)
	assert.Equal(t, time.Hour, store.historyTTL)

	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(cfg.Scheduler.Workers)
	scheduler.SetQueueCapacity(cfg.Scheduler.QueueCapacity)

	scheduler.mu.RLock()
	defer scheduler.mu.RUnlock()
	assert.Equal(t, 3, scheduler.concurrency)
	assert.Equal(t, 9, scheduler.queueCapacity)
}

func TestLoadConfig_AuthUsers(t *testing.T) {
	path := writeConfigFile(t, `
server:
  auth:
    token: machine-token
    jwt:
      secret: "12345678901234567890123456789012"
      access_ttl: 5m
      refresh_ttl: 1h
    users:
      - name: admin01
        password_bcrypt: "$2a$10$abc"
        role: admin
      - name: ops01
        password_bcrypt: "$2a$10$def"
        role: ops
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.Equal(t, "machine-token", cfg.Server.Auth.Token)
	assert.Equal(t, 5*time.Minute, cfg.Server.Auth.JWT.AccessTTL)
	assert.Equal(t, time.Hour, cfg.Server.Auth.JWT.RefreshTTL)
	require.Len(t, cfg.Server.Auth.Users, 2)
	assert.Equal(t, "admin01", cfg.Server.Auth.Users[0].Name)
	assert.Equal(t, "$2a$10$abc", cfg.Server.Auth.Users[0].PasswordBcrypt)

	role, ok := cfg.Server.Auth.Users[1].ResolveRole()
	require.True(t, ok)
	assert.Equal(t, RoleOps, role)

	// 配了账号就必须能启用鉴权，登录页据此决定是否显示
	assert.True(t, cfg.Server.Auth.Enabled())
}

func TestLoadConfig_AuthJWTSecretFromEnv(t *testing.T) {
	path := writeConfigFile(t, `
server:
  auth:
    jwt:
      secret: "in-file-secret-too-short"
    users:
      - name: only
        password_bcrypt: "$2a$10$abc"
        role: viewer
`)
	t.Setenv("GODELAYQ_SERVER_AUTH_JWT_SECRET", strings.Repeat("k", 40))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("k", 40), cfg.Server.Auth.JWT.Secret,
		"密钥应可被环境变量覆盖，便于不落盘")
}

func TestLoadConfig_AuthUsersRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "users without secret",
			yaml: "server:\n  auth:\n    users:\n      - name: a\n        password_bcrypt: \"$2a$10$x\"\n        role: admin\n",
			want: "jwt.secret must not be empty",
		},
		{
			name: "short secret",
			yaml: "server:\n  auth:\n    jwt:\n      secret: \"short\"\n    users:\n      - name: a\n        password_bcrypt: \"$2a$10$x\"\n        role: admin\n",
			want: "at least 32 bytes",
		},
		{
			name: "unknown role",
			yaml: "server:\n  auth:\n    jwt:\n      secret: \"0123456789012345678901234567890ab\"\n    users:\n      - name: a\n        password_bcrypt: \"$2a$10$x\"\n        role: superuser\n",
			want: "role \"superuser\" for user \"a\" is invalid",
		},
		{
			name: "machine is not configurable",
			yaml: "server:\n  auth:\n    jwt:\n      secret: \"0123456789012345678901234567890ab\"\n    users:\n      - name: a\n        password_bcrypt: \"$2a$10$x\"\n        role: machine\n",
			want: "role \"machine\"",
		},
		{
			name: "duplicate name differs only by case",
			yaml: "server:\n  auth:\n    jwt:\n      secret: \"0123456789012345678901234567890ab\"\n    users:\n      - name: Admin\n        password_bcrypt: \"$2a$10$x\"\n        role: admin\n      - name: admin\n        password_bcrypt: \"$2a$10$y\"\n        role: viewer\n",
			want: "duplicate name",
		},
		{
			name: "empty password hash",
			yaml: "server:\n  auth:\n    jwt:\n      secret: \"0123456789012345678901234567890ab\"\n    users:\n      - name: a\n        role: admin\n",
			want: "password_bcrypt must not be empty",
		},
		{
			name: "negative ttl",
			yaml: "server:\n  auth:\n    jwt:\n      secret: \"0123456789012345678901234567890ab\"\n      access_ttl: -1m\n    users:\n      - name: a\n        password_bcrypt: \"$2a$10$x\"\n        role: admin\n",
			want: "access_ttl must not be negative",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, tc.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestAuthConfig_NoUsersSkipsJWTChecks(t *testing.T) {
	// 只有机器凭据（或完全不配鉴权）时不该被 JWT 规则拦住，
	// 否则现有部署升级后会直接起不来。
	cfg := DefaultConfig()
	cfg.Server.Auth.Token = "legacy-token"
	assert.NoError(t, cfg.Validate())

	cfg.Server.Auth.JWT.Secret = ""
	cfg.Server.Auth.JWT.AccessTTL = -time.Minute
	assert.NoError(t, cfg.Validate(), "未启用账号时 JWT 参数无意义")
	assert.False(t, cfg.Server.Auth.Enabled() && len(cfg.Server.Auth.Users) > 0)
}

func TestConfig_NormalizedFillsJWTDefaults(t *testing.T) {
	cfg := Config{}
	cfg.Server.Auth.JWT.Secret = strings.Repeat("s", 32)
	cfg.Server.Auth.Users = []UserConfig{{Name: "a", PasswordBcrypt: "$2a$10$x", Role: "viewer"}}

	normalized := cfg.Normalized()
	assert.Equal(t, DefaultAccessTTL, normalized.Server.Auth.JWT.AccessTTL)
	assert.Equal(t, DefaultRefreshTTL, normalized.Server.Auth.JWT.RefreshTTL)

	// 显式值保留
	kept := normalized
	kept.Server.Auth.JWT.AccessTTL = time.Minute
	assert.Equal(t, time.Minute, kept.Normalized().Server.Auth.JWT.AccessTTL)
}

// TestExampleConfigMatchesLocal 钉住 config.yaml 与 config.example.yaml 的键集合。
// 前者含本机凭据、已被 .gitignore 排除，两份只能靠人肉同步，而漂移在运行期没有任何征兆：
// 模板少一个键，新人照它配出来的进程就静默用着默认值；反过来模板多一个键，
// UnmarshalExact 会让对方一启动就报"未知键"。
func TestExampleConfigMatchesLocal(t *testing.T) {
	const example = "../configs/config.example.yaml"
	const local = "../configs/config.yaml"

	if _, err := os.Stat(local); err != nil {
		t.Skipf("本机没有 %s（克隆后需 cp configs/config.example.yaml configs/config.yaml），跳过比对", local)
	}

	exampleKeys, err := configKeys(example)
	require.NoError(t, err)
	localKeys, err := configKeys(local)
	require.NoError(t, err)

	assert.Equal(t, exampleKeys, localKeys, "两份配置的键不一致，按提示补齐另一份")
}

// configKeys 返回配置文件里全部叶子键的扁平路径，已排序好做集合比较。
func configKeys(path string) ([]string, error) {
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read %s failed: %w", path, err)
	}

	keys := v.AllKeys()
	sort.Strings(keys)
	return keys, nil
}

func TestExecutorsDefaults(t *testing.T) {
	cfg := DefaultConfig()

	assert.False(t, cfg.Executors.Enabled, "执行能力必须默认关闭：打开它等于把提交任务的权限扩展成执行命令的权限")
	assert.Equal(t, "admin", cfg.Executors.RequiredRole)
	assert.Equal(t, DefaultExecWorkspace, cfg.Executors.Workspace)
	assert.Equal(t, []string{"bash", "sh", "cmd", "pwsh", "node", "php", "python", "java"}, cfg.Executors.RuntimeAllow)
	assert.Equal(t, []string{"PATH", "LANG", "LC_ALL", "TZ", "HOME"}, cfg.Executors.EnvAllow)
	assert.Equal(t, DefaultExecConcurrency, cfg.Executors.Concurrency)
	assert.Zero(t, cfg.Executors.QueueCapacity, "0 表示与本节 concurrency 相等，由执行器侧解释")
	assert.Equal(t, DefaultExecDefaultTimeout, cfg.Executors.DefaultTimeout)
	assert.Equal(t, DefaultExecMaxTimeout, cfg.Executors.MaxTimeout)
	assert.Equal(t, "pause", cfg.Executors.RestorePolicy)
	assert.False(t, cfg.Executors.LoaderAllow)
	assert.False(t, cfg.Executors.WebEnabled,
		"档位的在线修改必须默认关闭：打开它等于把\"能执行什么\"的修改权限交给一个 ops 档 JWT")
	assert.Equal(t, DefaultExecProfilesPath, cfg.Executors.ProfilesPath)
	assert.Equal(t, DefaultExecInlinePreview, cfg.Executors.Output.InlinePreview)
	assert.Equal(t, DefaultExecMaxOutputBytes, cfg.Executors.Output.MaxBytes)
	assert.Equal(t, DefaultExecOutputDir, cfg.Executors.Output.Dir)
	assert.Equal(t, DefaultExecOutputTTL, cfg.Executors.Output.TTL)
	assert.Nil(t, cfg.Executors.Commands)

	assert.NoError(t, cfg.Validate())

	normalized := cfg.Normalized()
	assert.Equal(t, cfg.Executors, normalized.Executors, "默认值经归一化不应发生变化")
	assert.Zero(t, normalized.Executors.QueueCapacity, "queue_capacity 的 0 有含义（与并发数相等），不能被补成别的值")

	// 默认值里的列表每次都是新切片：否则某个使用者改写列表会波及后续拿到的默认配置
	runtimes := DefaultConfig().Executors.RuntimeAllow
	runtimes[0] = "mutated"
	assert.Equal(t, "bash", DefaultConfig().Executors.RuntimeAllow[0])

	// 只打开开关、其它一律用默认，必须是合法配置
	enabled := cfg
	enabled.Executors.Enabled = true
	assert.NoError(t, enabled.Validate())
}

func TestExecutorsValidate_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"role machine is not configurable", "executors:\n  required_role: machine\n", `required_role "machine"`},
		{"role viewer cannot execute", "executors:\n  required_role: viewer\n", "must be operator, admin or ops"},
		{"role typo", "executors:\n  required_role: admm\n", `required_role "admm" is invalid`},
		{"restore policy", "executors:\n  restore_policy: skip\n", `restore_policy "skip" is invalid`},
		{"workspace is whitespace", "executors:\n  workspace: \"   \"\n", "workspace must not be whitespace"},
		{"output dir is whitespace", "executors:\n  output:\n    dir: \" \"\n", "output.dir must not be whitespace"},
		{"empty runtime entry", "executors:\n  runtime_allow: [bash, \"\"]\n", "runtime_allow must not contain an empty entry"},
		{"padded runtime entry", "executors:\n  runtime_allow: [\" node \"]\n", "must not have surrounding spaces"},
		{"env allow would leak server credential", "executors:\n  env_allow: [PATH, GODELAYQ_SERVER_AUTH_TOKEN]\n", "GODELAYQ_ prefixed keys hold server credentials"},
		{"negative concurrency", "executors:\n  concurrency: -1\n", "concurrency must not be negative"},
		{"negative queue capacity", "executors:\n  queue_capacity: -2\n", "queue_capacity must not be negative"},
		{"negative default timeout", "executors:\n  default_timeout: -1m\n", "default_timeout must not be negative"},
		{"negative max timeout", "executors:\n  max_timeout: -1m\n", "max_timeout must not be negative"},
		{"negative inline preview", "executors:\n  output:\n    inline_preview: -1\n", "inline_preview must not be negative"},
		{"negative max bytes", "executors:\n  output:\n    max_bytes: -1\n", "max_bytes must not be negative"},
		{"negative ttl", "executors:\n  output:\n    ttl: -1h\n", "ttl must not be negative"},
		{"loader allow without enabled", "executors:\n  loader_allow: true\n", "requires executors.enabled to be true"},
		{"web enabled without executors", "executors:\n  web_enabled: true\n", "executors.web_enabled requires executors.enabled to be true"},
		{"profiles path is whitespace", "executors:\n  enabled: true\n  profiles_path: \"  \"\n", "profiles_path must not be whitespace"},
		{"concurrency zero while enabled", "executors:\n  enabled: true\n  concurrency: 0\n", "concurrency must be at least 1"},
		{"default timeout zero while enabled", "executors:\n  enabled: true\n  default_timeout: 0s\n", "default_timeout must be positive"},
		{"max timeout zero while enabled", "executors:\n  enabled: true\n  max_timeout: 0s\n", "max_timeout must be positive"},
		{"default timeout exceeds max", "executors:\n  enabled: true\n  default_timeout: 2h\n  max_timeout: 30m\n", "must not exceed executors.max_timeout"},
		{"output cap too small while enabled", "executors:\n  enabled: true\n  output:\n    max_bytes: 512\n", "max_bytes must be at least 1024"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, tc.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestExecutorsValidate_AllowsUnsetWhenDisabled(t *testing.T) {
	// 关闭执行器时，"没填"与"写 0"都不该报错：默认部署不该被要求抄一遍完整配置。
	// 对照 Rejects 用例里的 concurrency: 0 / default_timeout: 0s —— 打开开关后同样写法会被拒。
	cfg, err := LoadConfig(writeConfigFile(t,
		"executors:\n  enabled: false\n  concurrency: 0\n  default_timeout: 0s\n  max_timeout: 0s\n  output:\n    max_bytes: 0\n"))
	require.NoError(t, err)
	assert.False(t, cfg.Executors.Enabled)

	normalized := cfg.Normalized()
	assert.Equal(t, DefaultExecConcurrency, normalized.Executors.Concurrency)
	assert.Equal(t, DefaultExecDefaultTimeout, normalized.Executors.DefaultTimeout)
	assert.Equal(t, DefaultExecMaxTimeout, normalized.Executors.MaxTimeout)
	assert.Equal(t, DefaultExecMaxOutputBytes, normalized.Executors.Output.MaxBytes)
	assert.Equal(t, DefaultExecOutputTTL, normalized.Executors.Output.TTL,
		"ttl 的 0 表示不按时间清理，是有意的取值，不能被补成默认时长")
	assert.False(t, normalized.Executors.WebEnabled,
		"web_enabled 的 false 同样是有意取值：归一化不该把\"没打开\"补成打开")
	assert.Equal(t, DefaultExecProfilesPath, normalized.Executors.ProfilesPath)

	// 归一化之后的配置必须是"打开开关就能直接用"的，否则默认值本身有毛病
	normalized.Executors.Enabled = true
	assert.NoError(t, normalized.Validate())
}

func TestLoadConfig_ExecutorsFromYAML(t *testing.T) {
	path := writeConfigFile(t, `
executors:
  enabled: true
  required_role: ops
  workspace: /srv/exec
  runtime_allow:
    - bash
    - node
  env_allow:
    - PATH
    - TZ
  concurrency: 2
  queue_capacity: 8
  default_timeout: 1m
  max_timeout: 10m
  restore_policy: replay
  loader_allow: true
  output:
    inline_preview: 512
    max_bytes: 4096
    dir: /var/lib/godelayq/exec
    ttl: 48h
  commands:
    - name: nightly_report
      kind: script
      runtime: node
`)

	cfg, err := LoadConfig(path)
	require.NoError(t, err)

	assert.True(t, cfg.Executors.Enabled)
	assert.Equal(t, "ops", cfg.Executors.RequiredRole)
	assert.Equal(t, "/srv/exec", cfg.Executors.Workspace)
	assert.Equal(t, []string{"bash", "node"}, cfg.Executors.RuntimeAllow)
	assert.Equal(t, []string{"PATH", "TZ"}, cfg.Executors.EnvAllow)
	assert.Equal(t, 2, cfg.Executors.Concurrency)
	assert.Equal(t, 8, cfg.Executors.QueueCapacity)
	assert.Equal(t, time.Minute, cfg.Executors.DefaultTimeout)
	assert.Equal(t, 10*time.Minute, cfg.Executors.MaxTimeout)
	assert.Equal(t, "replay", cfg.Executors.RestorePolicy)
	assert.True(t, cfg.Executors.LoaderAllow)
	assert.Equal(t, 512, cfg.Executors.Output.InlinePreview)
	assert.Equal(t, 4096, cfg.Executors.Output.MaxBytes)
	assert.Equal(t, "/var/lib/godelayq/exec", cfg.Executors.Output.Dir)
	assert.Equal(t, 48*time.Hour, cfg.Executors.Output.TTL)

	// 档位现在是有类型的结构：只断言"能被读出来"，字段级校验在 executor.LoadProfiles
	require.Len(t, cfg.Executors.Commands, 1)
	assert.Equal(t, []ExecutorCommand{
		{Name: "nightly_report", Kind: "script", Runtime: "node"},
	}, cfg.Executors.Commands)
}

func TestLoadConfig_ExecutorsEnvOverrides(t *testing.T) {
	path := writeConfigFile(t, "executors:\n  enabled: true\n")

	t.Setenv("GODELAYQ_EXECUTORS_CONCURRENCY", "8")
	t.Setenv("GODELAYQ_EXECUTORS_RUNTIME_ALLOW", "node,php")
	t.Setenv("GODELAYQ_EXECUTORS_OUTPUT_TTL", "24h")
	t.Setenv("GODELAYQ_EXECUTORS_REQUIRED_ROLE", "operator")

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, 8, cfg.Executors.Concurrency)
	assert.Equal(t, []string{"node", "php"}, cfg.Executors.RuntimeAllow,
		"列表键可用逗号分隔的环境变量覆盖")
	assert.Equal(t, 24*time.Hour, cfg.Executors.Output.TTL)
	assert.Equal(t, "operator", cfg.Executors.RequiredRole)
	assert.Equal(t, DefaultExecMaxOutputBytes, cfg.Executors.Output.MaxBytes, "未被覆盖的键保持默认值")
}

// 档位在线管理的两个键都是标量，所以必须能被环境变量覆盖。
// 漏进 BindEnv 列表不会让任何东西失败——配置照常读、值照常生效，只有环境变量悄悄无效。
func TestLoadConfig_WebProfileEnvOverrides(t *testing.T) {
	path := writeConfigFile(t, "executors:\n  enabled: true\n  web_enabled: true\n  profiles_path: ./from-file.json\n")

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.True(t, cfg.Executors.WebEnabled)
	assert.Equal(t, "./from-file.json", cfg.Executors.ProfilesPath)

	t.Setenv("GODELAYQ_EXECUTORS_WEB_ENABLED", "false")
	t.Setenv("GODELAYQ_EXECUTORS_PROFILES_PATH", "./from-env.json")

	cfg, err = LoadConfig(path)
	require.NoError(t, err)
	assert.False(t, cfg.Executors.WebEnabled, "布尔键同样可覆盖：用来在单台机器上临时关掉在线管理")
	assert.Equal(t, "./from-env.json", cfg.Executors.ProfilesPath)
}

func TestLoadConfig_RejectsUnknownExecutorsKeys(t *testing.T) {
	// 这一条固定了"不提供绕过白名单的命令入口"：raw 模式相关的键在配置里根本不存在，
	// 写进来会启动失败，而不是被静默忽略后让人以为已经打开了某种自由命令行能力。
	_, err := LoadConfig(writeConfigFile(t, "executors:\n  allow_raw_command: true\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config failed")

	_, err = LoadConfig(writeConfigFile(t, "executors:\n  output:\n    keep_everything: true\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config failed")

	// TASK-E02 之后档位内部也是有类型的结构：拼错的键名同样被 UnmarshalExact 拒绝，
	// 不再存在"档位写错但配置加载通过"的窗口。
	_, err = LoadConfig(writeConfigFile(t, "executors:\n  commands:\n    - nmae: nightly\n"))
	require.Error(t, err, "档位内部的未知键也必须报错")
	assert.Contains(t, err.Error(), "nmae")
}

func TestObservabilityDefaults(t *testing.T) {
	cfg := DefaultConfig()

	assert.False(t, cfg.Observability.Enabled, "观测层必须默认关闭：打开它才会创建库文件并订阅事件总线")
	assert.Equal(t, DefaultObservePath, cfg.Observability.Path)
	assert.Equal(t, DefaultFlushInterval, cfg.Observability.FlushInterval, "与任务快照的合并周期同一口径")
	assert.Equal(t, DefaultObserveQueueCapacity, cfg.Observability.QueueCapacity)
	assert.Equal(t, DefaultObserveBusyTimeout, cfg.Observability.BusyTimeout)
	assert.Equal(t, "normal", cfg.Observability.Synchronous)
	assert.True(t, cfg.Observability.Events.Enabled)
	assert.Equal(t, DefaultObserveEventRetentionCount, cfg.Observability.Events.RetentionCount)
	assert.Equal(t, 720*time.Hour, cfg.Observability.Events.RetentionAge)
	assert.True(t, cfg.Observability.Artifacts.Enabled)
	assert.True(t, cfg.Observability.Audit.Enabled)
	assert.Equal(t, DefaultObserveAuditRetentionCount, cfg.Observability.Audit.RetentionCount)
	assert.Equal(t, 2160*time.Hour, cfg.Observability.Audit.RetentionAge)

	assert.NoError(t, cfg.Validate())

	normalized := cfg.Normalized()
	assert.Equal(t, cfg.Observability, normalized.Observability, "默认值经归一化不应发生变化")

	// 零值配置补齐后，除"0 有含义"的保留时长外都应回到默认值。
	// retention_age 的 0 表示不按时间淘汰，与 store.history_ttl、executors.output.ttl 同一读法，
	// 因此代码里构造的零值配置不会被补成 720h/2160h——那是显式选择，不是缺省。
	zero := Config{}.Normalized().Observability
	assert.Equal(t, DefaultObservePath, zero.Path)
	assert.Equal(t, DefaultObserveFlushInterval, zero.FlushInterval)
	assert.Equal(t, DefaultObserveQueueCapacity, zero.QueueCapacity, "队列容量没有 0 这种取值，零值回到默认")
	assert.Equal(t, DefaultObserveBusyTimeout, zero.BusyTimeout)
	assert.Equal(t, DefaultObserveSynchronous, zero.Synchronous)
	assert.Equal(t, DefaultObserveEventRetentionCount, zero.Events.RetentionCount)
	assert.Zero(t, zero.Events.RetentionAge)
	assert.Equal(t, DefaultObserveAuditRetentionCount, zero.Audit.RetentionCount)
	assert.Zero(t, zero.Audit.RetentionAge)

	// 默认值必须是"打开总开关就能直接用"的，否则默认配置本身有毛病
	enabled := cfg
	enabled.Observability.Enabled = true
	assert.NoError(t, enabled.Validate())
}

func TestObservabilityValidateRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"empty path", "observability:\n  enabled: true\n  path: \"   \"\n", "observability.path must not be empty"},
		{"bad synchronous", "observability:\n  enabled: true\n  synchronous: off\n", `observability.synchronous "off" is invalid, use normal or full`},
		{"negative flush interval", "observability:\n  enabled: true\n  flush_interval: -1s\n", "observability.flush_interval must not be negative"},
		{"flush interval below batch floor", "observability:\n  enabled: true\n  flush_interval: 1ms\n", "observability.flush_interval"},
		{"negative busy timeout", "observability:\n  enabled: true\n  busy_timeout: -1s\n", "observability.busy_timeout must not be negative"},
		{"zero queue capacity", "observability:\n  enabled: true\n  queue_capacity: 0\n", "observability.queue_capacity must be positive"},
		{"negative queue capacity", "observability:\n  enabled: true\n  queue_capacity: -1\n", "observability.queue_capacity must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, tc.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want, "错误信息里要带上被拒的键名，便于运维定位")
		})
	}

	// 低于批量下限的取值，报错里要给出建议值，否则运维只能再去翻代码
	_, err := LoadConfig(writeConfigFile(t, "observability:\n  enabled: true\n  flush_interval: 5ms\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "one transaction per record")
	assert.Contains(t, err.Error(), DefaultObserveFlushInterval.String())
}

func TestObservabilityIgnoredWhenDisabled(t *testing.T) {
	// 关闭总开关时上面每一条非法取值都不该报错：默认部署不该被要求抄一遍完整配置，
	// 也不该因为一节不生效的取值而起不来。这是"默认关闭 = 行为零变化"的守卫。
	cfg, err := LoadConfig(writeConfigFile(t, `
observability:
  enabled: false
  path: "  "
  synchronous: off
  flush_interval: 1ms
  busy_timeout: -1s
  queue_capacity: 0
  events:
    enabled: true
    retention_count: -5
    retention_age: -1h
  artifacts:
    enabled: true
  audit:
    enabled: true
    retention_count: -1
    retention_age: -1h
`))
	require.NoError(t, err)
	assert.False(t, cfg.Observability.Enabled)

	// 子节的开关在总开关关闭时同样只是"没有读取方"，不报错
	cfg.Observability.Events.Enabled = false
	cfg.Observability.Artifacts.Enabled = false
	cfg.Observability.Audit.Enabled = false
	assert.NoError(t, cfg.Validate())
}

func TestObservabilityPartialOverride(t *testing.T) {
	cfg, err := LoadConfig(writeConfigFile(t, `
observability:
  enabled: true
  path: /var/lib/godelayq/observe.sqlite
  audit:
    retention_age: 30h
`))
	require.NoError(t, err)

	assert.True(t, cfg.Observability.Enabled)
	assert.Equal(t, "/var/lib/godelayq/observe.sqlite", cfg.Observability.Path)
	assert.Equal(t, 30*time.Hour, cfg.Observability.Audit.RetentionAge)

	assert.Equal(t, DefaultObserveFlushInterval, cfg.Observability.FlushInterval, "未配的键保持默认值")
	assert.Equal(t, DefaultObserveQueueCapacity, cfg.Observability.QueueCapacity)
	assert.Equal(t, DefaultObserveBusyTimeout, cfg.Observability.BusyTimeout)
	assert.Equal(t, DefaultObserveSynchronous, cfg.Observability.Synchronous)
	assert.Equal(t, DefaultObserveEventRetentionCount, cfg.Observability.Events.RetentionCount)
	assert.Equal(t, DefaultObserveEventRetentionAge, cfg.Observability.Events.RetentionAge)
	assert.True(t, cfg.Observability.Events.Enabled)
	assert.True(t, cfg.Observability.Artifacts.Enabled)
	assert.True(t, cfg.Observability.Audit.Enabled)

	// 显式写了值的部分，归一化不能再改动
	normalized := cfg.Normalized().Observability
	assert.Equal(t, 30*time.Hour, normalized.Audit.RetentionAge)
	assert.Equal(t, "/var/lib/godelayq/observe.sqlite", normalized.Path)

	// retention_age 写 0 是"不按时间淘汰"的有意取值，归一化必须放过
	off, err := LoadConfig(writeConfigFile(t, "observability:\n  enabled: true\n  events:\n    retention_age: 0s\n"))
	require.NoError(t, err)
	assert.Zero(t, off.Normalized().Observability.Events.RetentionAge)
}

// TestLoadConfig_RejectsUnknownObservabilityKeys 守的是拼错的键：写进来必须启动失败，
// 而不是静默用着默认值。判据来自 LoadConfig 里的 v.UnmarshalExact（按 Config 结构体有无对应字段），
// 与 core/config.go 里那份 BindEnv 列表无关——BindEnv 漏项不会报错，只让环境变量无效，
// 那条由 TestLoadConfig_EnvOverrides 里的四个观测层断言守住。
func TestLoadConfig_RejectsUnknownObservabilityKeys(t *testing.T) {
	_, err := LoadConfig(writeConfigFile(t, "observability:\n  bogous: 1\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config failed")

	_, err = LoadConfig(writeConfigFile(t, "observability:\n  events:\n    keep_forever: true\n"))
	require.Error(t, err, "子节里的未知键同样要被拒绝")
	assert.Contains(t, err.Error(), "parse config failed")
}
