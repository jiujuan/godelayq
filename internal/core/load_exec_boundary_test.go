package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这一组是 TASK-E17 的加载器边界：往监控目录里放一个 exec. 前缀的任务文件，
// 默认既不能进堆也不能被执行。写成独立文件而不是塞进 load_test.go，
// 是因为本组要同时看日志、归档目录与错误目录，写法照 load_format_test.go。

// boundaryLoader 造一个带日志捕获的加载器，返回加载器与那块日志缓冲。
func boundaryLoader(t *testing.T, options LoaderOptions) (*DirectoryLoader, *bytes.Buffer) {
	t.Helper()

	logs := &bytes.Buffer{}
	options.Logger = slog.New(slog.NewTextHandler(logs, nil))
	loader, err := NewDirectoryLoader(NewScheduler(nil, nil, nil), options)
	require.NoError(t, err)
	return loader, logs
}

// writeNamedJobFile 往目录里写一个只带 name 与 delay 的任务文件，返回它的路径。
// 名字与 core/load_watcher_test.go 里同包的 writeJobFile 区分开：那个写的是固定 name 的监控用例。
func writeNamedJobFile(t *testing.T, dir, filename, name string) string {
	t.Helper()

	body, err := json.Marshal(FileJobFormat{Name: name, Delay: "1m"})
	require.NoError(t, err)

	path := filepath.Join(dir, filename)
	require.NoError(t, os.WriteFile(path, body, 0o644))
	return path
}

func countLogLines(text, needle string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			count++
		}
	}
	return count
}

// TestLoader_RejectsExecJobByDefault 是 §5.1：默认配置下 exec. 文件既不进堆也不执行，
// 源文件按 PostLoadAction 离开监控目录，ErrorDir 留下带原因的副本，日志一行 warn。
func TestLoader_RejectsExecJobByDefault(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	errorDir := filepath.Join(dir, "errors")

	called := false
	loader, logs := boundaryLoader(t, LoaderOptions{
		Dir:            dir,
		PostLoadAction: ArchiveAfterLoad,
		ArchiveDir:     archiveDir,
		ErrorDir:       errorDir,
		HandlerMap: map[string]Handler{
			"exec.echo": func(ctx context.Context, job *Job) error {
				called = true
				return nil
			},
		},
	})

	jobFile := writeNamedJobFile(t, dir, "exec_try.json", "exec.echo")
	require.NoError(t, loader.LoadFile(jobFile), "拒绝不是故障，不该以 error 返回")

	assert.Equal(t, 0, loader.scheduler.HeapLen(), "任务没进堆")
	assert.False(t, called, "处理函数没被调用")

	if _, err := os.Stat(jobFile); !os.IsNotExist(err) {
		t.Error("源文件应已按 ArchiveAfterLoad 离开监控目录")
	}
	archived, err := os.ReadDir(archiveDir)
	require.NoError(t, err)
	assert.Len(t, archived, 1, "归档目录里是那一份原文件")

	stored, err := os.ReadFile(filepath.Join(errorDir, "exec_try.json.error"))
	require.NoError(t, err)
	assert.Contains(t, string(stored), execJobRejectReason, "错误副本要写着为什么被拒绝")

	assert.Equal(t, 1, countLogLines(logs.String(), "level=WARN"), "一行 warn 就够")
	assert.Contains(t, logs.String(), `reason="`+execJobRejectReason+`"`)
	assert.Contains(t, logs.String(), "path="+jobFile)
	assert.Equal(t, 0, countLogLines(logs.String(), "level=ERROR"))
}

// TestLoader_AllowExecJobs 是 §5.2：显式打开开关之后，同一份文件正常加载并绑上处理函数。
func TestLoader_AllowExecJobs(t *testing.T) {
	dir := t.TempDir()

	called := false
	loader, logs := boundaryLoader(t, LoaderOptions{
		Dir:            dir,
		PostLoadAction: KeepAfterLoad,
		AllowExecJobs:  true,
		HandlerMap: map[string]Handler{
			"exec.echo": func(ctx context.Context, job *Job) error {
				called = true
				return nil
			},
		},
	})

	jobFile := writeNamedJobFile(t, dir, "exec_allow.json", "exec.echo")
	require.NoError(t, loader.LoadFile(jobFile))

	require.Equal(t, 1, loader.scheduler.HeapLen(), "开关打开时任务照常入队")
	job := loader.scheduler.heap.Peek().(*Job)
	require.NotNil(t, job.Handler, "HandlerMap 里的函数要绑上")

	// 直接调用堆里这份绑定结果：要证明"执行到了的就是加载时绑上的那个函数"，
	// 不需要为了等 worker 把用例挂在时钟上。
	require.NoError(t, job.Handler(context.Background(), job))
	assert.True(t, called)
	assert.NotContains(t, logs.String(), "rejected")
}

// TestLoader_ExecPrefixIsExact 是 §5.3：前缀只看开头，不误伤名字里带 exec 的普通任务；
// 只有前缀没有档位名（"exec."）与带前导空白的写法都按执行器键拒绝。
func TestLoader_ExecPrefixIsExact(t *testing.T) {
	dir := t.TempDir()

	loader, logs := boundaryLoader(t, LoaderOptions{
		Dir:            dir,
		PostLoadAction: KeepAfterLoad,
		ErrorDir:       filepath.Join(dir, "errors"),
	})

	for index, name := range []string{"execx", "payment_check", "my_exec_job", "EXECUTE"} {
		path := writeNamedJobFile(t, dir, "ok-"+strconv.Itoa(index)+".json", name)
		require.NoError(t, loader.LoadFile(path))
	}
	assert.Equal(t, 4, loader.scheduler.HeapLen(), "四个普通任务都进了堆")

	for index, name := range []string{"exec.", " exec.hello"} {
		path := writeNamedJobFile(t, dir, "no-"+strconv.Itoa(index)+".json", name)
		require.NoError(t, loader.LoadFile(path))
	}
	assert.Equal(t, 4, loader.scheduler.HeapLen(), "两个执行器键都没进堆")
	assert.Equal(t, 2, countLogLines(logs.String(), "level=WARN"),
		"空前缀也拒绝：注册键等于 exec. 说明配置写错了，按执行器键处理更安全")
}

// TestLoader_RejectedFileNotLooping 是 §5.4：连扫两轮，第二轮不再报同一个文件，
// 错误副本也不堆积。
func TestLoader_RejectedFileNotLooping(t *testing.T) {
	dir := t.TempDir()
	errorDir := filepath.Join(dir, "errors")

	loader, logs := boundaryLoader(t, LoaderOptions{
		Dir:            dir,
		PostLoadAction: KeepAfterLoad,
		ErrorDir:       errorDir,
	})
	writeNamedJobFile(t, dir, "loop.json", "exec.echo")

	require.NoError(t, loader.ScanAndLoad())
	require.NoError(t, loader.ScanAndLoad())

	assert.Equal(t, 0, loader.scheduler.HeapLen())
	assert.Equal(t, 1, countLogLines(logs.String(), "level=WARN"),
		"Keep 模式下被拒绝的文件也记成已处理，否则每轮扫描都重复拒绝同一个文件")

	entries, err := os.ReadDir(errorDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "错误副本只有一份")
}

// TestLoader_NoErrorDirConfigured 是 §5.5：没配 ErrorDir 时拒绝路径不 panic，
// 源文件仍按 PostLoadAction 处理掉。
func TestLoader_NoErrorDirConfigured(t *testing.T) {
	dir := t.TempDir()

	loader, logs := boundaryLoader(t, LoaderOptions{
		Dir:            dir,
		PostLoadAction: DeleteAfterLoad,
	})
	jobFile := writeNamedJobFile(t, dir, "noerrdir.json", "exec.echo")

	require.NoError(t, loader.LoadFile(jobFile))

	assert.Equal(t, 0, loader.scheduler.HeapLen())
	if _, err := os.Stat(jobFile); !os.IsNotExist(err) {
		t.Error("没配 ErrorDir 也要按删除策略处理源文件，否则会反复扫描")
	}
	assert.Equal(t, 1, countLogLines(logs.String(), "level=WARN"))
	assert.Equal(t, 0, countLogLines(logs.String(), "level=ERROR"), "拒绝不该同时报一条错误")
}

// TestExecPrefixHelper 检查常量与判断函数本身：加载器、注册键与前缀判断共用这一处字面量。
func TestExecPrefixHelper(t *testing.T) {
	assert.Equal(t, "exec.", ExecPrefix)

	for _, name := range []string{"exec.hello", "exec.", "  exec.hello", "exec.a.b"} {
		assert.True(t, IsExecHandlerKey(name), "%q 应按执行器键处理", name)
	}
	for _, name := range []string{"execx", "payment_check", "", "  ", "my.exec"} {
		assert.False(t, IsExecHandlerKey(name), "%q 不是执行器键", name)
	}
}
