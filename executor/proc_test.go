package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// runnerShell 返回本机上一个"能把任意命令串当程序跑"的解释器与它的执行参数前缀。
//
// Unix 用 sh -c，Windows 用 cmd /c：两者都在默认 executors.runtime_allow 里，
// 也都是系统自带的，测试因此不依赖目标机器装了 node 或 php（卡片 §5 的要求）。
// 取不到就跳过整组进程测试，跳过理由会写进测试输出，不会静默通过。
func runnerShell(t *testing.T) (string, []string) {
	t.Helper()

	name, prefix := "sh", []string{"-c"}
	if runtime.GOOS == "windows" {
		name, prefix = "cmd", []string{"/c"}
	}
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("program %q is not available on this machine, process tests cannot run", name)
	}
	return name, prefix
}

// shellCommand 造一条二进制档位：程序是 runnerShell，命令串写在 fixed_args 里。
// 命令串属于配置（审查过的那一侧），不是 payload 能影响的输入。
func shellCommand(t *testing.T, body string) core.ExecutorCommand {
	name, prefix := runnerShell(t)
	return core.ExecutorCommand{
		Name:      "runner_case",
		Kind:      string(KindBinary),
		Program:   name,
		FixedArgs: append(append([]string{}, prefix...), body),
	}
}

// loopBody 生成"打印 lines 行同样内容"的命令串。
func loopBody(t *testing.T, lines int, line string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		return "for /L %A in (1,1," + strconv.Itoa(lines) + ") do @echo " + line
	}
	return "i=0; while [ $i -lt " + strconv.Itoa(lines) + " ]; do echo " + line + "; i=$((i+1)); done"
}

// slowCommand 造一条"运行 seconds 秒"的档位，并返回它的程序名（要加进 runtime_allow）。
//
// 等待者必须是直接子进程本身，不能套在 cmd/sh 里面：取消或超时时我们杀掉的是那层 shell，
// 真正还在计时的孙进程活着，还会继续占住工作目录（用例结束时清理临时目录就会失败）。
// 整棵进程树的终止归 TASK-E10（Unix）与 TASK-E11（Windows），本卡的默认实现只杀直接子进程。
func slowCommand(t *testing.T, seconds int) (core.ExecutorCommand, string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		return core.ExecutorCommand{
			Name:      "runner_case",
			Kind:      string(KindBinary),
			Program:   "ping",
			FixedArgs: []string{"-n", strconv.Itoa(seconds + 1), "127.0.0.1"},
		}, "ping"
	}
	return core.ExecutorCommand{
		Name:      "runner_case",
		Kind:      string(KindBinary),
		Program:   "sleep",
		FixedArgs: []string{strconv.Itoa(seconds)},
	}, "sleep"
}

// slowFixture 用 slowCommand 建档位并构造 Runner：mutate 负责超时与并发这些逐用例不同的项。
func slowFixture(t *testing.T, seconds int, mutate func(*core.ExecutorCommand)) *runnerFixture {
	t.Helper()

	command, program := slowCommand(t, seconds)
	if mutate != nil {
		mutate(&command)
	}
	return newRunnerFixture(t, command, func(cfg *core.Config, _ *ArtifactOptions) {
		cfg.Executors.RuntimeAllow = append(cfg.Executors.RuntimeAllow, program)
	})
}

// runnerFixture 是一次执行所需的全部对象：档位、Runner、产物存储与日志缓冲。
type runnerFixture struct {
	t         *testing.T
	cfg       core.Config
	profile   *Profile
	runner    *Runner
	artifacts *ArtifactStore
	logs      *bytes.Buffer
}

// newRunnerFixture 按 tune 调整配置与产物上限后构造 Runner。
// tune 为 nil 时用默认值（产物单流上限取全局默认）。
func newRunnerFixture(t *testing.T, command core.ExecutorCommand, tune func(*core.Config, *ArtifactOptions)) *runnerFixture {
	t.Helper()

	cfg := configWith(t.TempDir(), command)
	opts := ArtifactOptions{Dir: filepath.Join(t.TempDir(), "exec"), MaxBytes: core.DefaultExecMaxOutputBytes}
	if tune != nil {
		tune(&cfg, &opts)
	}

	normalized := cfg.Normalized()
	profiles := mustLoad(t, normalized)
	require.Len(t, profiles, 1)

	store, err := NewArtifactStore(opts, quietLogger())
	require.NoError(t, err)

	fixture := &runnerFixture{
		t:         t,
		cfg:       normalized,
		profile:   profiles[0],
		artifacts: store,
		logs:      &bytes.Buffer{},
	}
	fixture.runner = NewRunner(profiles[0], store, normalized.Executors, bufferLogger(fixture.logs))
	return fixture
}

// run 用给定上下文执行一次任务。payload 传空串表示"不带 payload"。
func (f *runnerFixture) run(ctx context.Context, jobID, payload string) (*core.Job, error) {
	f.t.Helper()

	job := &core.Job{ID: jobID, Name: f.profile.HandlerKey(), Attempts: 1}
	if payload != "" {
		job.Payload = []byte(payload)
	}
	return job, f.runner.Handler()(ctx, job)
}

// stream 读出某次尝试的产物内容。
func (f *runnerFixture) stream(jobID string, attempt int, stream string) []byte {
	f.t.Helper()

	data, _, err := f.artifacts.Read(jobID, attempt, stream, 0)
	require.NoError(f.t, err)
	return data
}

// streamSize 返回产物文件的字节数，文件不存在时报错。
func (f *runnerFixture) streamSize(jobID string, attempt int, stream string) int64 {
	f.t.Helper()

	size, err := f.artifacts.Stat(jobID, attempt, stream)
	require.NoError(f.t, err)
	return size
}

func asExitError(t *testing.T, err error) *ExitError {
	t.Helper()

	require.Error(t, err)
	var failure *ExitError
	require.True(t, errors.As(err, &failure), "错误应该是 *ExitError，实际是 %T", err)
	return failure
}

func TestRunner_ExitCode(t *testing.T) {
	fixture := newRunnerFixture(t, shellCommand(t, "exit 3"), nil)

	job, err := fixture.run(context.Background(), "job-exit-code", "")
	failure := asExitError(t, err)

	assert.Equal(t, 3, failure.ExitCode, "退出码要原样带出来：脚本自己报的错才是可解释的")
	assert.False(t, failure.TimedOut)
	assert.False(t, failure.Cancelled)
	assert.True(t, failure.Permanent(), "退出码没写进 retry_on_exit 就不该重试")

	require.NotNil(t, job.Exec)
	assert.Equal(t, 3, job.Exec.ExitCode)
	assert.Equal(t, string(KindBinary), job.Exec.Kind)
	assert.Equal(t, fixture.profile.Name, job.Exec.Profile)
	assert.True(t, job.Exec.Permanent)
	assert.Equal(t, core.ArtifactAvailable, job.Exec.Artifact)
}

func TestRunner_RetryOnExitKeepsRetry(t *testing.T) {
	command := shellCommand(t, "exit 7")
	command.RetryOnExit = []int{7}
	fixture := newRunnerFixture(t, command, nil)

	_, err := fixture.run(context.Background(), "job-retry-exit", "")

	assert.False(t, asExitError(t, err).Permanent(), "档位声明了要重试这个退出码")
}

func TestRunner_SuccessPreview(t *testing.T) {
	fixture := newRunnerFixture(t, shellCommand(t, "echo hello"), nil)

	job, err := fixture.run(context.Background(), "job-hello", "")
	require.NoError(t, err)

	require.NotNil(t, job.Exec)
	assert.Zero(t, job.Exec.ExitCode)
	assert.NotZero(t, job.Exec.OutBytes, "一行输出的字节数要记进摘要")
	assert.Equal(t, "hello", strings.TrimSpace(job.Exec.Preview))
	assert.Equal(t, core.ArtifactAvailable, job.Exec.Artifact)
	assert.NotZero(t, job.Exec.DurationMs, "耗时落到毫秒；仍是 0 说明这次执行没有真的等待进程")
	assert.False(t, job.Exec.Truncated)
	assert.Equal(t, int64(len(fixture.stream("job-hello", 1, "out"))), job.Exec.OutBytes)
}

// TestRunner_LargeOutputComplete 是卡片 §5.3 的那条：子进程输出 2MB 并立刻退出。
//
// 管道的系统缓冲通常只有 64KB，剩下的量只有"等拷贝协程结束"才写得完。
// Handler 若在拷贝完成前返回，这里少掉的就是输出的尾部——产物文件与摘要会同时缺一截，
// 而且缺多少取决于调度时机，是最难复现的那类问题。
func TestRunner_LargeOutputComplete(t *testing.T) {
	const lines = 20000
	line := strings.Repeat("x", 100)

	fixture := newRunnerFixture(t, shellCommand(t, loopBody(t, lines, line)),
		func(cfg *core.Config, opts *ArtifactOptions) { opts.MaxBytes = 8 << 20 })

	job, err := fixture.run(context.Background(), "job-large-output", "")
	require.NoError(t, err)

	newline := int64(1)
	if runtime.GOOS == "windows" {
		newline = 2 // cmd 的 echo 以 \r\n 结尾
	}
	expected := int64(lines) * (int64(len(line)) + newline)

	require.NotNil(t, job.Exec)
	assert.Equal(t, expected, job.Exec.OutBytes, "摘要字节数必须等于子进程写出的总量")
	assert.Equal(t, expected, fixture.streamSize("job-large-output", 1, "out"), "产物文件少一个字节就是尾部丢了")
	assert.Equal(t, expected, int64(len(fixture.stream("job-large-output", 1, "out"))))
	assert.False(t, job.Exec.Truncated, "上限已放到 8MB，2MB 输出不该被判成截断")
}

func TestRunner_OutputCappedAndTruncated(t *testing.T) {
	const lines = 300
	line := strings.Repeat("y", 80)

	fixture := newRunnerFixture(t, shellCommand(t, loopBody(t, lines, line)),
		func(cfg *core.Config, opts *ArtifactOptions) { opts.MaxBytes = 4096 })

	job, err := fixture.run(context.Background(), "job-capped-output", "")
	require.NoError(t, err)

	require.NotNil(t, job.Exec)
	assert.True(t, job.Exec.Truncated, "到达上限即停止写入要留下标记")
	assert.Equal(t, int64(4096), job.Exec.OutBytes)
	assert.Equal(t, int64(4096), fixture.streamSize("job-capped-output", 1, "out"))
}

// TestPreview_PrefersStderr 钉住"失败时那一行摘要来自哪条流"。
// 只看 stdout 的话，脚本的报错信息永远进不了事件与列表。
func TestPreview_PrefersStderr(t *testing.T) {
	t.Run("两条流都有内容时用 stderr", func(t *testing.T) {
		fixture := newRunnerFixture(t, shellCommand(t, redirectBody(t, "progress", "boom", 4)), nil)

		job, err := fixture.run(context.Background(), "job-both-streams", "")
		asExitError(t, err)

		require.NotNil(t, job.Exec)
		assert.Equal(t, "boom", strings.TrimSpace(job.Exec.Preview))
		assert.Equal(t, "progress", strings.TrimSpace(string(fixture.stream("job-both-streams", 1, "out"))))
		assert.NotZero(t, job.Exec.ErrBytes)
	})

	t.Run("只有 stdout 时回落到 stdout", func(t *testing.T) {
		fixture := newRunnerFixture(t, shellCommand(t, "echo only-stdout"+exitSuffix(t, 5)), nil)

		job, err := fixture.run(context.Background(), "job-stdout-only", "")
		asExitError(t, err)

		require.NotNil(t, job.Exec)
		assert.Equal(t, "only-stdout", strings.TrimSpace(job.Exec.Preview))
	})
}

// redirectBody 生成"stdout 写 outLine、stderr 写 errLine、退出码为 code"的命令串。
func redirectBody(t *testing.T, outLine, errLine string, code int) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		return "echo " + outLine + "& echo " + errLine + " 1>&2& exit /b " + strconv.Itoa(code)
	}
	return "echo " + outLine + "; echo " + errLine + " >&2; exit " + strconv.Itoa(code)
}

// exitSuffix 生成"以 code 退出"的那半句命令串，写法随平台而变。
func exitSuffix(t *testing.T, code int) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		return "& exit /b " + strconv.Itoa(code)
	}
	return "; exit " + strconv.Itoa(code)
}

func TestRunner_BadSubmissionNeverStartsProcess(t *testing.T) {
	command := shellCommand(t, "echo hello")
	command.Args = []core.ExecutorArg{{Name: "day", Required: true}}
	command.ArgsRender = []string{"--day={day}"}
	fixture := newRunnerFixture(t, command, nil)

	// 未声明的键与缺必填参数都要在起进程之前挡掉
	for _, payload := range []string{`{"args":{"nope":"x"}}`, `{"args":{}}`} {
		_, err := fixture.run(context.Background(), "job-bad-"+strconv.Itoa(len(payload)), payload)
		failure := asExitError(t, err)

		assert.Equal(t, "invalid submission", failure.Reason)
		assert.True(t, failure.Permanent())
		assert.Contains(t, failure.Error(), "args")
	}

	entries, err := os.ReadDir(fixture.artifacts.Dir())
	require.NoError(t, err)
	assert.Empty(t, entries, "校验没过就不该留下任何产物目录，那是没有起进程的证据")
}

func TestRunner_ProgramMissing(t *testing.T) {
	command := core.ExecutorCommand{
		Name:    "missing_binary",
		Kind:    string(KindBinary),
		Program: "bin/godelayq-does-not-exist.exe",
	}
	fixture := newRunnerFixture(t, command, nil)
	require.False(t, Probe(fixture.profile).Available, "这条档位的前提是探测判定它不可用")

	job, err := fixture.run(context.Background(), "job-missing-program", "")
	failure := asExitError(t, err)

	assert.True(t, failure.Permanent())
	assert.Equal(t, "cannot start the process", failure.Reason)
	// 没跑起来的进程没有退出码：留零值，不写一个看起来像结论的假数字
	assert.Zero(t, failure.ExitCode)

	require.NotNil(t, job.Exec, "起进程失败同样要留下可解释的摘要")
	assert.Equal(t, string(KindBinary), job.Exec.Kind)
	assert.Equal(t, "missing_binary", job.Exec.Profile)
	assert.NotZero(t, job.Exec.DurationMs)
	assert.Zero(t, job.Exec.ExitCode)
	assert.True(t, job.Exec.Permanent)
}

func TestRunner_Timeout(t *testing.T) {
	fixture := slowFixture(t, 30, func(command *core.ExecutorCommand) {
		command.Timeout = 200 * time.Millisecond
	})

	started := time.Now()
	_, err := fixture.run(context.Background(), "job-timeout", "")
	failure := asExitError(t, err)

	assert.True(t, failure.TimedOut)
	assert.False(t, failure.Cancelled)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"调度器靠这条把执行归到超时，事件与重试分类才不用改（设计文档 §6.3）")
	assert.False(t, failure.Permanent(), "超时可以再试一次")
	assert.Less(t, time.Since(started), 10*time.Second, "超时之后必须真的把进程结束掉")
}

func TestRunner_Cancelled(t *testing.T) {
	fixture := slowFixture(t, 30, func(command *core.ExecutorCommand) {
		command.Timeout = 20 * time.Second
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fixture.run(ctx, "job-cancelled", "")
		done <- err
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	failure := asExitError(t, <-done)
	assert.True(t, failure.Cancelled)
	assert.False(t, failure.TimedOut)
	assert.True(t, errors.Is(failure, context.Canceled), "取消要走调度器的中断分支，不消耗重试次数")
}

func TestMaxParallel(t *testing.T) {
	fixture := slowFixture(t, 30, func(command *core.ExecutorCommand) {
		command.MaxParallel = 1
		command.Timeout = 30 * time.Second
	})

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	t.Cleanup(cancelFirst)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = fixture.run(firstCtx, "job-busy-holder", "")
	}()

	// 第二个任务要等第一个真的拿到许可之后再提交，否则两条会同时抢许可，用例变成掷骰子
	require.Eventually(t, func() bool { return len(fixture.runner.permits) == 1 },
		2*time.Second, 10*time.Millisecond, "第一个执行没有拿到档位许可，后面的断言没有意义")

	// 等待许可的上限就是本次的生效超时：payload 把超时压到 300ms，等不到许可就该给出结论
	started := time.Now()
	_, err := fixture.run(context.Background(), "job-waits-for-permit", `{"timeout":"300ms"}`)
	failure := asExitError(t, err)

	assert.Equal(t, "concurrency limit", failure.Reason)
	assert.True(t, failure.TimedOut, "等不到许可按超时归类，调度器的重试与告警口径才一致")
	assert.False(t, failure.Permanent())
	assert.GreaterOrEqual(t, time.Since(started), 300*time.Millisecond)

	cancelFirst()
	wg.Wait()
}

func TestRunner_NoSecretInLogs(t *testing.T) {
	command := shellCommand(t, "echo done")
	command.Args = []core.ExecutorArg{{Name: "token", Secret: true}}
	command.ArgsRender = []string{"--token={token}"}
	fixture := newRunnerFixture(t, command, nil)

	_, err := fixture.run(context.Background(), "job-secret", `{"args":{"token":"s3cr3t-value"}}`)
	require.NoError(t, err)

	output := fixture.logs.String()
	assert.NotContains(t, output, "s3cr3t-value", "参数值不进气动日志")
	assert.NotContains(t, output, "--token", "整条命令行都不进气动日志")
	assert.NotContains(t, strings.ToLower(output), "payload", "payload 不进气动日志")

	assert.Contains(t, output, "job_id=job-secret")
	assert.Contains(t, output, "handler_key=exec.runner_case")
	assert.Contains(t, output, "profile=runner_case")
	assert.Contains(t, output, "exit_code=0")
	assert.Contains(t, output, "duration_ms=")
}

func TestRunner_HttpProfileIsNotExecutedByProcessRunner(t *testing.T) {
	command := core.ExecutorCommand{
		Name:         "rebuild",
		Kind:         string(KindHTTP),
		Method:       "POST",
		Body:         "none",
		URLTemplate:  "https://api.internal/v1/rebuild",
		AllowedHosts: []string{"api.internal"},
	}
	fixture := newRunnerFixture(t, command, nil)

	job, err := fixture.run(context.Background(), "job-http-profile", "")
	failure := asExitError(t, err)

	assert.Contains(t, failure.Reason, "TASK-E15", "错误要说明这条分支归哪一卡，而不是含糊的 not implemented")
	assert.True(t, failure.Permanent())
	require.NotNil(t, job.Exec)
	assert.Equal(t, string(KindHTTP), job.Exec.Kind)
	assert.Empty(t, job.Exec.Artifact, "还没建产物文件就不该声称产物可用")
}

func TestRunner_WithoutArtifactStore(t *testing.T) {
	command := shellCommand(t, "echo hello")
	fixture := newRunnerFixture(t, command, nil)
	noStore := NewRunner(fixture.profile, nil, fixture.cfg.Executors, quietLogger())

	err := noStore.Handler()(context.Background(), &core.Job{ID: "job-no-store", Attempts: 1})
	failure := asExitError(t, err)

	assert.Contains(t, failure.Reason, "artifact store")
	assert.True(t, failure.Permanent())
}
