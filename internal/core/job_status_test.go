package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJobStatusNaming 状态名是 HTTP API 的过滤取值，序列化与解析必须互为逆运算。
func TestJobStatusNaming(t *testing.T) {
	for _, status := range []JobStatus{StatusPending, StatusRunning, StatusSuccess, StatusFailed, StatusCancelled} {
		parsed, ok := ParseJobStatus(status.String())
		require.True(t, ok, status.String())
		assert.Equal(t, status, parsed)

		// 大小写不敏感，手写查询参数更宽容
		mixed, ok := ParseJobStatus(mixedCase(status.String()))
		require.True(t, ok)
		assert.Equal(t, status, mixed)

		// 前后空白不容错，避免把拼写错误当成合法值
		_, ok = ParseJobStatus("  " + status.String() + "  ")
		assert.False(t, ok)
	}

	_, ok := ParseJobStatus("done")
	assert.False(t, ok)
	assert.Equal(t, "unknown", JobStatus(42).String())

	assert.True(t, StatusSuccess.IsTerminal())
	assert.True(t, StatusFailed.IsTerminal())
	assert.True(t, StatusCancelled.IsTerminal())
	assert.False(t, StatusPending.IsTerminal())
	assert.False(t, StatusRunning.IsTerminal())
}

func mixedCase(name string) string {
	if name == "" {
		return name
	}

	return string([]byte{name[0] - 32}) + name[1:]
}
