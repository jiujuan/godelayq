package executor

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 这一组是 TASK-N06 §3.4 的启动告警用例。
//
// 断言打在 slog 的 StructAttr 上（bufferLogger 那份文本输出），而不是 grep 控制台：
// 一行日志的键值对才是运维要读的东西，也是"这条告警说了哪四件事"的可测形态。

// registerWithLogger 用真实的登记表跑一次 Register，返回那次写出的日志。
func registerWithLogger(t *testing.T, cfg core.Config) string {
	t.Helper()

	registry, err := NewRegistry(cfg, nil)
	require.NoError(t, err)

	logs := &bytes.Buffer{}
	result, err := Register(newFakeRegistrar(), registry, cfg, nil, bufferLogger(logs))
	require.NoError(t, err)
	require.Greater(t, result.Registered, 0, "空表会走另一条早退分支，本用例要的是完整注册")
	return logs.String()
}

func adhocConfig(workspace string, tune func(*core.Config)) core.Config {
	cfg := adhocEnabled(configWith(workspace))
	if tune != nil {
		tune(&cfg)
	}
	return cfg
}

func TestRegister_AdhocEnabledWarnsOnceWithTheFourFacts(t *testing.T) {
	workspace := t.TempDir()
	cfg := adhocConfig(workspace, func(cfg *core.Config) {
		cfg.Executors.Adhoc.PathPrefixes = []string{"scripts", "jobs"}
		cfg.Executors.Adhoc.URLHosts = []string{"hook.example.com"}
		cfg.Executors.RequiredRole = "ops"
	})

	logs := registerWithLogger(t, cfg)

	require.Contains(t, logs, "free-form execution profiles are enabled")
	assert.Contains(t, logs, "profiles=exec.http,exec.php,exec.python,exec.shell",
		"列出的是登记表里真正生效的那几条，按注册键字典序")
	assert.Contains(t, logs, "required_role=ops",
		"告警要说清是哪个身份拿到这份能力")
	assert.Contains(t, logs, `path_prefixes=scripts,jobs`)
	assert.Contains(t, logs, "url_hosts=hook.example.com")
	assert.Contains(t, logs, "url_allow_private=false")

	// 两份守卫都关着，不该出现那两条单独的告警。
	assert.NotContains(t, logs, "path_prefixes is empty")
	assert.NotContains(t, logs, "url_allow_private is true")
}

func TestRegister_AdhocWarnsOnEachTurnedOffGuard(t *testing.T) {
	t.Run("限定目录留空", func(t *testing.T) {
		logs := registerWithLogger(t, adhocConfig(t.TempDir(), nil))
		assert.Contains(t, logs, "executors.adhoc.path_prefixes is empty",
			"不限目录必须单独说一句：它不是配置漏填就是放开本机任意文件")
	})

	t.Run("放开回环与私网", func(t *testing.T) {
		logs := registerWithLogger(t, adhocConfig(t.TempDir(), func(cfg *core.Config) {
			cfg.Executors.Adhoc.URLAllowPrivate = true
			prefixes := filepath.Join(cfg.Executors.Workspace, "scripts")
			cfg.Executors.Adhoc.PathPrefixes = []string{prefixes}
		}))
		assert.Contains(t, logs, "executors.adhoc.url_allow_private is true")
		assert.NotContains(t, logs, "path_prefixes is empty")

		// 同一次放开也会撞到既有的那条 http 档位告警（deny_private_ranges=false 的那一条），
		// 两条说的是不同侧面：那条按档位列出，这条按配置说清是哪个开关。
		assert.Contains(t, logs, "executor http profiles accept private and loopback addresses")
	})

	t.Run("整节关闭时半句都没有", func(t *testing.T) {
		cfg := configWith(t.TempDir(), scriptUsingRuntime("bash", "scripts/deploy.sh"))
		logs := registerWithLogger(t, cfg)
		assert.NotContains(t, logs, "free-form execution profiles")
		assert.NotContains(t, logs, "path_prefixes is empty")
		assert.NotContains(t, logs, "url_allow_private is true")
	})
}

// TestRegister_AdhocEnabledButAllYielded 覆盖那条"打开却一条都没登记"的分支：
// 四条全被 executors.commands 顶掉时，说清状态比留一份安静更负责。
func TestRegister_AdhocEnabledButAllYielded(t *testing.T) {
	workspace := t.TempDir()

	// 四条内置档位的键全部被 executors.commands 占掉：内置那四条一条条让位，
	// 于是"整节打开"与"有一条能用"是两件事，这一行状态要说出来。
	yield := func(name string) core.ExecutorCommand {
		return core.ExecutorCommand{
			Name: name, Kind: string(KindBinary), Program: "ping",
			FixedArgs: []string{"-n", "1", "127.0.0.1"}, Timeout: time.Second,
		}
	}

	cfg := adhocConfig(workspace, func(cfg *core.Config) {
		cfg.Executors.Commands = []core.ExecutorCommand{
			yield(AdhocShellName), yield(AdhocPHPName), yield(AdhocPythonName),
			{Name: AdhocHTTPName, Kind: string(KindHTTP), Method: "GET", Body: "none",
				URLTemplate: "https://api.example.com/x", AllowedHosts: []string{"api.example.com"},
				Timeout: time.Second},
		}
	})

	registry, err := NewRegistry(cfg, nil)
	require.NoError(t, err)

	logs := &bytes.Buffer{}
	_, err = Register(newFakeRegistrar(), registry, cfg, nil, bufferLogger(logs))
	require.NoError(t, err)

	for _, profile := range registry.Profiles() {
		require.False(t, profile.Adhoc, "四条都让位了，登记表里不该还有内置档位")
	}
	assert.Contains(t, logs.String(), "executors.adhoc is enabled but no built-in profile is registered")
}
