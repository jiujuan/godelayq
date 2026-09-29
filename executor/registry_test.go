package executor

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

func names(profiles []*Profile) []string {
	keys := make([]string, 0, len(profiles))
	for _, p := range profiles {
		keys = append(keys, p.HandlerKey())
	}
	return keys
}

// namedScript 造一条名字可改、解释器固定为"一定存在"的脚本档位。
func namedScript(t *testing.T, workspace, name string) core.ExecutorCommand {
	cmd := scriptUsingRuntime(selfExecutable(t), declareFile(t, workspace, "scripts/"+name+".mjs"))
	cmd.Name = name
	return cmd
}

func TestNewRegistry_EmptyWhenDisabled(t *testing.T) {
	workspace := t.TempDir()

	// 关闭状态下配置里的错误不暴露：这是 E02/E03 一致采用的"关闭即完全惰性"口径
	cfg := configAllowing(workspace, []string{missingProgram},
		scriptUsingRuntime(missingProgram, "../../evil.sh"))
	cfg.Executors.Enabled = false

	registry, err := NewRegistry(cfg, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	require.NoError(t, err)
	require.NotNil(t, registry, "关闭时要返回空登记表，不是 nil")

	assert.False(t, registry.Enabled())
	assert.Empty(t, registry.Keys())
	assert.Empty(t, registry.Profiles())

	_, ok := registry.Lookup("exec.nightly_report")
	assert.False(t, ok)
	reason, ok := registry.Available("exec.nightly_report")
	assert.False(t, ok)
	assert.Empty(t, reason)
}

func TestNewRegistry_EnabledWithoutCommands(t *testing.T) {
	registry, err := NewRegistry(configWith(t.TempDir()), nil)
	require.NoError(t, err)
	assert.True(t, registry.Enabled())
	assert.Empty(t, registry.Keys())
}

func TestNewRegistry_LookupAndSort(t *testing.T) {
	workspace := t.TempDir()

	// 故意按非字典序声明，验证输出顺序与声明顺序无关
	registry, err := NewRegistry(configAllowing(workspace, []string{selfExecutable(t)},
		namedScript(t, workspace, "zeta"),
		namedScript(t, workspace, "alpha"),
		namedScript(t, workspace, "mid"),
	), nil)
	require.NoError(t, err)

	want := []string{"exec.alpha", "exec.mid", "exec.zeta"}
	assert.Equal(t, want, registry.Keys())
	assert.Equal(t, want, names(registry.Profiles()))

	// 重复调用顺序稳定：接口输出与测试断言都依赖这一点
	assert.Equal(t, want, registry.Keys())
	assert.Equal(t, want, names(registry.Profiles()))

	// 返回的是副本，调用方排序或清空不会污染登记表
	keys := registry.Keys()
	keys[0] = "tampered"
	assert.Equal(t, want, registry.Keys())

	profiles := registry.Profiles()
	profiles = append(profiles[:0], profiles[2])
	assert.Len(t, registry.Profiles(), 3)

	for _, key := range want {
		profile, ok := registry.Lookup(key)
		require.True(t, ok)
		assert.Equal(t, key, profile.HandlerKey())

		result, ok := registry.ProbeOf(key)
		require.True(t, ok)
		assert.True(t, result.Available, "reason = %q", result.Reason)

		reason, ok := registry.Available(key)
		assert.True(t, ok)
		assert.Empty(t, reason)
	}

	_, ok := registry.Lookup("exec.absent")
	assert.False(t, ok)
	_, ok = registry.ProbeOf("exec.absent")
	assert.False(t, ok)
	_, ok = registry.Available("exec.absent")
	assert.False(t, ok)
}

func TestNewRegistry_LogsUnavailable(t *testing.T) {
	workspace := t.TempDir()
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))

	executable := selfExecutable(t)
	script := declareFile(t, workspace, "scripts/report.mjs")

	good := scriptUsingRuntime(executable, script)
	good.Name = "good_one"
	bad := scriptUsingRuntime(missingProgram, script)
	bad.Name = "bad_one"

	registry, err := NewRegistry(configAllowing(workspace, []string{executable, missingProgram},
		good, bad), logger)
	require.NoError(t, err)

	output := log.String()
	assert.Contains(t, output, "level=WARN")
	assert.Contains(t, output, "executor profile unavailable")
	assert.Contains(t, output, missingProgram)

	// 一个 warn 行对应一个不可用档位：可用的那条不该出现在日志里
	assert.Equal(t, 1, strings.Count(output, "executor profile unavailable"), "output=%s", output)
	assert.Contains(t, output, "profile=bad_one")
	assert.NotContains(t, output, "good_one")
	assert.Len(t, registry.Keys(), 2)
}

func TestNewRegistry_InvalidProfileIsAnError(t *testing.T) {
	workspace := t.TempDir()

	cfg := configAllowing(workspace, []string{selfExecutable(t)},
		scriptUsingRuntime(selfExecutable(t), "../../evil.sh"))

	registry, err := NewRegistry(cfg, nil)
	require.Error(t, err)
	assert.Nil(t, registry, "校验失败时不返回半成品的登记表")
	assert.Contains(t, err.Error(), "executors.commands[0]")
}

func TestRegistry_AccessorsReflectConfig(t *testing.T) {
	workspace := t.TempDir()

	cfg := configAllowing(workspace, []string{selfExecutable(t)},
		namedScript(t, workspace, "one"))
	cfg.Executors.RequiredRole = "ops"
	cfg.Executors.LoaderAllow = true

	registry, err := NewRegistry(cfg, nil)
	require.NoError(t, err)
	assert.True(t, registry.Enabled())
	assert.Equal(t, "ops", registry.RequiredRole())
	assert.True(t, registry.LoaderAllowed())

	// 未显式配置时取默认值，而不是零值字符串
	defaulted, err := NewRegistry(configAllowing(workspace, []string{selfExecutable(t)},
		namedScript(t, workspace, "two")), nil)
	require.NoError(t, err)
	assert.Equal(t, core.DefaultExecRequiredRole, defaulted.RequiredRole())
	assert.False(t, defaulted.LoaderAllowed())
}

// TestNewRegistry_FromConfigFile 是本卡的手工确认的可执行版本：
// 一份真实 YAML → core.LoadConfig → NewRegistry，混合"机器上有"和"机器上没有"两种档位，
// 断言前者可用、后者留在表里并带原因、日志里有对应的 warn。
//
// 卡片 §7 写的"启动服务看日志"要到 TASK-E04 把 NewRegistry 接进装配之后才可能，
// 本卡以这条测试替代，并在这里说明而不是假装验证过。
func TestNewRegistry_FromConfigFile(t *testing.T) {
	workspace := t.TempDir()
	executable := selfExecutable(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "scripts"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "scripts", "present.mjs"),
		[]byte("console.log('ok')\n"), 0o600))

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
executors:
  enabled: true
  workspace: ` + filepath.ToSlash(workspace) + `
  runtime_allow:
    - bash
    - ` + filepath.ToSlash(executable) + `
    - ` + missingProgram + `
  commands:
    - name: report_present
      kind: script
      runtime: ` + filepath.ToSlash(executable) + `
      script: scripts/present.mjs
    - name: report_absent
      kind: script
      runtime: ` + missingProgram + `
      script: scripts/present.mjs
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := core.LoadConfig(path)
	require.NoError(t, err)

	var log bytes.Buffer
	registry, err := NewRegistry(cfg, slog.New(slog.NewTextHandler(&log, nil)))
	require.NoError(t, err)

	assert.Equal(t, []string{"exec.report_absent", "exec.report_present"}, registry.Keys())

	reason, ok := registry.Available("exec.report_present")
	assert.True(t, ok)
	assert.Empty(t, reason, "解释器与脚本都存在的档位应判为可用")

	reason, ok = registry.Available("exec.report_absent")
	assert.False(t, ok)
	assert.Contains(t, reason, missingProgram)

	output := log.String()
	assert.Contains(t, output, "msg=\"executor profile unavailable\"")
	assert.Contains(t, output, "profile=report_absent")
	assert.Contains(t, output, "handler_key=exec.report_absent")
	assert.NotContains(t, output, "report_present", "可用档位不该被记 warn")
}
