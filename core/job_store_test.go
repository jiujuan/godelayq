package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type JobTestSuite struct {
	suite.Suite
}

func (s *JobTestSuite) TestJobCloneForRetry() {
	original := &Job{
		ID:         "job-1",
		Name:       "test-job",
		Payload:    []byte(`{"key":"value"}`),
		RetryCount: 1,
		MaxRetries: 3,
		RetryDelay: 1 * time.Minute,
		TriggerAt:  time.Now(),
	}

	nextTime := time.Now().Add(5 * time.Minute)
	retryJob := original.CloneForRetry(nextTime)

	assert.Contains(s.T(), retryJob.ID, "job-1_retry_")
	assert.Equal(s.T(), original.Name, retryJob.Name)
	assert.Equal(s.T(), original.Payload, retryJob.Payload)
	assert.Equal(s.T(), 2, retryJob.RetryCount)
	assert.Equal(s.T(), 2*time.Minute, retryJob.RetryDelay) // 指数退避
	assert.Equal(s.T(), StatusPending, retryJob.Status)
}

func (s *JobTestSuite) TestJobSnapshotRoundTrip() {
	original := &Job{
		ID:         "job-1",
		Name:       "test-job",
		Payload:    []byte(`{"data":123}`),
		TriggerAt:  time.Now().UTC(),
		CronExpr:   "*/5 * * * *",
		IsRepeat:   true,
		MaxRetries: 3,
		RetryCount: 1,
		RetryDelay: 30 * time.Second,
		Status:     StatusRunning,
		Attempts:   2,
		CreatedAt:  time.Now().UTC().Add(-1 * time.Hour),
		UpdatedAt:  time.Now().UTC(),
	}

	snapshot := original.ToSnapshot()

	// 验证快照字段
	assert.Equal(s.T(), original.ID, snapshot.ID)
	assert.Equal(s.T(), int64(original.RetryDelay), snapshot.RetryDelay)

	// 恢复测试
	restored := &Job{}
	restored.FromSnapshot(snapshot)

	assert.Equal(s.T(), original.ID, restored.ID)
	assert.Equal(s.T(), original.Name, restored.Name)
	assert.Equal(s.T(), original.Status, restored.Status)
	assert.Equal(s.T(), StatusPending, restored.Status) // 恢复后重置为pending
}

func TestJobSuite(t *testing.T) {
	suite.Run(t, new(JobTestSuite))
}

// Store 测试
type StoreTestSuite struct {
	suite.Suite
	tempDir   string
	store     *JSONFileStore
	storePath string
}

func (s *StoreTestSuite) SetupTest() {
	s.tempDir = s.T().TempDir()
	s.storePath = filepath.Join(s.tempDir, "test_jobs.json")
	var err error
	s.store, err = NewJSONFileStore(s.storePath)
	require.NoError(s.T(), err)
}

func (s *StoreTestSuite) TearDownTest() {
	os.RemoveAll(s.tempDir)
}

func (s *StoreTestSuite) TestSaveAndLoad() {
	job := &Job{
		ID:        "test-1",
		Name:      "payment",
		TriggerAt: time.Now(),
		Status:    StatusPending,
	}

	err := s.store.Save(job)
	assert.NoError(s.T(), err)

	// 验证文件存在
	_, err = os.Stat(s.storePath)
	assert.NoError(s.T(), err)

	// 重新加载
	loaded, err := s.store.LoadAll()
	assert.NoError(s.T(), err)
	assert.Len(s.T(), loaded, 1)
	assert.Equal(s.T(), job.ID, loaded[0].ID)
}

func (s *StoreTestSuite) TestUpdate() {
	job := &Job{
		ID:     "test-1",
		Status: StatusPending,
	}
	s.store.Save(job)

	// 更新状态
	snapshot := job.ToSnapshot()
	snapshot.Status = int(StatusFailed)
	err := s.store.Update(snapshot)
	assert.NoError(s.T(), err)

	loaded, _ := s.store.LoadAll()
	assert.Equal(s.T(), StatusFailed, JobStatus(loaded[0].Status))
}

func (s *StoreTestSuite) TestDelete() {
	job := &Job{ID: "test-1", Status: StatusPending}
	s.store.Save(job)

	err := s.store.Delete("test-1")
	assert.NoError(s.T(), err)

	loaded, _ := s.store.LoadAll()
	assert.Len(s.T(), loaded, 0)
}

func (s *StoreTestSuite) TestLoadOnlyPendingAndRunning() {
	jobs := []*Job{
		{ID: "1", Status: StatusPending},
		{ID: "2", Status: StatusRunning},
		{ID: "3", Status: StatusSuccess},
		{ID: "4", Status: StatusFailed},
	}

	for _, j := range jobs {
		s.store.Save(j)
	}

	loaded, err := s.store.LoadAll()
	assert.NoError(s.T(), err)

	// 只加载pending和running
	assert.Len(s.T(), loaded, 2)
	ids := make(map[string]bool)
	for _, s := range loaded {
		ids[s.ID] = true
	}
	assert.True(s.T(), ids["1"])
	assert.True(s.T(), ids["2"])
}

func (s *StoreTestSuite) TestConcurrentSave() {
	// 测试并发写入安全性
	for i := 0; i < 10; i++ {
		go func(idx int) {
			job := &Job{
				ID:        fmt.Sprintf("concurrent-%d", idx),
				TriggerAt: time.Now(),
			}
			err := s.store.Save(job)
			assert.NoError(s.T(), err)
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	loaded, _ := s.store.LoadAll()
	assert.Equal(s.T(), 10, len(loaded))
}

func TestStoreSuite(t *testing.T) {
	suite.Run(t, new(StoreTestSuite))
}
