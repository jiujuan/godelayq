package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
		"unsupported store type":  "store:\n  type: redis\n",
		"negative workers":        "scheduler:\n  workers: -1\n",
		"negative queue capacity": "scheduler:\n  queue_capacity: -5\n",
		"empty port":              "server:\n  port: \"\"\n",
		"empty store path":        "store:\n  path: \"\"\n",
		"non-positive shutdown":   "scheduler:\n  shutdown_timeout: 0s\n",
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

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "7777", cfg.Server.Port)
	assert.Equal(t, 13, cfg.Scheduler.Workers)
}

func TestConfig_Normalized(t *testing.T) {
	zero := Config{}
	cfg := zero.Normalized()
	defaults := DefaultConfig()

	assert.Equal(t, defaults.Server.Port, cfg.Server.Port)
	assert.Equal(t, defaults.Scheduler.Workers, cfg.Scheduler.Workers)
	assert.Equal(t, defaults.Scheduler.ShutdownTimeout, cfg.Scheduler.ShutdownTimeout)
	assert.Equal(t, defaults.Store.Type, cfg.Store.Type)
	assert.Equal(t, defaults.Store.Path, cfg.Store.Path)
	assert.Equal(t, defaults.Store.FlushInterval, cfg.Store.FlushInterval)

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

	storePath := filepath.Join(t.TempDir(), "jobs.json")
	cfg.Store.Path = storePath

	store, err := NewJSONFileStoreWithInterval(cfg.Store.Path, cfg.Store.FlushInterval)
	require.NoError(t, err)
	defer store.Close()
	assert.Equal(t, 15*time.Millisecond, store.interval)

	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(cfg.Scheduler.Workers)
	scheduler.SetQueueCapacity(cfg.Scheduler.QueueCapacity)

	scheduler.mu.RLock()
	defer scheduler.mu.RUnlock()
	assert.Equal(t, 3, scheduler.concurrency)
	assert.Equal(t, 9, scheduler.queueCapacity)
}
