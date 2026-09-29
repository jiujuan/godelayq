//go:build !windows

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// grandchildScript 生成"起一个后台 sleep、把它的 PID 写进 workspace 里的文件、然后等它"的脚本。
// 这正是本卡要消掉的那类脚本：只杀直接子进程的话，那个 sleep 会活到最后。
func grandchildScript(pidFile string, seconds int) string {
	return "sleep " + strconv.Itoa(seconds) + " & echo $! > " + pidFile + "; wait"
}

// processAlive 判断进程还在不在。发 0 号信号只做存在性检查；
// 权限不足（EPERM）说明进程存在只是不属于当前用户，同样算"还活着"。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

// readRecordedPID 读取脚本写下的孙进程 PID。文件在档位的工作目录里（cmd.Dir 就是 workspace）。
func readRecordedPID(t *testing.T, workspace, file string) int {
	t.Helper()

	path := filepath.Join(workspace, file)
	var last string
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			last = fmt.Sprintf("read %s: %v", file, err)
			return false
		}
		text := strings.TrimSpace(string(data))
		pid, err := strconv.Atoi(text)
		if err != nil || pid <= 0 {
			last = "内容不是 PID: " + text
			return false
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "等不到脚本写下的孙进程 PID，最后状态：%s", last)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	return pid
}

// sleeperPIDs 返回当前名为 sleep 的进程集合（/proc/<pid>/stat 的第二个字段是命令名）。
// 第三.5 条用例用它统计"残留的 sleep 有没有被清干净"。
func sleeperPIDs(t *testing.T) map[int]bool {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("cannot list /proc on this system: %v", err)
	}

	found := make(map[int]bool)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue // 进程刚好在这两步之间退出，跳过它
		}
		text := string(data)
		open, end := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
		if open < 0 || end <= open {
			continue
		}
		if text[open+1:end] == "sleep" {
			found[pid] = true
		}
	}
	return found
}

// TestKillTree_GrandchildDies 是本卡的核心用例（§5.1）：
// 脚本 fork 出去的后台进程必须跟着一起结束，不能留下"任务显示没在跑、机器上还有进程"。
func TestKillTree_GrandchildDies(t *testing.T) {
	fixture := newRunnerFixture(t, shellCommand(t, grandchildScript("grandchild.pid", 300)), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fixture.run(ctx, "job-kill-tree", "")
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("取消之后 Handler 没有返回，进程树可能没被结束")
		}
	})

	pid := readRecordedPID(t, fixture.profile.Workspace, "grandchild.pid")
	require.True(t, processAlive(pid), "前提：孙进程此刻还在跑")

	cancel()

	require.Eventually(t, func() bool { return !processAlive(pid) },
		10*time.Second, 50*time.Millisecond, "孙进程 %d 在取消之后仍然活着", pid)
}

// TestKillTree_TimeoutPath 走超时那条路径（§5.2）：结果与取消一致，但标记要落在超时上。
func TestKillTree_TimeoutPath(t *testing.T) {
	command := shellCommand(t, grandchildScript("timeout.pid", 300))
	command.Timeout = time.Second
	fixture := newRunnerFixture(t, command, nil)

	started := time.Now()
	_, err := fixture.run(context.Background(), "job-kill-timeout", "")
	elapsed := time.Since(started)
	failure := asExitError(t, err)

	pid := readRecordedPID(t, fixture.profile.Workspace, "timeout.pid")

	assert.True(t, failure.TimedOut)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
	assert.False(t, failure.Permanent(), "超时可重试")
	assert.Less(t, elapsed, killGrace+processWaitDelay+5*time.Second,
		"超时之后应当在宽限期加管道兜底的量级内返回，实际用了 %v", elapsed)

	// 孙进程可能还没被系统收尸，用 Eventually 给一点时间
	require.Eventually(t, func() bool { return !processAlive(pid) },
		10*time.Second, 50*time.Millisecond, "超时之后孙进程 %d 仍然活着", pid)
}

// TestKillTree_NoOrphanAfterCancelStress 一次并发 20 条带后台子进程的任务再全部取消（§5.3）。
// 单条用例过不代表批量也过：进程组写法漏掉任何一条路径都会在这里留下成片的孤儿。
func TestKillTree_NoOrphanAfterCancelStress(t *testing.T) {
	const jobs = 20

	command := shellCommand(t, "sleep 600 & wait")
	command.MaxParallel = jobs
	fixture := newRunnerFixture(t, command, nil)

	before := sleeperPIDs(t)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, _ = fixture.run(ctx, "job-stress-"+strconv.Itoa(index), "")
		}(i)
	}

	require.Eventually(t, func() bool {
		return len(fixture.runner.permits) >= jobs/2
	}, 10*time.Second, 50*time.Millisecond, "并发任务没有跑起来，这条用例没测到东西")

	cancel()
	wg.Wait()

	require.Eventually(t, func() bool {
		for pid := range sleeperPIDs(t) {
			if !before[pid] {
				return false
			}
		}
		return true
	}, 15*time.Second, 100*time.Millisecond, "取消之后仍有新起的 sleep 进程没被结束")
}

// TestWaitDelay_PipeHeldByGrandchild 是 §5.4 那条最容易忘的用例：
// 孙进程继承了 stdout 的写端，直接子进程先退出，此时没人再写、也没人关管道。
// 没有 cmd.WaitDelay 的话 Wait 会一直等 EOF，Handler 永不返回。
func TestWaitDelay_PipeHeldByGrandchild(t *testing.T) {
	fixture := newRunnerFixture(t, shellCommand(t, "sleep 5 & echo started"), nil)

	started := time.Now()
	_, err := fixture.run(context.Background(), "job-pipe-held", "")
	elapsed := time.Since(started)

	assert.GreaterOrEqual(t, elapsed, processWaitDelay,
		"这条路径本就该等满 WaitDelay，太快返回说明管道没被真正持有")
	assert.Less(t, elapsed, processWaitDelay+10*time.Second,
		"Wait 必须在 WaitDelay 之后放开，不能挂到执行超时（%v）", elapsed)

	failure := asExitError(t, err)
	assert.Equal(t, "output stayed open past the wait limit", failure.Reason)

	// 进程自己的输出已经落盘，兜底不该把已写的内容也丢掉
	content := fixture.stream("job-pipe-held", 1, "out")
	assert.Equal(t, "started", strings.TrimSpace(string(content)))
}

// TestSignalOf 验信号名解析（§5.5）：被信号结束时 Signal 要有名字，
// 而"没跑起来"与"正常退出"两条路径都该留空串。
func TestSignalOf(t *testing.T) {
	cases := []struct {
		label      string
		id         string
		body       string
		wantSignal string
	}{
		{"SIGTERM", "job-signal-term", "kill -TERM $$", "SIGTERM"},
		{"SIGKILL", "job-signal-kill", "kill -KILL $$", "SIGKILL"},
		{"正常退出没有信号", "job-signal-clean", "exit 0", ""},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			fixture := newRunnerFixture(t, shellCommand(t, tc.body), nil)

			job, err := fixture.run(context.Background(), tc.id, "")

			if tc.wantSignal == "" {
				require.NoError(t, err)
				require.NotNil(t, job.Exec)
				assert.Empty(t, job.Exec.Signal, "正常退出没有致死信号，摘要里不该出现名字")
				return
			}

			failure := asExitError(t, err)
			assert.Equal(t, tc.wantSignal, failure.Signal)
			assert.NotZero(t, failure.ExitCode, "被信号结束的进程不该报告一个 0 退出码")
			require.NotNil(t, job.Exec)
			assert.Equal(t, tc.wantSignal, job.Exec.Signal)
		})
	}
}

// TestGracefulShutdown_NotStalled 覆盖 §5.6：一个长任务正在执行时关停调度器，
// 关停应当在有界时间内完成，而不是等脚本自己跑完。
//
// 这里能成立的边界来自本卡的两步兜底（先 SIGTERM 整组、再 WaitDelay 关管道），
// 不是来自 scheduler.shutdown_timeout：那个配置目前只管 cmd/server 的 HTTP 优雅关闭，
// core 的 Scheduler.Stop 取消在途任务之后是无期限等 worker 的（见卡片第 10 节的记录）。
// 容差取 2 倍，避免机器负载把边界内的用例推成不稳定失败。
func TestGracefulShutdown_NotStalled(t *testing.T) {
	command := shellCommand(t, "sleep 30")
	command.Timeout = 5 * time.Minute
	fixture := newRunnerFixture(t, command, nil)

	store, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	scheduler := core.NewScheduler(store, nil, nil, core.WithLogger(quietLogger()))
	scheduler.RegisterHandler(fixture.profile.HandlerKey(), fixture.runner.Handler())

	// 先记下机器上本来就有的 sleep 进程：别的用例（比如管道兜底那条）可能还留着几个，
	// 这条用例只对"本次新起的进程有没有被清掉"下结论。
	before := sleeperPIDs(t)

	require.NoError(t, scheduler.Schedule(&core.Job{
		ID:        "job-shutdown",
		Name:      fixture.profile.HandlerKey(),
		Type:      fixture.profile.HandlerKey(),
		TriggerAt: time.Now().Add(200 * time.Millisecond),
	}))
	scheduler.Start()

	// 等任务真的进入执行：许可被占住说明 Handler 已经进去并卡在进程上
	require.Eventually(t, func() bool { return len(fixture.runner.permits) == 1 },
		10*time.Second, 20*time.Millisecond, "任务没有在 30 秒脚本里跑起来，这条用例没测到东西")

	started := time.Now()
	scheduler.Stop()
	elapsed := time.Since(started)

	assert.Less(t, elapsed, 2*(killGrace+processWaitDelay)+3*time.Second,
		"关停花了 %v，说明取消没有在宽限期内结束进程树", elapsed)

	require.Eventually(t, func() bool {
		for pid := range sleeperPIDs(t) {
			if !before[pid] {
				return false
			}
		}
		return true
	}, 10*time.Second, 100*time.Millisecond, "关停之后仍有本次新起的 sleep 进程残留")
}
