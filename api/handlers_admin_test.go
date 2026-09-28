package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

func decodeRuntime(t *testing.T, recorder *httptest.ResponseRecorder) RuntimeResponse {
	t.Helper()

	var resp RuntimeResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	return resp
}

func TestAdminRuntimeReportsOccupancy(t *testing.T) {
	srv := newSecurityServer(t, Security{})
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "rt-1", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour),
	}))

	resp := decodeRuntime(t, doGet(t, srv, "/api/v1/admin/runtime", nil))
	assert.False(t, resp.Scheduler.Started, "测试里没启动调度循环，不该谎报在跑")
	assert.Equal(t, core.DefaultConcurrency, resp.Scheduler.Workers)
	assert.Equal(t, core.DefaultConcurrency, resp.Scheduler.QueueCapacity,
		"未配置队列容量时与 worker 数相等")
	assert.Equal(t, 1, resp.Scheduler.HeapSize)
	assert.NotEmpty(t, resp.Uptime)
	assert.Equal(t, historyGlobalLimit, resp.EventHistory.GlobalCapacity)
	assert.Empty(t, resp.SchedulingSuspendedNote)

	require.Equal(t, http.StatusOK,
		doJSON(t, srv, http.MethodPost, "/api/v1/admin/scheduler/suspend", "", nil).Code)

	suspended := decodeRuntime(t, doGet(t, srv, "/api/v1/admin/runtime", nil))
	assert.True(t, suspended.Scheduler.Suspended)
	assert.Contains(t, suspended.SchedulingSuspendedNote, "restart clears it",
		"挂起状态要自带一句解释，否则运维会当成服务卡死")

	require.Equal(t, http.StatusOK,
		doJSON(t, srv, http.MethodPost, "/api/v1/admin/scheduler/unsuspend", "", nil).Code)
	assert.False(t, decodeRuntime(t, doGet(t, srv, "/api/v1/admin/runtime", nil)).Scheduler.Suspended)
}

// 调度总开关是进程内的：这里只确认端点确实翻了标志位，
// 重启即解除的语义由 core 的 Start 负责（core/suspend_test.go 已覆盖）。
func TestSchedulerSuspendSwitchIsIdempotent(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	for i := 0; i < 2; i++ {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/admin/scheduler/suspend", "", nil)
		require.Equal(t, http.StatusOK, recorder.Code)
		assert.Contains(t, recorder.Body.String(), `"suspended":true`)
	}

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/admin/scheduler/unsuspend", "", nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"suspended":false`)
}

func TestClearEventHistoryReportsCount(t *testing.T) {
	srv := newSecurityServer(t, Security{})
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "clr-1", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour),
	}))
	waitForRecordedEvents(t, srv, "clr-1", 1)

	recorder := doJSON(t, srv, http.MethodDelete, "/api/v1/admin/events", "", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body map[string]int
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, 1, body["cleared"])
	assert.Empty(t, srv.history.Events("clr-1", 0))
}

// 运维端点暴露的是进程内部占用，档位要求最高；
// admin 也不能用——这是决策里"再加一档 ops"的全部意义。
func TestAdminEndpointsRequireOpsRole(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))

	targets := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/runtime"},
		{http.MethodPost, "/api/v1/admin/scheduler/suspend"},
		{http.MethodPost, "/api/v1/admin/scheduler/unsuspend"},
		{http.MethodDelete, "/api/v1/admin/events"},
	}

	sessions := map[string]TokenSessionResponse{
		testViewerName:   login(t, srv, testViewerName, testPassword),
		testOperatorName: login(t, srv, testOperatorName, testPassword),
		testAdminName:    login(t, srv, testAdminName, testPassword),
	}

	for name, session := range sessions {
		for _, target := range targets {
			recorder := doJSON(t, srv, target.method, target.path, "", bearer(session.AccessToken))
			assert.Equal(t, http.StatusForbidden, recorder.Code,
				"%s 调 %s %s 应当被拒", name, target.method, target.path)
		}
	}

	ops := login(t, srv, testOpsName, testPassword)
	for _, target := range targets {
		recorder := doJSON(t, srv, target.method, target.path, "", bearer(ops.AccessToken))
		assert.Equal(t, http.StatusOK, recorder.Code,
			"ops 调 %s %s 应当放行", target.method, target.path)
	}

	// 未认证连"这是什么端点"都不该知道
	assert.Equal(t, http.StatusUnauthorized, doGet(t, srv, "/api/v1/admin/runtime", nil).Code)
}
