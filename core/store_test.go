package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewJSONFileStore(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "test_store.json")

	store, err := NewJSONFileStore(storePath)

	require.NoError(t, err, "NewJSONFileStore should not return error")
	require.NotNil(t, store, "Store should not be nil")
	assert.Equal(t, storePath, store.filePath, "File path should be set correctly")
	assert.NotNil(t, store.data, "Data map should be initialized")
	assert.False(t, store.dirty, "Store should not be dirty initially")
}

func TestNewJSONFileStore_CreatesDirectory(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "subdir", "nested", "store.json")

	store, err := NewJSONFileStore(storePath)

	require.NoError(t, err, "Should create nested directories")
	require.NotNil(t, store)

	// Verify directory was created
	dir := filepath.Dir(storePath)
	info, err := os.Stat(dir)
	require.NoError(t, err, "Directory should exist")
	assert.True(t, info.IsDir(), "Should be a directory")
}

func TestNewJSONFileStore_LoadsExistingData(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "existing.json")

	// Create existing data
	existingData := map[string]JobSnapshot{
		"job1": {
			ID:     "job1",
			Name:   "test-job",
			Status: int(StatusPending),
		},
	}

	data, err := json.MarshalIndent(existingData, "", "  ")
	require.NoError(t, err)
	err = os.WriteFile(storePath, data, 0644)
	require.NoError(t, err)

	// Load store
	store, err := NewJSONFileStore(storePath)

	require.NoError(t, err)
	assert.Len(t, store.data, 1, "Should load existing data")
	assert.Contains(t, store.data, "job1", "Should contain existing job")
}

func TestNewJSONFileStore_EmptyFile(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "empty.json")

	// Create empty file
	err := os.WriteFile(storePath, []byte(""), 0644)
	require.NoError(t, err)

	// Load store
	store, err := NewJSONFileStore(storePath)

	require.NoError(t, err, "Should handle empty file gracefully")
	assert.Empty(t, store.data, "Data should be empty")
}

func TestNewJSONFileStore_InvalidJSON(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "invalid.json")

	// Create invalid JSON file
	err := os.WriteFile(storePath, []byte("invalid json {{{"), 0644)
	require.NoError(t, err)

	// Load store
	store, err := NewJSONFileStore(storePath)

	assert.Error(t, err, "Should return error for invalid JSON")
	assert.Nil(t, store, "Store should be nil on error")
}

func TestJSONFileStore_Save(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "save_test.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	job := &Job{
		ID:        "test-job-1",
		Name:      "test-job",
		Status:    StatusPending,
		CreatedAt: time.Now(),
	}

	err = store.Save(job)

	require.NoError(t, err, "Save should not return error")
	assert.Contains(t, store.data, "test-job-1", "Job should be in memory")

	// Verify file was written
	fileData, err := os.ReadFile(storePath)
	require.NoError(t, err, "File should exist")
	assert.NotEmpty(t, fileData, "File should not be empty")

	// Verify JSON content
	var savedData map[string]JobSnapshot
	err = json.Unmarshal(fileData, &savedData)
	require.NoError(t, err, "File should contain valid JSON")
	assert.Contains(t, savedData, "test-job-1", "File should contain saved job")
}

func TestJSONFileStore_Save_Multiple(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "multiple.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	// Save multiple jobs
	for i := 1; i <= 3; i++ {
		job := &Job{
			ID:     "job-" + string(rune('0'+i)),
			Name:   "test-job",
			Status: StatusPending,
		}
		err = store.Save(job)
		require.NoError(t, err)
	}

	assert.Len(t, store.data, 3, "Should have 3 jobs in memory")

	// Verify all jobs persisted
	fileData, err := os.ReadFile(storePath)
	require.NoError(t, err)

	var savedData map[string]JobSnapshot
	err = json.Unmarshal(fileData, &savedData)
	require.NoError(t, err)
	assert.Len(t, savedData, 3, "File should contain all 3 jobs")
}

func TestJSONFileStore_Update(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "update_test.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	// Save initial job
	job := &Job{
		ID:     "update-job",
		Name:   "original-name",
		Status: StatusPending,
	}
	err = store.Save(job)
	require.NoError(t, err)

	// Update job
	snapshot := JobSnapshot{
		ID:     "update-job",
		Name:   "updated-name",
		Status: int(StatusSuccess),
	}
	err = store.Update(snapshot)

	require.NoError(t, err, "Update should not return error")
	assert.Equal(t, "updated-name", store.data["update-job"].Name, "Name should be updated")
	assert.Equal(t, int(StatusSuccess), store.data["update-job"].Status, "Status should be updated")

	// Verify file was updated
	fileData, err := os.ReadFile(storePath)
	require.NoError(t, err)

	var savedData map[string]JobSnapshot
	err = json.Unmarshal(fileData, &savedData)
	require.NoError(t, err)
	assert.Equal(t, "updated-name", savedData["update-job"].Name, "File should contain updated data")
}

func TestJSONFileStore_Delete(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "delete_test.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	// Save jobs
	job1 := &Job{ID: "job1", Name: "test1", Status: StatusPending}
	job2 := &Job{ID: "job2", Name: "test2", Status: StatusPending}
	store.Save(job1)
	store.Save(job2)

	// Delete one job
	err = store.Delete("job1")

	require.NoError(t, err, "Delete should not return error")
	assert.NotContains(t, store.data, "job1", "Deleted job should not be in memory")
	assert.Contains(t, store.data, "job2", "Other job should remain")

	// Verify file was updated
	fileData, err := os.ReadFile(storePath)
	require.NoError(t, err)

	var savedData map[string]JobSnapshot
	err = json.Unmarshal(fileData, &savedData)
	require.NoError(t, err)
	assert.NotContains(t, savedData, "job1", "Deleted job should not be in file")
	assert.Contains(t, savedData, "job2", "Other job should be in file")
}

func TestJSONFileStore_Delete_NonExistent(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "delete_nonexistent.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	// Delete non-existent job (should not error)
	err = store.Delete("nonexistent")

	assert.NoError(t, err, "Deleting non-existent job should not error")
}

func TestJSONFileStore_LoadAll(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "loadall_test.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	// Save jobs with different statuses
	jobs := []*Job{
		{ID: "pending1", Name: "test", Status: StatusPending},
		{ID: "running1", Name: "test", Status: StatusRunning},
		{ID: "success1", Name: "test", Status: StatusSuccess},
		{ID: "failed1", Name: "test", Status: StatusFailed},
		{ID: "pending2", Name: "test", Status: StatusPending},
	}

	for _, job := range jobs {
		store.Save(job)
	}

	// Load all
	loaded, err := store.LoadAll()

	require.NoError(t, err, "LoadAll should not return error")
	
	// Should only load Pending and Running jobs
	assert.Len(t, loaded, 3, "Should load only Pending and Running jobs")

	// Verify loaded jobs
	loadedIDs := make(map[string]bool)
	for _, job := range loaded {
		loadedIDs[job.ID] = true
	}

	assert.True(t, loadedIDs["pending1"], "Should load pending job 1")
	assert.True(t, loadedIDs["running1"], "Should load running job")
	assert.True(t, loadedIDs["pending2"], "Should load pending job 2")
	assert.False(t, loadedIDs["success1"], "Should not load success job")
	assert.False(t, loadedIDs["failed1"], "Should not load failed job")
}

func TestJSONFileStore_LoadAll_Empty(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "empty_loadall.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	loaded, err := store.LoadAll()

	require.NoError(t, err, "LoadAll on empty store should not error")
	assert.Empty(t, loaded, "Should return empty slice")
}

func TestJSONFileStore_Concurrency(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "concurrent.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	// Concurrent writes
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			job := &Job{
				ID:     "concurrent-" + string(rune('0'+id)),
				Name:   "test",
				Status: StatusPending,
			}
			store.Save(job)
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify all jobs were saved
	assert.Len(t, store.data, 10, "All concurrent saves should succeed")
}

func TestJSONFileStore_AtomicWrite(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "atomic.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	job := &Job{
		ID:     "atomic-job",
		Name:   "test",
		Status: StatusPending,
	}
	err = store.Save(job)
	require.NoError(t, err)

	// Verify temp file was cleaned up
	tmpFile := storePath + ".tmp"
	_, err = os.Stat(tmpFile)
	assert.True(t, os.IsNotExist(err), "Temp file should be cleaned up")

	// Verify main file exists
	_, err = os.Stat(storePath)
	assert.NoError(t, err, "Main file should exist")
}

func TestJSONFileStore_DirtyFlag(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "dirty.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	assert.False(t, store.dirty, "Store should not be dirty initially")

	job := &Job{
		ID:     "dirty-job",
		Name:   "test",
		Status: StatusPending,
	}

	// Save should set dirty flag and then clear it after flush
	err = store.Save(job)
	require.NoError(t, err)
	assert.False(t, store.dirty, "Store should not be dirty after successful flush")
}

func TestJSONFileStore_Persistence(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "persistence.json")

	// Create first store and save data
	store1, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	job := &Job{
		ID:        "persist-job",
		Name:      "test-job",
		Status:    StatusPending,
		CreatedAt: time.Now(),
	}
	err = store1.Save(job)
	require.NoError(t, err)

	// Create second store (simulating restart)
	store2, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	// Verify data was loaded
	assert.Contains(t, store2.data, "persist-job", "Data should persist across store instances")
	assert.Equal(t, "test-job", store2.data["persist-job"].Name, "Job data should be intact")
}

func TestJSONFileStore_SaveOverwrite(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "overwrite.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	// Save job
	job1 := &Job{
		ID:     "overwrite-job",
		Name:   "original",
		Status: StatusPending,
	}
	err = store.Save(job1)
	require.NoError(t, err)

	// Save again with same ID (should overwrite)
	job2 := &Job{
		ID:     "overwrite-job",
		Name:   "updated",
		Status: StatusRunning,
	}
	err = store.Save(job2)
	require.NoError(t, err)

	assert.Len(t, store.data, 1, "Should still have only one job")
	assert.Equal(t, "updated", store.data["overwrite-job"].Name, "Job should be overwritten")
	assert.Equal(t, int(StatusRunning), store.data["overwrite-job"].Status, "Status should be updated")
}

func TestJSONFileStore_JSONFormatting(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "formatted.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	job := &Job{
		ID:     "format-job",
		Name:   "test",
		Status: StatusPending,
	}
	err = store.Save(job)
	require.NoError(t, err)

	// Read file and verify it's formatted (indented)
	fileData, err := os.ReadFile(storePath)
	require.NoError(t, err)

	content := string(fileData)
	assert.Contains(t, content, "\n", "JSON should be formatted with newlines")
	assert.Contains(t, content, "  ", "JSON should be indented")
}

func TestJSONFileStore_ComplexJobData(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "complex.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	now := time.Now()
	job := &Job{
		ID:         "complex-job",
		Name:       "complex-test",
		Payload:    []byte(`{"key": "value", "nested": {"data": 123}}`),
		TriggerAt:  now.Add(1 * time.Hour),
		CronExpr:   "*/5 * * * *",
		IsRepeat:   true,
		MaxRetries: 5,
		RetryCount: 2,
		RetryDelay: 30 * time.Second,
		Status:     StatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
		Attempts:   3,
	}

	err = store.Save(job)
	require.NoError(t, err)

	// Load and verify
	loaded, err := store.LoadAll()
	require.NoError(t, err)
	require.Len(t, loaded, 1)

	loadedJob := loaded[0]
	assert.Equal(t, job.ID, loadedJob.ID)
	assert.Equal(t, job.Name, loadedJob.Name)
	assert.Equal(t, job.CronExpr, loadedJob.CronExpr)
	assert.Equal(t, job.IsRepeat, loadedJob.IsRepeat)
	assert.Equal(t, job.MaxRetries, loadedJob.MaxRetries)
	assert.Equal(t, job.RetryCount, loadedJob.RetryCount)
	assert.Equal(t, int64(job.RetryDelay), loadedJob.RetryDelay)
	assert.Equal(t, job.Attempts, loadedJob.Attempts)
}

func TestJSONFileStore_Interface(t *testing.T) {
	// Verify JSONFileStore implements Store interface
	var _ Store = &JSONFileStore{}
}

func TestErrJobNotFound(t *testing.T) {
	err := ErrJobNotFound
	assert.Error(t, err)
	assert.Equal(t, "job not found", err.Error())
}

func TestJSONFileStore_LoadFromDisk_Permissions(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "permissions.json")

	// Create store and save data
	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	job := &Job{
		ID:     "perm-job",
		Name:   "test",
		Status: StatusPending,
	}
	err = store.Save(job)
	require.NoError(t, err)

	// Verify file has correct permissions
	info, err := os.Stat(storePath)
	require.NoError(t, err)
	
	// File should be readable and writable
	mode := info.Mode()
	assert.True(t, mode&0600 != 0, "File should have read/write permissions")
}

func TestJSONFileStore_MultipleDeletes(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "multi_delete.json")

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	// Save multiple jobs
	for i := 1; i <= 5; i++ {
		job := &Job{
			ID:     "job-" + string(rune('0'+i)),
			Name:   "test",
			Status: StatusPending,
		}
		store.Save(job)
	}

	// Delete multiple jobs
	store.Delete("job-1")
	store.Delete("job-3")
	store.Delete("job-5")

	assert.Len(t, store.data, 2, "Should have 2 jobs remaining")
	assert.Contains(t, store.data, "job-2")
	assert.Contains(t, store.data, "job-4")
}
