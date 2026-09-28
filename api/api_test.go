package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"godelayq/core"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// newTestLogger 丢弃日志输出，只保留 error，避免访问日志刷屏
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// APITestSuite 使用真实的 core.Scheduler 与临时 JSON 存储，
// 覆盖 HTTP 处理器的请求/响应契约。
type APITestSuite struct {
	suite.Suite
	router    *gin.Engine
	scheduler *core.Scheduler
	server    *Server
	store     core.Store
}

func (s *APITestSuite) SetupTest() {
	gin.SetMode(gin.TestMode)

	store, err := core.NewJSONFileStore(filepath.Join(s.T().TempDir(), "jobs.json"))
	require.NoError(s.T(), err)

	s.store = store
	s.scheduler = core.NewScheduler(store, nil, nil)
	s.server = NewServer(s.scheduler, store, "8080", Security{}, newTestLogger())
	s.router = s.server.engine
}

// TearDownTest 关闭存储：否则后台合并协程会在临时目录删除后持续报错
func (s *APITestSuite) TearDownTest() {
	require.NoError(s.T(), s.store.Close())
}

func (s *APITestSuite) TestCreateJob() {
	s.server.RegisterJobHandler("payment_check", func(ctx context.Context, job *core.Job) error {
		return nil
	})

	body := CreateJobRequest{
		Name:       "payment_check",
		Delay:      "10m",
		Payload:    json.RawMessage(`{"order_id":"123"}`),
		MaxRetries: 3,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/jobs", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	assert.Equal(s.T(), 201, w.Code)

	var resp JobResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(s.T(), err)
	assert.Equal(s.T(), "payment_check", resp.Name)
	assert.NotEmpty(s.T(), resp.ID)
	assert.Equal(s.T(), 1, s.scheduler.HeapLen())
}

func (s *APITestSuite) TestCreateJobUnknownType() {
	body := CreateJobRequest{
		Name: "unknown_type",
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/jobs", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	assert.Equal(s.T(), 400, w.Code)
}

func (s *APITestSuite) TestCancelJob() {
	job := &core.Job{
		ID:        "job-123",
		Name:      "payment_check",
		TriggerAt: time.Now().Add(1 * time.Hour),
	}
	require.NoError(s.T(), s.scheduler.Schedule(job))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/jobs/job-123", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(s.T(), 204, w.Code)
	assert.Equal(s.T(), 0, s.scheduler.HeapLen())
}

func (s *APITestSuite) TestCancelJobNotFound() {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/jobs/missing", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(s.T(), 404, w.Code)
}

func (s *APITestSuite) TestGetStatsReportsHeapSize() {
	for i := 0; i < 3; i++ {
		require.NoError(s.T(), s.scheduler.Schedule(&core.Job{
			Name:      "stats_job",
			TriggerAt: time.Now().Add(time.Hour),
		}))
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stats", nil)
	s.router.ServeHTTP(w, req)

	require.Equal(s.T(), 200, w.Code)

	var stats StatsResponse
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &stats))
	assert.Equal(s.T(), 3, stats.HeapSize, "heap_size must reflect the live scheduling queue")
	assert.Equal(s.T(), 3, stats.Pending)
}

// listJobs 调用 GET /jobs 并解析响应
func (s *APITestSuite) listJobs(query string) (int, ListJobsResponse) {
	s.T().Helper()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/jobs"+query, nil)
	s.router.ServeHTTP(w, req)

	var resp ListJobsResponse
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))

	return w.Code, resp
}

func (s *APITestSuite) seedSnapshots(snapshots ...core.JobSnapshot) {
	s.T().Helper()

	for _, snap := range snapshots {
		require.NoError(s.T(), s.server.store.Update(snap))
	}
}

// TestListJobsFiltersByStatusName 覆盖 #12：终态记录可查，过滤按状态名而非数字。
func (s *APITestSuite) TestListJobsFiltersByStatusName() {
	now := time.Now()
	s.seedSnapshots(
		core.JobSnapshot{ID: "p1", Name: "nightly", TriggerAt: now.Add(time.Hour), Status: int(core.StatusPending), UpdatedAt: now},
		core.JobSnapshot{ID: "s1", Name: "nightly", TriggerAt: now, Status: int(core.StatusSuccess), UpdatedAt: now.Add(time.Minute)},
		core.JobSnapshot{ID: "f1", Name: "nightly", TriggerAt: now, Status: int(core.StatusFailed), UpdatedAt: now.Add(2 * time.Minute)},
	)

	code, all := s.listJobs("")
	require.Equal(s.T(), 200, code)
	require.Equal(s.T(), 3, all.Total)
	// 存储是 map，必须按更新时间稳定排序，否则带 limit 时结果会漂移
	assert.Equal(s.T(), []string{"f1", "s1", "p1"}, []string{all.Items[0].ID, all.Items[1].ID, all.Items[2].ID})

	for _, tc := range []struct {
		query      string
		wantID     string
		wantStatus string
	}{
		{"?status=success", "s1", "success"},
		{"?status=failed", "f1", "failed"},
		{"?status=pending", "p1", "pending"},
		{"?status=SUCCESS", "s1", "success"},
	} {
		code, filtered := s.listJobs(tc.query)
		require.Equal(s.T(), 200, code, tc.query)
		require.Len(s.T(), filtered.Items, 1, tc.query)
		assert.Equal(s.T(), tc.wantID, filtered.Items[0].ID)
		assert.Equal(s.T(), tc.wantStatus, filtered.Items[0].Status, "响应里的状态必须是快照真实状态")
		assert.Equal(s.T(), 1, filtered.Total)
	}

	code, bad := s.listJobs("?status=3")
	assert.Equal(s.T(), 400, code, "数字状态码不再是合法的过滤值")
	assert.Empty(s.T(), bad.Items)

	code, limited := s.listJobs("?limit=1")
	require.Equal(s.T(), 200, code)
	assert.Equal(s.T(), 3, limited.Total, "total 是匹配总数，不受 limit 截断影响")
	assert.Len(s.T(), limited.Items, 1)

	code, secondPage := s.listJobs("?limit=1&offset=1")
	require.Equal(s.T(), 200, code)
	require.Len(s.T(), secondPage.Items, 1)
	assert.Equal(s.T(), "s1", secondPage.Items[0].ID, "offset 必须真实生效，否则翻页会重复读同一页")

	code, beyondEnd := s.listJobs("?offset=99")
	require.Equal(s.T(), 200, code)
	assert.Equal(s.T(), 3, beyondEnd.Total)
	assert.Empty(s.T(), beyondEnd.Items)

	code, none := s.listJobs("?name=other")
	require.Equal(s.T(), 200, code)
	assert.Equal(s.T(), 0, none.Total)
}

// TestParseListJobsPaging 分页参数的容错口径。
func TestParseListJobsPaging(t *testing.T) {
	assert.Equal(t, defaultListJobsLimit, parseListJobsLimit(""))
	assert.Equal(t, defaultListJobsLimit, parseListJobsLimit("abc"))
	assert.Equal(t, defaultListJobsLimit, parseListJobsLimit("0"))
	assert.Equal(t, defaultListJobsLimit, parseListJobsLimit("-5"))
	assert.Equal(t, 20, parseListJobsLimit("20"))
	assert.Equal(t, maxListJobsLimit, parseListJobsLimit("100000"), "超过硬上限要截断而不是把整个存储吐出去")

	assert.Equal(t, 0, parseListJobsOffset(""))
	assert.Equal(t, 0, parseListJobsOffset("abc"))
	assert.Equal(t, 0, parseListJobsOffset("-1"))
	assert.Equal(t, 40, parseListJobsOffset("40"))
}

// TestGetStatsCountsTerminalAndLiveRunning 覆盖 #12：completed/failed 不再恒 0，
// running 取实时执行数。
func (s *APITestSuite) TestGetStatsCountsTerminalAndLiveRunning() {
	now := time.Now()
	s.seedSnapshots(
		core.JobSnapshot{ID: "done", Name: "nightly", TriggerAt: now, Status: int(core.StatusSuccess), UpdatedAt: now},
		core.JobSnapshot{ID: "broken", Name: "nightly", TriggerAt: now, Status: int(core.StatusFailed), UpdatedAt: now},
	)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stats", nil)
	s.router.ServeHTTP(w, req)

	var stats StatsResponse
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &stats))
	assert.Equal(s.T(), 1, stats.Completed)
	assert.Equal(s.T(), 1, stats.Failed)
	assert.Equal(s.T(), 0, stats.Running)

	running := make(chan int, 1)
	s.server.RegisterJobHandler("nightly", func(ctx context.Context, job *core.Job) error {
		running <- 1
		<-ctx.Done()
		return ctx.Err()
	})
	require.NoError(s.T(), s.scheduler.Schedule(&core.Job{
		ID: "in-flight", Name: "nightly", TriggerAt: time.Now().Add(-time.Second),
	}))

	s.scheduler.Start()
	defer s.scheduler.Stop()

	<-running
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/stats", nil)
	s.router.ServeHTTP(w, req)
	statsWhileRunning := StatsResponse{}
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &statsWhileRunning))
	assert.Equal(s.T(), 1, statsWhileRunning.Running, "running 必须反映正在执行的任务")
}

// TestRetryJobSeesFailedSnapshot 记录失败任务此前被 LoadAll 过滤掉，
// 手动重试端点因此永远找不到目标。
func (s *APITestSuite) TestRetryJobSeesFailedSnapshot() {
	now := time.Now()
	s.seedSnapshots(core.JobSnapshot{
		ID: "retry-me", Name: "nightly", TriggerAt: now.Add(-time.Hour),
		Status: int(core.StatusFailed), UpdatedAt: now,
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/jobs/retry-me/retry", nil)
	s.router.ServeHTTP(w, req)

	require.Equal(s.T(), 200, w.Code)
	var resp JobResponse
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(s.T(), "retry-me", resp.ID)
	assert.Equal(s.T(), "pending", resp.Status)
	assert.Equal(s.T(), 1, s.scheduler.HeapLen())
}

// TestJobResponseSurfacesTimeout 响应里要能看到执行超时配置。
func (s *APITestSuite) TestJobResponseSurfacesTimeout() {
	s.server.RegisterJobHandler("payment_check", func(ctx context.Context, job *core.Job) error {
		return nil
	})

	body, _ := json.Marshal(CreateJobRequest{Name: "payment_check", Delay: "10m", Timeout: "45s"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/jobs", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	require.Equal(s.T(), 201, w.Code)
	var created JobResponse
	require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &created))
	assert.Equal(s.T(), "45s", created.Timeout)

	code, listed := s.listJobs("?status=pending")
	require.Equal(s.T(), 200, code)
	require.Len(s.T(), listed.Items, 1)
	assert.Equal(s.T(), "45s", listed.Items[0].Timeout)
}

func (s *APITestSuite) TestUpdateJobEditsPendingJobInPlace() {
	s.server.RegisterJobHandler("payment_check", func(ctx context.Context, job *core.Job) error {
		return nil
	})

	created := httptest.NewRecorder()
	body, _ := json.Marshal(CreateJobRequest{
		Name:    "payment_check",
		Delay:   "2h",
		Payload: json.RawMessage(`{"order":"old"}`),
	})
	req, _ := http.NewRequest("POST", "/api/v1/jobs", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(created, req)
	require.Equal(s.T(), 201, created.Code)

	var createdJob JobResponse
	require.NoError(s.T(), json.Unmarshal(created.Body.Bytes(), &createdJob))

	newTrigger := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Second)
	updateBody, _ := json.Marshal(UpdateJobRequest{
		TriggerAt: &newTrigger,
		Payload:   json.RawMessage(`{"order":"new"}`),
		Timeout:   "45s",
	})
	updated := httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/api/v1/jobs/"+createdJob.ID, bytes.NewBuffer(updateBody))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(updated, req)

	require.Equal(s.T(), 200, updated.Code)
	var updatedJob JobResponse
	require.NoError(s.T(), json.Unmarshal(updated.Body.Bytes(), &updatedJob))
	assert.Equal(s.T(), createdJob.ID, updatedJob.ID, "an in-place update must keep the job id")
	assert.Equal(s.T(), `{"order":"new"}`, string(updatedJob.Payload))
	assert.Equal(s.T(), "45s", updatedJob.Timeout, "timeout 也应能被更新")
	assert.True(s.T(), updatedJob.TriggerAt.Equal(newTrigger), "got %v", updatedJob.TriggerAt)

	// 没有"取消后重排"的空窗：堆里始终只有这一条
	assert.Equal(s.T(), 1, s.scheduler.HeapLen())

	missing := httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/api/v1/jobs/nope", bytes.NewBuffer(updateBody))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(missing, req)
	assert.Equal(s.T(), 404, missing.Code)
}

// postBatch 提交批量创建请求并解析响应
func (s *APITestSuite) postBatch(body any) (int, BatchCreateJobsResponse) {
	s.T().Helper()

	raw, err := json.Marshal(body)
	require.NoError(s.T(), err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/jobs/batch", bytes.NewBuffer(raw))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	var resp BatchCreateJobsResponse
	if w.Code == 207 {
		require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), &resp))
	}

	return w.Code, resp
}

func (s *APITestSuite) registerPaymentHandler() {
	s.T().Helper()

	s.server.RegisterJobHandler("payment_check", func(ctx context.Context, job *core.Job) error {
		return nil
	})
}

// TestBatchCreateAllSucceed 覆盖 #13：批量端点不再是空壳，条目逐条入队。
func (s *APITestSuite) TestBatchCreateAllSucceed() {
	s.registerPaymentHandler()

	code, resp := s.postBatch([]CreateJobRequest{
		{Name: "payment_check", Delay: "1m"},
		{Name: "payment_check", Delay: "2m", Timeout: "30s", MaxRetries: 2},
		{Name: "payment_check", TriggerAt: ptrTime(time.Now().Add(3 * time.Hour))},
	})

	require.Equal(s.T(), 207, code)
	assert.Equal(s.T(), 3, resp.Succeeded)
	assert.Zero(s.T(), resp.Failed)
	assert.Empty(s.T(), resp.Errors)
	assert.Len(s.T(), resp.Items, 3)
	assert.Equal(s.T(), "30s", resp.Items[1].Timeout)
	assert.Equal(s.T(), 2, resp.Items[1].MaxRetries)

	ids := make(map[string]struct{}, 3)
	for _, item := range resp.Items {
		require.Len(s.T(), item.ID, 36, "batch ids must be UUIDv7: %q", item.ID)
		ids[item.ID] = struct{}{}
	}
	assert.Len(s.T(), ids, 3, "each batch item needs its own id")
	assert.Equal(s.T(), 3, s.scheduler.HeapLen())
}

// TestBatchCreatePartialFailure 一条失败不影响其它条目，错误按下标返回。
func (s *APITestSuite) TestBatchCreatePartialFailure() {
	s.registerPaymentHandler()

	code, resp := s.postBatch([]CreateJobRequest{
		{Name: "payment_check", Delay: "1m"},
		{Name: "nope_not_registered", Delay: "1m"},
		{Name: "payment_check", Delay: "yesterday"},
		{Name: "payment_check", Timeout: "soon"},
		{Name: "payment_check", Delay: "5m"},
	})

	require.Equal(s.T(), 207, code)
	assert.Equal(s.T(), 2, resp.Succeeded)
	assert.Equal(s.T(), 3, resp.Failed)
	assert.Len(s.T(), resp.Errors, 3)

	indexes := make([]int, 0, len(resp.Errors))
	for _, failure := range resp.Errors {
		indexes = append(indexes, failure.Index)
		assert.NotEmpty(s.T(), failure.Message)
		assert.NotEmpty(s.T(), failure.Details)
	}
	assert.Equal(s.T(), []int{1, 2, 3}, indexes)
	assert.Equal(s.T(), "unknown job type", resp.Errors[0].Message)
	assert.Equal(s.T(), 400, resp.Errors[0].Code)
	assert.Equal(s.T(), 2, s.scheduler.HeapLen(), "只有校验通过的条目会进入调度堆")
}

func (s *APITestSuite) TestBatchCreateRejectsEmptyAndOversized() {
	s.registerPaymentHandler()

	code, _ := s.postBatch([]CreateJobRequest{})
	assert.Equal(s.T(), 400, code)

	tooMany := make([]CreateJobRequest, maxBatchCreateSize+1)
	for i := range tooMany {
		tooMany[i] = CreateJobRequest{Name: "payment_check", Delay: "10m"}
	}
	code, _ = s.postBatch(tooMany)
	assert.Equal(s.T(), 400, code)

	// 上限本身可用
	code, resp := s.postBatch(tooMany[:maxBatchCreateSize])
	require.Equal(s.T(), 207, code)
	assert.Equal(s.T(), maxBatchCreateSize, resp.Succeeded)
	assert.Zero(s.T(), resp.Failed)
}

func (s *APITestSuite) TestBatchCreateRejectsNonArrayBody() {
	s.registerPaymentHandler()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/jobs/batch", strings.NewReader(`{"name":"payment_check"}`))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, req)

	assert.Equal(s.T(), 400, w.Code)
	assert.Contains(s.T(), w.Body.String(), "invalid batch format")
}

func (s *APITestSuite) TestCalculateTriggerTime() {
	now := time.Now()

	// 测试Delay
	req := CreateJobRequest{Delay: "10m"}
	triggerAt, err := s.server.calculateTriggerTime(req)
	assert.NoError(s.T(), err)
	assert.True(s.T(), triggerAt.After(now.Add(9*time.Minute)))
	assert.True(s.T(), triggerAt.Before(now.Add(11*time.Minute)))

	// 测试绝对时间
	future := now.Add(1 * time.Hour)
	req = CreateJobRequest{TriggerAt: &future}
	triggerAt, err = s.server.calculateTriggerTime(req)
	assert.NoError(s.T(), err)
	assert.Equal(s.T(), future, triggerAt)

	// 测试无效Delay
	req = CreateJobRequest{Delay: "invalid"}
	_, err = s.server.calculateTriggerTime(req)
	assert.Error(s.T(), err)
}

func TestAPISuite(t *testing.T) {
	suite.Run(t, new(APITestSuite))
}

// ptrTime 便于在请求体里填 *time.Time 字段。
func ptrTime(value time.Time) *time.Time {
	return &value
}
