package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectoryLoaderScanAndLoad(t *testing.T) {
	// 创建临时目录
	tempDir := t.TempDir()
	archiveDir := filepath.Join(tempDir, "archive")
	os.MkdirAll(archiveDir, 0755)

	// 创建测试任务文件
	jobFile := filepath.Join(tempDir, "test_job.json")
	jobContent := `{
		"id": "file-job-1",
		"name": "test_job",
		"delay": "5m",
		"payload": {"key": "value"},
		"max_retries": 2
	}`
	err := os.WriteFile(jobFile, []byte(jobContent), 0644)
	require.NoError(t, err)

	// Mock调度器
	mockSched := &MockScheduler{}
	mockSched.On("Schedule", mock.AnythingOfType("*core.Job")).Return(nil)

	loader, err := NewDirectoryLoader(mockSched, LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: ArchiveAfterLoad,
		ArchiveDir:     archiveDir,
		Recursive:      false,
	})
	require.NoError(t, err)

	// 执行扫描
	err = loader.ScanAndLoad()
	assert.NoError(t, err)

	// 验证文件被归档
	_, err = os.Stat(jobFile)
	assert.True(t, os.IsNotExist(err))

	files, _ := os.ReadDir(archiveDir)
	assert.Len(t, files, 1)

	mockSched.AssertExpectations(t)
}

func TestDirectoryLoaderFormatToJob(t *testing.T) {
	loader := &DirectoryLoader{}

	now := time.Now()

	t.Run("with delay", func(t *testing.T) {
		format := &FileJobFormat{
			Name:       "test",
			Delay:      "10m",
			MaxRetries: 3,
		}
		job, err := loader.formatToJob(format)
		require.NoError(t, err)
		assert.True(t, job.TriggerAt.After(now.Add(9*time.Minute)))
		assert.Equal(t, 3, job.MaxRetries)
	})

	t.Run("with cron", func(t *testing.T) {
		format := &FileJobFormat{
			Name:     "cronjob",
			CronExpr: "0 0 * * *",
			IsRepeat: true,
		}
		job, err := loader.formatToJob(format)
		require.NoError(t, err)
		assert.True(t, job.TriggerAt.After(now))
		assert.True(t, job.IsRepeat)
	})

	t.Run("invalid delay", func(t *testing.T) {
		format := &FileJobFormat{Delay: "abc"}
		_, err := loader.formatToJob(format)
		assert.Error(t, err)
	})
}
