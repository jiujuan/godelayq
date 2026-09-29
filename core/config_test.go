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
