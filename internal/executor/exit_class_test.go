package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/require"
)

// exit_class_test.go 钉 TASK-RE04 的分类透传：failureClass 的封闭词表、
// ExitError 的 Class/HTTPStatus 填充、AttemptOutcome 接口的读取语义。

// TestFailureClassString：十类词表与设计文档 §6.4 一一对应，全仓只有这一份映射。
func TestFailureClassString(t *testing.T) {
	cases := map[failureClass]string{
		failureInvalidSubmission:  "invalid_submission",
		failureProfileUnavailable: "profile_unavailable",
		failureNotAllowed:         "not_allowed",
		failureTimeout:            "timeout",
		failurePermitWait:         "permit_wait",
		failureInterrupted:        "interrupted",
		failureExitCode:           "exit_code",
		failureHTTPStatus:         "http_status",
		failureAddressRefused:     "address_refused",
		failureTransport:          "transport",
	}
	for class, want := range cases {
		require.Equal(t, want, class.String(), "类别 %d 的文本漂移", class)
	}
}

// TestExitErrorCarriesClassification：构造点集中填 Class/HTTPStatus，
// AttemptOutcome 的 ok 语义按类别分列（exit_code/http_status 互不冒充）。
func TestExitErrorCarriesClassification(t *testing.T) {
	p := &Profile{Name: "p"}

	// 进程类：退出码落在 exit_code 列，http_status 不给
	exitFailure := newFailure(p, failureExitCode, 7, "", errors.New("boom"))
	require.Equal(t, "exit_code", exitFailure.Class)
	code, ok := exitFailure.AttemptExitCode()
	require.True(t, ok)
	require.Equal(t, 7, code)
	_, ok = exitFailure.AttemptHTTPStatus()
	require.False(t, ok, "退出码类别不得给 http_status")
	retryable, ok := exitFailure.AttemptRetryable()
	require.True(t, ok)
	require.False(t, retryable, "未列 retry_on_exit 的退出码默认不重试")

	// 起不来的进程：exit code 0 也没有（class 不是 exit_code）
	startFailure := newFailure(p, failureProfileUnavailable, 0, "cannot start", errors.New("not found"))
	require.Equal(t, "profile_unavailable", startFailure.Class)
	_, ok = startFailure.AttemptExitCode()
	require.False(t, ok)

	// http 类：状态码落在 http_status 列，exit_code 不给
	statusFailure := newStatusFailure(p, failureHTTPStatus, 502, "", errors.New("bad gateway"))
	require.Equal(t, "http_status", statusFailure.Class)
	status, ok := statusFailure.AttemptHTTPStatus()
	require.True(t, ok)
	require.Equal(t, 502, status)
	_, ok = statusFailure.AttemptExitCode()
	require.False(t, ok, "http 类别不得给退出码")
	retryable, _ = statusFailure.AttemptRetryable()
	require.True(t, retryable, "5xx 可重试")

	// 超时与传输：可重试，两列都不给
	timeoutFailure := newFailure(p, failureTimeout, 0, "timed out", context.DeadlineExceeded)
	require.Equal(t, "timeout", timeoutFailure.Class)
	retryable, _ = timeoutFailure.AttemptRetryable()
	require.True(t, retryable)

	// 脱敏文本：AttemptErrorText 走 Error()（E08 口径在其内）
	require.Equal(t, exitFailure.Error(), exitFailure.AttemptErrorText())
}

// TestExitErrorSatisfiesAttemptOutcome：接口实现钉在编译期之外再跑一次真值，
// 防止方法被改成永远 ok=false 之类静默退化。
func TestExitErrorSatisfiesAttemptOutcome(t *testing.T) {
	var outcome core.AttemptOutcome = &ExitError{Class: "exit_code", ExitCode: 3, Retryable: true}
	require.Equal(t, "exit_code", outcome.AttemptClass())
	retryable, ok := outcome.AttemptRetryable()
	require.True(t, ok)
	require.True(t, retryable)
}
