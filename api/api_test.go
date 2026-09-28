package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"godelayq/core"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// APITestSuite 使用真实的 core.Scheduler 与临时 JSON 存储，
// 覆盖 HTTP 处理器的请求/响应契约。
type APITestSuite struct {
	suite.Suite
	router    *gin.Engine
	scheduler *core.Scheduler
	server    *Server
}

func (s *APITestSuite) SetupTest() {
	gin.SetMode(gin.TestMode)

	store, err := core.NewJSONFileStore(filepath.Join(s.T().TempDir(), "jobs.json"))
	require.NoError(s.T(), err)

	s.scheduler = core.NewScheduler(store, nil, nil)
	s.server = NewServer(s.scheduler, store, "8080", Security{})
	s.router = s.server.engine
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

// TestUpdateJobEditsPendingJobInPlace 覆盖 #15：更新不再走 Cancel→Schedule，
// 任务 ID 与堆内条目保持不变。
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
	assert.True(s.T(), updatedJob.TriggerAt.Equal(newTrigger), "got %v", updatedJob.TriggerAt)

	// 没有"取消后重排"的空窗：堆里始终只有这一条
	assert.Equal(s.T(), 1, s.scheduler.HeapLen())

	missing := httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/api/v1/jobs/nope", bytes.NewBuffer(updateBody))
	req.Header.Set("Content-Type", "application/json")
	s.router.ServeHTTP(missing, req)
	assert.Equal(s.T(), 404, missing.Code)
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
