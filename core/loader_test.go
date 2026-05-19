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
	tempDir := t.TempDir()
	archiveDir := filepath.Join(tempDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	jobFile := filepath.Join(tempDir, "test_job.json")
	jobContent := `{
		"id": "file-job-1",
		"name": "test_job",
		"delay": "5m",
		"payload": {"key": "value"},
		"max_retries": 2
	}`
	require.NoError(t, os.WriteFile(jobFile, []byte(jobContent), 0o644))

	storePath := filepath.Join(tempDir, "jobs-store.json")
	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)
	scheduler := NewScheduler(store, nil, nil)

	loader, err := NewDirectoryLoader(scheduler, LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: ArchiveAfterLoad,
		ArchiveDir:     archiveDir,
		Recursive:      false,
	})
	require.NoError(t, err)

	err = loader.ScanAndLoad()
	assert.NoError(t, err)

	_, err = os.Stat(jobFile)
	assert.True(t, os.IsNotExist(err))

	files, err := os.ReadDir(archiveDir)
	require.NoError(t, err)
	assert.Len(t, files, 1)
	assert.Equal(t, 1, scheduler.HeapLen())

	loaded, err := store.LoadAll()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "file-job-1", loaded[0].ID)
	assert.Equal(t, "test_job", loaded[0].Name)
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

	t.Run("with cron metadata", func(t *testing.T) {
		format := &FileJobFormat{
			Name:     "cronjob",
			CronExpr: "0 0 * * *",
			IsRepeat: true,
		}
		job, err := loader.formatToJob(format)
		require.NoError(t, err)
		assert.True(t, job.TriggerAt.After(now))
		assert.Equal(t, "0 0 * * *", job.CronExpr)
		assert.True(t, job.IsRepeat)
	})

	t.Run("invalid delay", func(t *testing.T) {
		format := &FileJobFormat{Delay: "abc"}
		_, err := loader.formatToJob(format)
		assert.Error(t, err)
	})
}
