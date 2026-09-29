//go:build windows

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 这一组是卡片 §5 的六条 Windows 专属用例，加上它们共用的平台夹具（§3.4）。
//
// 夹具的平台分工：Unix 侧在 proc_unix_test.go，同名函数各自实现。
// Windows 没有"脚本把自己的 PID 写进文件"这种顺手写法，孙进程改成按父子关系找：
// 执行器是在测试进程里起进程的，所以"本测试进程的后代里的辅助进程"就是本次执行派生的那一批，
// 不需要脚本配合，也不会被机器上别的同名进程干扰。
// 辅助进程统一用 ping：系统自带，`ping -n N 127.0.0.1` 大约占住 N-1 秒。

const (
	// helperProcessName 是夹具里那个长命孙进程的可执行文件名。
	helperProcessName = "ping.exe"

	// 下面两个值取自 Windows SDK：
	// PROCESS_QUERY_LIMITED_INFORMATION 只需要查询权限，普通用户对自己起的进程一定拿得到；
	// STILL_ACTIVE(259) 是"进程还在跑"时 GetExitCodeProcess 返回的固定值。
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// grandchildCommand 造一条"直接子进程再派生长命孙进程"的档位。
//
// 外层 cmd 等内层 cmd，内层 cmd 等 ping，所以直接子进程之上至少还有两层，
// 正好是 cmd.Process.Kill() 单独使用时会留下孤儿的那类命令。
// workspace 在 Unix 侧用来让脚本写 PID 文件，Windows 按进程树找，用不到它（§3.4 保持同名同签名）。
func grandchildCommand(t *testing.T, seconds int) core.ExecutorCommand {
	t.Helper()

	return shellCommand(t, "cmd /c ping -n "+strconv.Itoa(seconds)+" 127.0.0.1")
}

// pipeHeldBody 造一条"直接子进程先退出、孙进程还活着并占着输出管道"的命令串。
// start /b 起的进程继承父进程的输出句柄，所以 EOF 要等孙进程也结束才会出现。
func pipeHeldBody(seconds int) string {
	return "start /b cmd /c ping -n " + strconv.Itoa(seconds) + " 127.0.0.1 >nul & echo started"
}

// watchGrandchild 返回一个"取本次执行派生的孙进程 PID"的函数。
// 必须在启动任务之前调用：这一步记录已经存在的辅助进程，返回的函数只认之后新出现的那些。
func watchGrandchild(t *testing.T, workspace string) func() int {
	t.Helper()

	before := helperPIDs(t)
	return func() int {
		var found int
		require.Eventually(t, func() bool {
			for _, pid := range descendantHelperPIDs(t) {
				if !before[pid] {
					found = pid
					return true
				}
			}
			return false
		}, 15*time.Second, 50*time.Millisecond, "没有观察到本次执行派生的孙进程")
		return found
	}
}

// processAlive 判断进程还在不在。
//
// Windows 上没有"发 0 号信号"这种存在性检查，做法是开一个只读句柄再看退出码：
// 句柄打不开说明进程对象已经没了；退出码不是 STILL_ACTIVE 说明进程已经结束。
// 注意这条写法对本测试派生的进程可靠（同用户，权限够），对别人的进程可能因权限不足而误判为"没了"，
// 所以只用于测试断言，不进生产代码。
func processAlive(pid int) bool {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)

	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}

// helperPIDs 返回机器上所有同名辅助进程，用于"本次新起了哪些"的增量判断。
func helperPIDs(t *testing.T) map[int]bool {
	t.Helper()

	found := make(map[int]bool)
	for pid, entry := range processSnapshot(t) {
		if strings.EqualFold(processNameOf(entry), helperProcessName) {
			found[pid] = true
		}
	}
	return found
}

// descendantHelperPIDs 返回本测试进程的后代里的辅助进程 PID。
// 只走"活着的父子链"：父进程已经退出的孤儿不再计入（它的父 PID 已经指向别处），
// 因此这条链恰好等于"本次执行还在管的树"。
func descendantHelperPIDs(t *testing.T) []int {
	t.Helper()

	snapshot := processSnapshot(t)
	children := make(map[uint32][]uint32, len(snapshot))
	for pid, entry := range snapshot {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], uint32(pid))
	}

	var found []int
	queue := []uint32{uint32(os.Getpid())}
	seen := map[uint32]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range children[current] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
			if strings.EqualFold(processNameOf(snapshot[int(child)]), helperProcessName) {
				found = append(found, int(child))
			}
		}
	}
	return found
}

// processSnapshot 抓一份当前进程表（Toolhelp32 快照），键是 PID。
// entry 的 Size 必须先填好，否则 Process32First 直接返回 ERROR_BAD_DRIVER 之类的错误。
func processSnapshot(t *testing.T) map[int]syscall.ProcessEntry32 {
	t.Helper()

	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	require.NoError(t, err)
	defer syscall.CloseHandle(snap)

	entries := make(map[int]syscall.ProcessEntry32)
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = syscall.Process32First(snap, &entry)
	for err == nil {
		entries[int(entry.ProcessID)] = entry
		err = syscall.Process32Next(snap, &entry)
	}
	// Process32Next 走到表尾返回 ERROR_NO_MORE_FILES，这是正常结束
	if !errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
		require.NoError(t, err, "进程快照没有走完")
	}
	return entries
}

func processNameOf(entry syscall.ProcessEntry32) string {
	return syscall.UTF16ToString(entry.ExeFile[:])
}

// withTaskkillPath 把 taskkillPath 换成指定值，用例结束时恢复。
// 清理函数按后进先出运行：需要"先恢复再收尾"的调用方要在这之前注册自己的清理。
func withTaskkillPath(t *testing.T, path string) {
	t.Helper()

	previous := taskkillPath
	taskkillPath = path
	t.Cleanup(func() { taskkillPath = previous })
}

// captureDefaultLogs 把 slog.Default 换成写进缓冲的 handler。
// killTree 的签名由卡片固定（只有 cmd 与 grace），拿不到 Runner 的 logger，
// 因此降级日志只能走默认 logger；这里就地接管，断言才不落空。
func captureDefaultLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// killProcessTreeForTest 用真正的 taskkill 收尾，避免用例留下长命进程。
// 这里不调用 taskkillPath：那个变量在用例里可能被换成假工具，收尾必须用系统工具。
func killProcessTreeForTest(t *testing.T, pid int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run(); err != nil {
		t.Logf("清理测试派生的进程树没有成功（进程可能已经自己退出）: %v", err)
	}
}

// startCancellableRun 在协程里跑一次执行，返回"取消它"和"等它返回"两个动作。
//
// 等返回这个动作可以重复调用：结果只有一份，正文取过一次之后清理函数就只确认"确实回来了"。
// 不做成可重复的话会在清理时卡在通道上——那份结果已经被正文读走了。
func startCancellableRun(t *testing.T, fixture *runnerFixture, jobID string) (cancel func(), wait func() error) {
	t.Helper()

	ctx, cancelFunc := context.WithCancel(context.Background())
	results := make(chan error, 1)
	go func() {
		_, err := fixture.run(ctx, jobID, "")
		results <- err
	}()

	var taken bool
	var runErr error
	wait = func() error {
		t.Helper()
		if !taken {
			select {
			case runErr = <-results:
				taken = true
			case <-time.After(60 * time.Second):
				t.Error("取消之后 Handler 没有返回，进程树可能没被结束")
			}
		}
		return runErr
	}
	t.Cleanup(func() { cancelFunc(); _ = wait() })

	return cancelFunc, wait
}

// TestKillTree_Windows_GrandchildDies 是 §5.1：取消之后 cmd.exe 派生出去的那一层要跟着没。
func TestKillTree_Windows_GrandchildDies(t *testing.T) {
	fixture := newRunnerFixture(t, grandchildCommand(t, 300), nil)
	watch := watchGrandchild(t, fixture.profile.Workspace)
	cancel, wait := startCancellableRun(t, fixture, "job-win-grandchild")

	pid := watch()
	require.True(t, processAlive(pid), "前提：孙进程此刻还在跑")

	cancel()

	failure := asExitError(t, wait())
	assert.True(t, failure.Cancelled, "这条路径是取消，不是超时")

	require.Eventually(t, func() bool { return !processAlive(pid) },
		10*time.Second, 50*time.Millisecond, "孙进程 %d 在取消之后仍然活着", pid)
	assert.Empty(t, descendantHelperPIDs(t), "本次执行派生的辅助进程应该全部结束，不该留下孤儿")
}

// TestKillTree_Windows_TaskkillMissing 是 §5.2：机器上没有 taskkill（精简容器镜像）时，
// killTree 要降级——只结束直接子进程，记一条 warn，不把错误抛回 os/exec 去改写执行结论。
func TestKillTree_Windows_TaskkillMissing(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "cmd /c ping -n 300 127.0.0.1 >nul")
	watch := watchGrandchild(t, "")
	require.NoError(t, cmd.Start())
	direct := cmd.Process.Pid

	t.Cleanup(func() {
		for _, pid := range descendantHelperPIDs(t) {
			killProcessTreeForTest(t, pid)
		}
		_ = cmd.Wait()
	})

	grandchild := watch()
	require.True(t, processAlive(grandchild), "前提：孙进程此刻还在跑")

	logs := captureDefaultLogs(t, slog.LevelInfo)
	missing := filepath.Join(t.TempDir(), "no-such-taskkill.exe")
	withTaskkillPath(t, missing)

	require.NoError(t, killTree(cmd, killGrace), "taskkill 缺失时应当降级返回成功，而不是把执行结论改掉")
	assert.Contains(t, logs.String(), "could not clean the whole process tree")
	assert.Contains(t, logs.String(), missing, "warn 日志要说明是哪个可执行文件没跑起来")

	require.Eventually(t, func() bool { return !processAlive(direct) },
		10*time.Second, 50*time.Millisecond, "直接子进程 %d 没有被 Kill 结束", direct)

	// 本卡的降级行为要说清楚：没有 taskkill 就只有 cmd.Process.Kill()，孙进程管不到。
	// 这条断言不是"实现失败"，是卡片 §3.3 记录的限制；E11a（Job Object）才负责把它消掉。
	if processAlive(grandchild) {
		t.Logf("降级路径留住了孙进程 %d：这正是 taskkill /T 承担的那部分能力", grandchild)
	} else {
		t.Logf("孙进程 %d 也一起没了：Windows 在父进程结束时可能顺带清理了句柄，但不作为断言依据", grandchild)
	}
}

// TestKillTree_Windows_TimeoutPath 是 §5.3：超时也要走整树终止，结论落在超时上。
func TestKillTree_Windows_TimeoutPath(t *testing.T) {
	command := grandchildCommand(t, 300)
	command.Timeout = time.Second
	fixture := newRunnerFixture(t, command, nil)

	started := time.Now()
	_, err := fixture.run(context.Background(), "job-win-timeout", "")
	elapsed := time.Since(started)

	failure := asExitError(t, err)
	assert.True(t, failure.TimedOut)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
	assert.False(t, failure.Permanent(), "超时可重试")
	assert.Less(t, elapsed, killGrace+processWaitDelay+5*time.Second,
		"超时之后应当在宽限期加管道兜底的量级内返回，实际用了 %v", elapsed)

	require.Eventually(t, func() bool { return len(descendantHelperPIDs(t)) == 0 },
		10*time.Second, 50*time.Millisecond, "超时之后本次执行派生的进程还活着")
}

// TestTaskkillOutput_NotInArtifact 是 §5.4：taskkill 自己的输出只能进日志，不能混进产物文件。
//
// 做法是把 taskkillPath 换成一个"只会打印一行标记"的批处理：
// 标记出现在日志里说明它确实被调用并产生了输出，同时产物里必须干净得像只有任务自己的输出。
func TestTaskkillOutput_NotInArtifact(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fake_taskkill.cmd")
	require.NoError(t, os.WriteFile(fake,
		[]byte("@echo off\r\necho TASKKILL_OUTPUT_MARKER\r\nexit /b 0\r\n"), 0o600))
	withTaskkillPath(t, fake)

	logs := captureDefaultLogs(t, slog.LevelDebug)

	command := shellCommand(t, "echo job-output-marker & cmd /c ping -n 6 127.0.0.1 >nul")
	fixture := newRunnerFixture(t, command, nil)
	watch := watchGrandchild(t, fixture.profile.Workspace)
	cancel, wait := startCancellableRun(t, fixture, "job-win-artifact")

	watch()
	cancel()
	assert.True(t, asExitError(t, wait()).Cancelled, "这条路径是取消，不是超时")

	out := string(fixture.stream("job-win-artifact", 1, "out"))
	errStream := string(fixture.stream("job-win-artifact", 1, "err"))
	assert.Equal(t, "job-output-marker", strings.TrimSpace(out),
		"产物里应当只有任务自己的输出")
	assert.NotContains(t, out, "TASKKILL_OUTPUT_MARKER")
	assert.NotContains(t, errStream, "TASKKILL_OUTPUT_MARKER")
	assert.Contains(t, logs.String(), "TASKKILL_OUTPUT_MARKER",
		"taskkill 的输出应当记进 debug 日志，而不是丢掉或写进产物")
}

// TestWaitDelay_Windows 是 §5.5，与 E10 §5.4 等价：
// 孙进程占着输出句柄、直接子进程先退出时，Wait 必须在 WaitDelay 之后放开。
func TestWaitDelay_Windows(t *testing.T) {
	fixture := newRunnerFixture(t, shellCommand(t, pipeHeldBody(6)), nil)

	started := time.Now()
	_, err := fixture.run(context.Background(), "job-win-pipe-held", "")
	elapsed := time.Since(started)

	assert.GreaterOrEqual(t, elapsed, processWaitDelay,
		"这条路径本就该等满 WaitDelay，太快返回说明管道没被真正持有")
	assert.Less(t, elapsed, processWaitDelay+10*time.Second,
		"Wait 必须在 WaitDelay 之后放开，不能挂到执行超时（实际 %v）", elapsed)

	failure := asExitError(t, err)
	assert.Equal(t, "output stayed open past the wait limit", failure.Reason)

	// 进程自己的输出已经落盘，兜底不该把已写的内容也丢掉
	content := fixture.stream("job-win-pipe-held", 1, "out")
	assert.Equal(t, "started", strings.TrimSpace(string(content)))
}

// TestSignalOf_WindowsIsEmpty 是 §5.6：Windows 没有信号概念，
// Signal 恒为空串，响应摘要里这个字段应当整个省略。
func TestSignalOf_WindowsIsEmpty(t *testing.T) {
	fixture := newRunnerFixture(t, shellCommand(t, "exit 5"), nil)

	job, err := fixture.run(context.Background(), "job-win-signal", "")
	failure := asExitError(t, err)

	assert.Equal(t, 5, failure.ExitCode)
	assert.Empty(t, failure.Signal, "Windows 的退出状态里没有终止信号")
	require.NotNil(t, job.Exec)
	assert.Empty(t, job.Exec.Signal)

	encoded, err := json.Marshal(job.Exec)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"signal"`,
		"空信号不该出现在响应 JSON 里（core.ExecMeta.Signal 带 omitempty）")

	// 被我们自己结束的进程同样没有信号名，只有 TimedOut/Cancelled 标记
	longRunning := newRunnerFixture(t, grandchildCommand(t, 300), nil)
	watch := watchGrandchild(t, longRunning.profile.Workspace)
	cancel, wait := startCancellableRun(t, longRunning, "job-win-signal-cancel")

	watch()
	cancel()

	cancelled := asExitError(t, wait())
	assert.True(t, cancelled.Cancelled)
	assert.Empty(t, cancelled.Signal, "取消路径在 Windows 上也拿不到信号名")
}
