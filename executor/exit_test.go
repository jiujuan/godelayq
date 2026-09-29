package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExitError_Is(t *testing.T) {
	// 这两条判断是 core/scheduler.go 分类超时与取消的唯一入口：
	// Is 写错，任务会被记成"失败并消耗重试"而不是"超时"或"被打断"。
	cases := []struct {
		label        string
		err          *ExitError
		wantCancel   bool
		wantDeadline bool
	}{
		{"超时", &ExitError{TimedOut: true}, false, true},
		{"取消", &ExitError{Cancelled: true}, true, false},
		{"普通失败", &ExitError{ExitCode: 3}, false, false},
		{"两个标记同时为真按超时算", &ExitError{TimedOut: true, Cancelled: true}, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			assert.Equal(t, tc.wantCancel, errors.Is(tc.err, context.Canceled))
			assert.Equal(t, tc.wantDeadline, errors.Is(tc.err, context.DeadlineExceeded))
		})
	}

	// 包装之后仍然认得：调用方可能在 Handler 的返回值外面再包一层描述
	wrapped := &ExitError{Reason: "executor run failed", Detail: &ExitError{TimedOut: true}}
	assert.True(t, errors.Is(wrapped, context.DeadlineExceeded))
}

func TestExitError_PermanentComesFromRetryable(t *testing.T) {
	assert.True(t, (&ExitError{}).Permanent(), "零值必须是永久失败")
	assert.True(t, (&ExitError{ExitCode: 2}).Permanent())
	assert.False(t, (&ExitError{ExitCode: 2, Retryable: true}).Permanent())
	assert.False(t, (&ExitError{TimedOut: true, Retryable: true}).Permanent())
}

// TestExitError_UnwrapKeepsSentinelErrors 钉住 E08 的哨兵错误能穿过执行层的错误：
// 调用方要能区分"档位类型不对"和"参数值不对"。
func TestExitError_UnwrapKeepsSentinelErrors(t *testing.T) {
	failure := &ExitError{Profile: "nightly", Reason: "invalid submission", Detail: ErrWrongKind}

	assert.True(t, errors.Is(failure, ErrWrongKind))
	assert.Contains(t, failure.Error(), `profile "nightly"`)
	assert.Contains(t, failure.Error(), "invalid submission")
}

func TestExitError_MessageShape(t *testing.T) {
	assert.Equal(t, "profile: command failed", (&ExitError{}).Error())
	assert.Equal(t, "profile \"etl\": timed out", (&ExitError{Profile: "etl", Reason: "timed out"}).Error())
	assert.Equal(t, "profile: boom", (&ExitError{Detail: errors.New("boom")}).Error())

	// Detail 与 Reason 同时存在时拼成一句，两条信息都不能丢
	assert.Equal(t, "profile: cannot start the process: no such file",
		(&ExitError{Reason: "cannot start the process", Detail: errors.New("no such file")}).Error())
}

// TestClassifyExit 把卡片 §3.5 那张表原样写成用例：八行全覆盖。
// 这一张表是执行器重试策略的规范位置，改任何一行都要同时改卡片与这里。
func TestClassifyExit(t *testing.T) {
	cases := []struct {
		label       string
		class       failureClass
		exitCode    int
		retryOnExit []int
		wantRetry   bool
	}{
		{"提交内容非法（参数越界、payload 结构错）", failureInvalidSubmission, 0, nil, false},
		{"档位不可用（探测失败、程序找不到）", failureProfileUnavailable, 0, nil, false},
		{"权限不足（脚本不可执行、目录不可写）", failureNotAllowed, 0, nil, false},
		{"被取消或优雅关闭打断", failureInterrupted, 1, []int{1}, false},
		{"超时", failureTimeout, 0, nil, true},
		{"退出码在 retry_on_exit 里", failureExitCode, 75, []int{75, 86}, true},
		{"并发许可等待超时", failurePermitWait, 0, nil, true},
		{"其它非 0 退出码", failureExitCode, 1, []int{75, 86}, false},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			assert.Equal(t, tc.wantRetry, classifyExit(tc.class, tc.exitCode, tc.retryOnExit))
		})
	}

	// 取消那一行返回 false 不是"重试没有意义"的判决，而是"这里不给判决"：
	// 调度器靠 errors.Is(err, context.Canceled) 先走中断分支，根本不读这个标记。
	// 这条依赖由 TestExitError_Is 与 TestClassifyFailure_InterruptedWinsOverExitCode 分别守住。
	assert.True(t, errors.Is(newFailure(&Profile{Name: "etl"}, failureInterrupted, 0, "cancelled", nil), context.Canceled))

	// retry_on_exit 里写 0 不会把成功变成"该重试的失败"：0 是成功，走不到失败判定。
	assert.False(t, classifyExit(failureExitCode, 0, []int{0}))
	assert.False(t, classifyExit(failureExitCode, 99, nil), "档位没声明就是永久失败")
}

// TestNewFailureMarksFlagsByClass 钉住"标记由类别出"这条约定（TASK-E12 §3.5）：
// 构造点不再手写 TimedOut/Cancelled/Retryable，读错一个布尔就会改到重试行为。
func TestNewFailureMarksFlagsByClass(t *testing.T) {
	profile := &Profile{Name: "etl", RetryOnExit: []int{75}}

	timeout := newFailure(profile, failureTimeout, 0, "timed out", nil)
	assert.True(t, timeout.TimedOut)
	assert.False(t, timeout.Cancelled)
	assert.False(t, timeout.Permanent(), "超时可重试")

	permit := newFailure(profile, failurePermitWait, 0, "concurrency limit", nil)
	assert.True(t, permit.TimedOut, "等不到许可按超时归类，事件与告警口径一致")
	assert.False(t, permit.Permanent())

	interrupted := newFailure(profile, failureInterrupted, 0, "cancelled", nil)
	assert.True(t, interrupted.Cancelled)
	assert.False(t, interrupted.TimedOut)

	invalid := newFailure(profile, failureInvalidSubmission, 0, "invalid submission", nil)
	assert.True(t, invalid.Permanent(), "重跑用的还是同一份 payload，结论不会变")
	assert.False(t, invalid.TimedOut, "永久失败不该被误标成超时")

	startFailed := newFailure(profile, failureProfileUnavailable, 0, "cannot start the process", nil)
	assert.Zero(t, startFailed.ExitCode, "没跑起来的进程没有退出码")
	assert.True(t, startFailed.Permanent())
}

// TestRetryOnExit_Configurable 走真实退出码：档位的 retry_on_exit 决定同一条命令的两次失败结论。
func TestRetryOnExit_Configurable(t *testing.T) {
	status75, state75 := realExitStatus(t, 75)
	status1, state1 := realExitStatus(t, 1)

	declared := &Profile{Name: "etl", RetryOnExit: []int{75}}
	assert.False(t, classifyFailure(declared, status75, state75, nil).Permanent(),
		"档位显式要重试这个退出码")
	assert.True(t, classifyFailure(declared, status1, state1, nil).Permanent(),
		"没声明的退出码默认不重试")

	bare := &Profile{Name: "etl"}
	assert.True(t, classifyFailure(bare, status75, state75, nil).Permanent(),
		"档位没写 retry_on_exit 就一个都不重试")
}

// realExitStatus 真的起一个短进程，拿到 os/exec 的退出错误与进程状态。
// 分类逻辑要对着这些真实对象验，人造的 ProcessState 反而说明不了问题。
func realExitStatus(t *testing.T, code int) (*exec.ExitError, *os.ProcessState) {
	t.Helper()

	name, prefix := runnerShell(t)
	body := "exit " + strconv.Itoa(code)
	if runtime.GOOS == "windows" {
		body = "exit /b " + strconv.Itoa(code)
	}

	cmd := exec.Command(name, append(append([]string{}, prefix...), body)...)
	err := cmd.Run()

	var status *exec.ExitError
	require.True(t, errors.As(err, &status), "子进程以 %d 退出时应该拿到 *exec.ExitError", code)
	return status, cmd.ProcessState
}

func TestClassifyFailure_ExitStatus(t *testing.T) {
	status, state := realExitStatus(t, 4)
	profile := &Profile{Name: "etl", RetryOnExit: []int{4}}

	failure := classifyFailure(profile, status, state, nil)

	assert.Equal(t, 4, failure.ExitCode)
	assert.True(t, failure.Retryable)
	assert.False(t, failure.TimedOut)
	assert.False(t, failure.Cancelled)
	assert.Empty(t, failure.Reason, "退出码本身就是结论，不需要再编一个类别")
	assert.Equal(t, status, failure.Detail)
}

func TestClassifyFailure_InterruptedWinsOverExitCode(t *testing.T) {
	status, state := realExitStatus(t, 1)

	// 进程是被我们杀掉的：它的退出码是 killing 的结果，不能按退出码决定重试
	failure := classifyFailure(&Profile{Name: "etl", RetryOnExit: []int{1}}, status, state, context.DeadlineExceeded)

	assert.True(t, failure.TimedOut)
	assert.True(t, failure.Retryable)
	assert.Equal(t, "timed out", failure.Reason)

	cancelled := classifyFailure(&Profile{Name: "etl"}, status, state, context.Canceled)
	assert.True(t, cancelled.Cancelled)
	assert.False(t, cancelled.TimedOut)
	assert.Equal(t, "cancelled", cancelled.Reason)
	assert.False(t, cancelled.Retryable, "取消不该被读成一次可重试的失败")
}

func TestClassifyFailure_StartFailureKeepsExitCodeEmpty(t *testing.T) {
	failure := classifyFailure(&Profile{Name: "etl"}, exec.ErrNotFound, nil, nil)

	assert.Equal(t, "cannot start the process", failure.Reason)
	assert.Zero(t, failure.ExitCode, "没跑起来的进程没有退出码")
	assert.Empty(t, failure.Signal)
	assert.True(t, failure.Permanent())
}

func TestClassifyFailure_WaitDelay(t *testing.T) {
	// 进程已退出、输出管道被派生的进程占着：WaitDelay 强关管道后 os/exec 报 ErrWaitDelay
	failure := classifyFailure(&Profile{Name: "etl"}, exec.ErrWaitDelay, nil, nil)

	assert.Equal(t, "output stayed open past the wait limit", failure.Reason)
	assert.True(t, failure.Permanent(), "重跑会把脚本的副作用再做一遍，而问题只在输出没收全")
}

func TestClassifyFailure_SuccessReturnsNil(t *testing.T) {
	assert.Nil(t, classifyFailure(&Profile{Name: "etl"}, nil, nil, nil))

	// 上下文已结束但进程自己跑完了：结论以进程为准，不额外制造失败
	assert.Nil(t, classifyFailure(&Profile{Name: "etl"}, nil, nil, context.DeadlineExceeded))
}
