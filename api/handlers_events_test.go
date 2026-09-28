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

// waitForRecordedEvents 等事件历史的 drain 协程跟上：
// 记录器是异步订阅者，发布与可读之间没有同步点。
func waitForRecordedEvents(t *testing.T, srv *Server, jobID string, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(srv.history.Events(jobID, 0)) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("在 2s 内没有记录到 %d 条事件（job_id=%s）", want, jobID)
}

func decodeEvents(t *testing.T, recorder *httptest.ResponseRecorder) EventsResponse {
	t.Helper()

	var resp EventsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	return resp
}

func TestJobEventsEndpointServesTimeline(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "ev-1", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour),
	}))
	_, err := srv.scheduler.Pause("ev-1")
	require.NoError(t, err)
	waitForRecordedEvents(t, srv, "ev-1", 2)

	resp := decodeEvents(t, doGet(t, srv, "/api/v1/jobs/ev-1/events", nil))
	require.Equal(t, 2, resp.Count)
	require.Len(t, resp.Items, 2)
	assert.Equal(t, "ev-1", resp.JobID)
	assert.Equal(t, core.EventJobScheduled, resp.Items[0].Type, "时间线按时间升序")
	assert.Equal(t, core.EventJobPaused, resp.Items[1].Type)
	assert.Equal(t, int(core.StatusPaused), int(resp.Items[1].Status))
	assert.Contains(t, resp.Note, "in-memory", "响应里要说清这是内存缓冲")

	limited := decodeEvents(t, doGet(t, srv, "/api/v1/jobs/ev-1/events?limit=1", nil))
	require.Equal(t, 1, limited.Count)
	assert.Equal(t, core.EventJobPaused, limited.Items[0].Type, "limit 取最近的那几条")

	// 没有记录的任务返回空列表而不是 404：详情页时间线本来就可能还没事件
	empty := decodeEvents(t, doGet(t, srv, "/api/v1/jobs/no-such-job/events", nil))
	assert.Zero(t, empty.Count)
	assert.Empty(t, empty.Items)
}

func TestRecentEventsEndpointServesDashboardFeed(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	for _, id := range []string{"feed-1", "feed-2"} {
		require.NoError(t, srv.scheduler.Schedule(&core.Job{
			ID: id, Name: "payment_check", TriggerAt: time.Now().Add(time.Hour),
		}))
	}
	waitForRecordedEvents(t, srv, "feed-2", 1)

	resp := decodeEvents(t, doGet(t, srv, "/api/v1/events", nil))
	assert.Equal(t, 2, resp.Count)
	assert.Empty(t, resp.JobID, "全局流没有单一归属")

	limited := decodeEvents(t, doGet(t, srv, "/api/v1/events?limit=1", nil))
	assert.Equal(t, 1, limited.Count)
}

func TestEventEndpointsAreReadLevel(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "ev-rbac", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour),
	}))
	waitForRecordedEvents(t, srv, "ev-rbac", 1)

	viewer := login(t, srv, testViewerName, testPassword)
	assert.Equal(t, http.StatusOK,
		doGet(t, srv, "/api/v1/jobs/ev-rbac/events", bearer(viewer.AccessToken)).Code)
	assert.Equal(t, http.StatusOK,
		doGet(t, srv, "/api/v1/events", bearer(viewer.AccessToken)).Code)
}
