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

func TestClassifyExit(t *testing.T) {
	profile := &Profile{Name: "etl", RetryOnExit: []int{75, 86}}

	assert.False(t, classifyExit(profile, 0), "0 是成功，不该出现在失败判定里；即使 retry_on_exit 写了 0 也不算重试")
	assert.False(t, classifyExit(profile, 1))
	assert.True(t, classifyExit(profile, 75))
	assert.True(t, classifyExit(profile, 86))

	bare := &Profile{Name: "etl"}
	assert.False(t, classifyExit(bare, 75), "档位没声明就是永久失败")
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
