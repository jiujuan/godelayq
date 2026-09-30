package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// fixedEventTime 是替身读取方给出事件时用的基准时间：库路径的用例不比真实时钟，
// 只比还原出来的字段。
var fixedEventTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

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

// fakeEventReader 是 eventReader 的替身：它只回答"读到些什么"和"收到了哪个 limit"，
// 用来把两个端点的库路径与错误路径固定下来。真读库在 store/sqlite 那一侧有用例。
type fakeEventReader struct {
	eventsByJob map[string][]core.Event
	recent      []core.Event
	err         error

	eventsCalls int
	recentCalls int
	seenJobID   string
	seenLimit   int
}

func (f *fakeEventReader) Events(jobID string, limit int) ([]core.Event, error) {
	f.eventsCalls++
	f.seenJobID, f.seenLimit = jobID, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.eventsByJob[jobID], nil
}

func (f *fakeEventReader) Recent(limit int) ([]core.Event, error) {
	f.recentCalls++
	f.seenLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.recent, nil
}

// persistedEvent fixture：库路径返回的两条事件，字段齐全，用于对照 JSON 形状。
func persistedEvents(jobID string) []core.Event {
	return []core.Event{
		{Type: core.EventJobScheduled, JobID: jobID, JobName: "payment_check",
			Status: core.StatusPending, Timestamp: fixedEventTime},
		{Type: core.EventJobCompleted, JobID: jobID, JobName: "payment_check",
			Status: core.StatusSuccess, Timestamp: fixedEventTime.Add(time.Second),
			Metadata: map[string]interface{}{"duration_ms": 2.0}},
	}
}

// TestGetJobEvents_MemoryPathUnchanged 守住"没装配事件库时行为一字不变"（本卡 DoD 第 1 条）：
// 响应文本、Note 原句、条数、升序都按 S03 之前的样子给。
func TestGetJobEvents_MemoryPathUnchanged(t *testing.T) {
	srv := newSecurityServer(t, Security{})
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "mem-1", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour),
	}))
	waitForRecordedEvents(t, srv, "mem-1", 1)

	recorder := doGet(t, srv, "/api/v1/jobs/mem-1/events", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	resp := decodeEvents(t, recorder)
	assert.Equal(t, "in-memory buffer, cleared on restart", resp.Note)
	assert.Equal(t, "mem-1", resp.JobID)
	assert.Equal(t, 1, resp.Count)
	assert.Equal(t, core.EventJobScheduled, resp.Items[0].Type)

	// 文本级对照：这条路径的 JSON 里不该出现库的句子
	body := recorder.Body.String()
	assert.Contains(t, body, `"note":"in-memory buffer, cleared on restart"`)
	assert.NotContains(t, body, "persisted")
}

// TestGetJobEvents_DBPath 注入替身后读的是库：条目来自替身，Note 换成持久化那句。
func TestGetJobEvents_DBPath(t *testing.T) {
	reader := &fakeEventReader{eventsByJob: map[string][]core.Event{"db-1": persistedEvents("db-1")}}
	srv := newSecurityServer(t, Security{}, WithEventLog(reader))

	recorder := doGet(t, srv, "/api/v1/jobs/db-1/events", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	resp := decodeEvents(t, recorder)
	assert.Equal(t, "db-1", resp.JobID)
	assert.Equal(t, 2, resp.Count)
	require.Len(t, resp.Items, 2)
	assert.Equal(t, core.EventJobScheduled, resp.Items[0].Type)
	assert.Equal(t, core.EventJobCompleted, resp.Items[1].Type)
	assert.Equal(t, "persisted event store; newest entry may lag by the write flush interval", resp.Note)
	assert.Equal(t, 1, reader.eventsCalls)
	assert.Equal(t, "db-1", reader.seenJobID)
	// limit 缺省时给的是库路径自己的上界，不是内存窗口的 100
	assert.Equal(t, eventsQueryLimit, reader.seenLimit)
}

// TestEventLimitCapacityDiffersByPath 固定两条路径的 limit 上界不同（本卡 §3.5）：
// 库路径把请求值原样带到 1000 封顶，内存路径仍按窗口容量压制。
// 内存路径的压制在响应上看不出来（任务本来就只有几条），所以直接对 parseEventLimit 断言。
func TestEventLimitCapacityDiffersByPath(t *testing.T) {
	reader := &fakeEventReader{eventsByJob: map[string][]core.Event{"db-1": persistedEvents("db-1")}}
	srv := newSecurityServer(t, Security{}, WithEventLog(reader))

	doGet(t, srv, "/api/v1/jobs/db-1/events?limit=500", nil)
	assert.Equal(t, 500, reader.seenLimit, "库里能留 20 万条，500 不该被压到 100")

	doGet(t, srv, "/api/v1/jobs/db-1/events?limit=5000", nil)
	assert.Equal(t, eventsQueryLimit, reader.seenLimit, "上界是单次响应的体积界限")

	assert.Equal(t, 100, parseEventLimit("500", historyPerJobLimit), "内存路径仍受窗口约束")
	assert.Equal(t, 500, parseEventLimit("500", historyGlobalLimit))
	assert.Equal(t, historyPerJobLimit, parseEventLimit("", historyPerJobLimit))
	assert.Equal(t, historyPerJobLimit, parseEventLimit("-1", historyPerJobLimit))
}

// TestGetJobEvents_DBReadErrorIs500 库读失败要报错，不能静默退回内存：
// 两条路径给的数据范围不同，拿一份缺历史的时间线做判断比拿一个 500 更糟。
func TestGetJobEvents_DBReadErrorIs500(t *testing.T) {
	reader := &fakeEventReader{err: errors.New("database disk image is malformed")}
	srv := newSecurityServer(t, Security{}, WithEventLog(reader))

	// 内存缓冲里放一条真事件：500 的响应里不该把它带出去
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "db-err", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour),
	}))
	waitForRecordedEvents(t, srv, "db-err", 1)

	recorder := doGet(t, srv, "/api/v1/jobs/db-err/events", nil)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.Equal(t, 500, resp.Code)
	assert.Equal(t, "failed to load job events", resp.Message)
	assert.Contains(t, resp.Details, "database disk image is malformed")
	assert.NotContains(t, recorder.Body.String(), "job.scheduled", "错误响应不得夹带内存路径的数据")
}

// TestListRecentEvents_BothPaths 全局端点的两条路径：Note 跟着分岔，
// 空结果仍是 200 且 items 是空数组（不是 null）。
func TestListRecentEvents_BothPaths(t *testing.T) {
	t.Run("内存", func(t *testing.T) {
		srv := newSecurityServer(t, Security{})
		require.NoError(t, srv.scheduler.Schedule(&core.Job{
			ID: "g-mem", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour),
		}))
		waitForRecordedEvents(t, srv, "g-mem", 1)

		recorder := doGet(t, srv, "/api/v1/events", nil)
		require.Equal(t, http.StatusOK, recorder.Code)
		resp := decodeEvents(t, recorder)
		assert.Equal(t, "in-memory buffer, cleared on restart", resp.Note)
		assert.Equal(t, 1, resp.Count)
	})

	t.Run("库", func(t *testing.T) {
		reader := &fakeEventReader{recent: persistedEvents("g-db")}
		srv := newSecurityServer(t, Security{}, WithEventLog(reader))

		recorder := doGet(t, srv, "/api/v1/events?limit=20", nil)
		require.Equal(t, http.StatusOK, recorder.Code)
		resp := decodeEvents(t, recorder)
		assert.Equal(t, "persisted event store; newest entry may lag by the write flush interval", resp.Note)
		assert.Equal(t, 2, resp.Count)
		assert.Empty(t, resp.JobID)
		assert.Equal(t, 20, reader.seenLimit)
	})

	t.Run("库空结果", func(t *testing.T) {
		reader := &fakeEventReader{}
		srv := newSecurityServer(t, Security{}, WithEventLog(reader))

		recorder := doGet(t, srv, "/api/v1/events", nil)
		require.Equal(t, http.StatusOK, recorder.Code)
		assert.Contains(t, recorder.Body.String(), `"items":[]`, "空列表必须是 []，不是 null")

		resp := decodeEvents(t, recorder)
		assert.Zero(t, resp.Count)
		assert.Empty(t, resp.Items)
	})

	t.Run("库读失败", func(t *testing.T) {
		reader := &fakeEventReader{err: errors.New("no such table: job_events")}
		srv := newSecurityServer(t, Security{}, WithEventLog(reader))

		recorder := doGet(t, srv, "/api/v1/events", nil)
		require.Equal(t, http.StatusInternalServerError, recorder.Code)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
		assert.Equal(t, "failed to load recent events", resp.Message)
	})
}

// TestWithEventLogNilKeepsMemoryPath 显式传 nil 与不注入等价：端点继续读内存缓冲。
func TestWithEventLogNilKeepsMemoryPath(t *testing.T) {
	var reader eventReader
	srv := newSecurityServer(t, Security{}, WithEventLog(reader))
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "nil-1", Name: "payment_check", TriggerAt: time.Now().Add(time.Hour),
	}))
	waitForRecordedEvents(t, srv, "nil-1", 1)

	resp := decodeEvents(t, doGet(t, srv, "/api/v1/jobs/nil-1/events", nil))
	assert.Equal(t, "in-memory buffer, cleared on restart", resp.Note)
	assert.Equal(t, 1, resp.Count)
}
