package api

import (
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/require"
)

// attempts_endpoint_test.go 是 GET /jobs/:id/attempts 的形状测试（TASK-RE02）：
// 账本能力缺失返回降级形状（ledger:false），注入读取方后按 AttemptResponse 形状出数。
// 真实账本的数据正确性在 store/jobs 双后端测试里；调度器挂点在 core/attempts_test.go 里。

// fakeAttemptReader 是 attemptReader 的替身：按 seq 原样返回预置行。
type fakeAttemptReader struct{ records []core.Attempt }

func (f fakeAttemptReader) Attempts(jobID string, limit int) ([]core.Attempt, error) {
	if limit > len(f.records) {
		limit = len(f.records)
	}
	return f.records[:limit], nil
}

// TestGetJobAttempts_DegradesWithoutLedger：JSON 部署（SetupTest 不注入 reader）——
// 200 + ledger:false + 空数组（不是 null）+ 降级说明。
func (s *APITestSuite) TestGetJobAttempts_DegradesWithoutLedger() {
	var resp AttemptsResponse
	code := s.doJSON("GET", "/api/v1/jobs/whatever/attempts", nil, &resp)
	require.Equal(s.T(), 200, code)
	require.False(s.T(), resp.Ledger)
	require.NotEmpty(s.T(), resp.Detail)
	require.NotNil(s.T(), resp.Items, "空账本要序列化成 []，不能是 null")
	require.Empty(s.T(), resp.Items)
}

// TestGetJobAttempts_ReturnsLedgerRows：库模式形状——Set 标记折成指针（null 与零值分得开）。
func (s *APITestSuite) TestGetJobAttempts_ReturnsLedgerRows() {
	finished := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s.server.attempts = fakeAttemptReader{records: []core.Attempt{{
		JobID: "j1", Seq: 2, Status: core.AttemptFailed, Trigger: core.AttemptTriggerRetry,
		StartedAt: finished.Add(-time.Second), FinishedAt: finished, DurationMs: 1000,
		Class: "exit_code", Retryable: true, RetryableSet: true,
		ExitCode: 7, ExitCodeSet: true,
	}}}

	var resp AttemptsResponse
	code := s.doJSON("GET", "/api/v1/jobs/j1/attempts", nil, &resp)
	require.Equal(s.T(), 200, code)
	require.True(s.T(), resp.Ledger)
	require.Empty(s.T(), resp.Detail)
	require.Len(s.T(), resp.Items, 1)

	item := resp.Items[0]
	require.Equal(s.T(), 2, item.Seq)
	require.Equal(s.T(), "failed", item.Status)
	require.Equal(s.T(), "retry", item.TriggerSource)
	require.Equal(s.T(), "exit_code", item.Class)
	require.NotNil(s.T(), item.Retryable)
	require.True(s.T(), *item.Retryable)
	require.NotNil(s.T(), item.ExitCode)
	require.Equal(s.T(), 7, *item.ExitCode)
	require.Nil(s.T(), item.HTTPStatus, "未判定字段必须是 null 而不是 0")
	require.NotNil(s.T(), item.FinishedAt)
}

// TestGetJobAttempts_RejectsBadLimit：limit 越界与非数字一律 400。
func (s *APITestSuite) TestGetJobAttempts_RejectsBadLimit() {
	s.server.attempts = fakeAttemptReader{}
	var resp ErrorResponse
	require.Equal(s.T(), 400, s.doJSON("GET", "/api/v1/jobs/j1/attempts?limit=0", nil, &resp))
	require.Equal(s.T(), 400, s.doJSON("GET", "/api/v1/jobs/j1/attempts?limit=101", nil, &resp))
	require.Equal(s.T(), 400, s.doJSON("GET", "/api/v1/jobs/j1/attempts?limit=abc", nil, &resp))
}
