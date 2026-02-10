package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"core"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockScheduler 模拟调度器
type MockScheduler struct {
	mock.Mock
}

func (m *MockScheduler) Schedule(job *core.Job) error {
	args := m.Called(job)
	return args.Error(0)
}

func (m *MockScheduler) Cancel(id string) error {
	args := m.Called(id)
	return args.Error(0)
}

func (m *MockScheduler) HeapLen() int {
	return m.Called().Int(0)
}

type APITestSuite struct {
	router    *gin.Engine
	mockSched *MockScheduler
	server    *Server
}

func (s *APITestSuite) SetupTest() {
	gin.SetMode(gin.TestMode)
	s.mockSched = new(MockScheduler)
	s.server = NewServer(s.mockSched, nil, "8080")
	s.router = s.server.engine
}

func (s *APITestSuite) TestCreateJob() {
	s.mockSched.On("Schedule", mock.AnythingOfType("*core.Job")).Return(nil)
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

	s.mockSched.AssertExpectations(s.T())
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
	s.mockSched.On("Cancel", "job-123").Return(nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/jobs/job-123", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(s.T(), 204, w.Code)
	s.mockSched.AssertCalled(s.T(), "Cancel", "job-123")
}

func (s *APITestSuite) TestCancelJobNotFound() {
	s.mockSched.On("Cancel", "missing").Return(core.ErrJobNotFound)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/jobs/missing", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(s.T(), 404, w.Code)
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
