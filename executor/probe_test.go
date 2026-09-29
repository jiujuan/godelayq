package executor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// missingProgram 是一个几乎不可能出现在 PATH 里的名字。
const missingProgram = "godelayq-test-runtime-xyz"

// selfExecutable 返回当前测试程序的可执行文件路径。
// 它一定存在且可执行，所以可以当作"PATH 里找得到的程序"用，
// 让探测的正反两个分支都不依赖目标机器装了 node 或 php（卡片 §5、§9 的要求）。
func selfExecutable(t *testing.T) string {
	t.Helper()

	path, err := os.Executable()
	require.NoError(t, err)
	return path
}

// configAllowing 在默认配置上把 extraRuntimes 加进白名单，
// 这样"程序名合法但机器上没有"的档位能通过 E02 校验、留给探测判断。
func configAllowing(workspace string, extraRuntimes []string, commands ...core.ExecutorCommand) core.Config {
	cfg := configWith(workspace, commands...)
	cfg.Executors.RuntimeAllow = append(cfg.Executors.RuntimeAllow, extraRuntimes...)
	return cfg
}

func scriptUsingRuntime(runtimeName, scriptRel string) core.ExecutorCommand {
	cmd := validScriptProfile(scriptRel)
	cmd.Runtime = runtimeName
	return cmd
}

func profileFrom(t *testing.T, cfg core.Config, index int) *Profile {
	t.Helper()

	profiles := mustLoad(t, cfg)
	require.Greater(t, len(profiles), index)
	return profiles[index]
}

func TestProbe_ScriptMissingRuntime(t *testing.T) {
	workspace := t.TempDir()
	script := declareFile(t, workspace, "scripts/report.mjs")

	profile := profileFrom(t, configAllowing(workspace, []string{missingProgram},
		scriptUsingRuntime(missingProgram, script)), 0)

	result := Probe(profile)
	assert.False(t, result.Available)
	assert.Empty(t, result.Path)
	assert.Contains(t, result.Reason, missingProgram)
	assert.Contains(t, result.Reason, "not found in PATH")
}

func TestProbe_ScriptMissingFile(t *testing.T) {
	workspace := t.TempDir()

	// 解释器存在、脚本不存在：原因必须是文件缺失，不能和"PATH 里找不到解释器"混淆
	profile := profileFrom(t, configAllowing(workspace, []string{selfExecutable(t)},
		scriptUsingRuntime(selfExecutable(t), "scripts/absent.mjs")), 0)

	result := Probe(profile)
	assert.False(t, result.Available)
	assert.Contains(t, result.Reason, "script file")
	assert.Contains(t, result.Reason, "does not exist")
	assert.NotContains(t, result.Reason, "not found in PATH")
}

func TestProbe_ScriptIsAcceptedWithoutExecuteBit(t *testing.T) {
	workspace := t.TempDir()
	absolute := filepath.Join(workspace, "scripts", "readonly.mjs")
	require.NoError(t, os.MkdirAll(filepath.Dir(absolute), 0o750))
	// 0600：没有执行位。脚本由解释器读取，不该要求它能被直接执行。
	require.NoError(t, os.WriteFile(absolute, []byte("console.log(1)\n"), 0o600))

	profile := profileFrom(t, configAllowing(workspace, []string{selfExecutable(t)},
		scriptUsingRuntime(selfExecutable(t), "scripts/readonly.mjs")), 0)

	result := Probe(profile)
	assert.True(t, result.Available, "reason = %q", result.Reason)
	assert.Equal(t, selfExecutable(t), result.Path, "脚本档位报告的是解释器路径")
}

func TestProbe_ScriptIsDirectory(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(workspace, "scripts"), 0o750))

	profile := profileFrom(t, configAllowing(workspace, []string{selfExecutable(t)},
		scriptUsingRuntime(selfExecutable(t), "scripts")), 0)

	result := Probe(profile)
	assert.False(t, result.Available)
	assert.Contains(t, result.Reason, "not a regular file")
}

func TestProbe_ReasonNeverLeaksWorkspacePath(t *testing.T) {
	workspace := t.TempDir()

	profile := profileFrom(t, configAllowing(workspace, []string{selfExecutable(t)},
		scriptUsingRuntime(selfExecutable(t), "scripts/absent.mjs")), 0)

	result := Probe(profile)
	require.False(t, result.Available)
	assert.NotContains(t, result.Reason, workspace,
		"原因会被原样显示给用户，不能带服务器目录结构")
	assert.Contains(t, result.Reason, "scripts/absent.mjs")
}

func TestProbe_BinaryInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	program := declareFile(t, workspace, "bin/tool.exe")

	cmd := validBinaryProfile(program)
	profile := profileFrom(t, configWith(workspace, cmd), 0)

	result := Probe(profile)
	assert.True(t, result.Available, "reason = %q", result.Reason)
	assert.True(t, filepath.IsAbs(result.Path))
	assert.Empty(t, result.Reason)
	assert.Equal(t, profile.ProgramPath, result.Path)
}

func TestProbe_BinaryNotExecutable(t *testing.T) {
	workspace := t.TempDir()
	absolute := filepath.Join(workspace, "bin", "tool")
	require.NoError(t, os.MkdirAll(filepath.Dir(absolute), 0o750))
	require.NoError(t, os.WriteFile(absolute, []byte("data"), 0o600))

	profile := profileFrom(t, configWith(workspace, validBinaryProfile("bin/tool")), 0)

	result := Probe(profile)
	assert.False(t, result.Available)
	assert.Contains(t, result.Reason, "executable")
	if runtime.GOOS == "windows" {
		assert.Contains(t, result.Reason, "extension")
	} else {
		assert.Contains(t, result.Reason, "no execute bit")
	}
}

func TestProbe_BinaryFileMissing(t *testing.T) {
	workspace := t.TempDir()

	profile := profileFrom(t, configWith(workspace, validBinaryProfile("bin/absent.exe")), 0)

	result := Probe(profile)
	assert.False(t, result.Available)
	assert.Contains(t, result.Reason, "program file")
	assert.Contains(t, result.Reason, "does not exist")
}

func TestProbe_BinaryProgramNameMissing(t *testing.T) {
	workspace := t.TempDir()

	cmd := validBinaryProfile(missingProgram)
	cmd.FixedArgs = nil
	profile := profileFrom(t, configAllowing(workspace, []string{missingProgram}, cmd), 0)
	require.Equal(t, missingProgram, profile.ProgramName, "program 命中白名单时应按 PATH 程序处理")

	result := Probe(profile)
	assert.False(t, result.Available)
	assert.Contains(t, result.Reason, missingProgram)
	assert.Contains(t, result.Reason, "not found in PATH")
}

func TestProbe_BinaryProgramNameFound(t *testing.T) {
	workspace := t.TempDir()
	executable := selfExecutable(t)

	cmd := validBinaryProfile(executable)
	cmd.FixedArgs = nil
	profile := profileFrom(t, configAllowing(workspace, []string{executable}, cmd), 0)

	result := Probe(profile)
	assert.True(t, result.Available, "reason = %q", result.Reason)
	assert.Equal(t, executable, result.Path)
}

func TestProbe_HTTPAlwaysAvailable(t *testing.T) {
	workspace := t.TempDir()

	profile := profileFrom(t, configWith(workspace, validHTTPProfile()), 0)

	result := Probe(profile)
	assert.True(t, result.Available)
	assert.Empty(t, result.Path)
	assert.Empty(t, result.Reason)
	assert.Empty(t, result.Version, "探测阶段不执行外部程序，版本一律留空")
}

func TestProbe_UnknownKindIsReportedNotPanicking(t *testing.T) {
	workspace := t.TempDir()
	profile := profileFrom(t, configWith(workspace, validScriptProfile(declareFile(t, workspace, "a.sh"))), 0)

	// E02 不允许未知 kind 通过校验，这里只确认万一被绕过也不会 panic 或误报可用
	profile.Kind = Kind("container")
	result := Probe(profile)
	assert.False(t, result.Available)
	assert.Contains(t, result.Reason, "unknown kind")
}

func TestProbe_ProfilesWithoutTargetFileField(t *testing.T) {
	workspace := t.TempDir()

	// 手工构造一个 script 档位但没有 ScriptPath（只可能来自内部错误），
	// 探测要给出明确原因而不是 stat 空路径的怪错
	profile := profileFrom(t, configWith(workspace, validScriptProfile(declareFile(t, workspace, "a.sh"))), 0)
	profile.ScriptPath = ""

	result := Probe(profile)
	assert.False(t, result.Available)
	assert.Contains(t, result.Reason, "has no script file configured")
	assert.True(t, strings.HasPrefix(result.Reason, "profile "), "reason=%q", result.Reason)
}
