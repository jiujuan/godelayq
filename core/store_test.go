package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flushSeqOf 在持锁状态下读取实际写盘次数
func flushSeqOf(s *JSONFileStore) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.flushSeq
}

// TestJSONFileStore_DebounceCoalescesWrites 未到期前不落盘，
// 大量变更由一次 Flush 合并为一次写。
func TestJSONFileStore_DebounceCoalescesWrites(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "debounce.json")

	// 周期设得足够长，确保只有显式 Flush 才会写盘
	store, err := NewJSONFileStoreWithOptions(storePath, StoreOptions{Interval: time.Hour})
	require.NoError(t, err)

	for i := 0; i < 400; i++ {
		require.NoError(t, store.Save(&Job{ID: "job-" + strconv.Itoa(i), Name: "burst", Status: StatusPending}))
	}
	assert.Equal(t, 0, flushSeqOf(store), "debounced store must not write per mutation")
	_, err = os.Stat(storePath)
	assert.True(t, os.IsNotExist(err), "file must not exist before the first flush")

	require.NoError(t, store.Flush())
	assert.Equal(t, 1, flushSeqOf(store), "400 mutations must collapse into a single write")

	require.NoError(t, store.Close())

	reloaded, err := NewJSONFileStore(storePath)
	require.NoError(t, err)
	defer reloaded.Close()
	assert.Len(t, reloaded.data, 400)
}

// TestJSONFileStore_PeriodicFlush 后台周期把突发合并成少量写入
func TestJSONFileStore_PeriodicFlush(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "periodic.json")

	store, err := NewJSONFileStoreWithOptions(storePath, StoreOptions{Interval: 20 * time.Millisecond})
	require.NoError(t, err)
	defer store.Close()

	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.Save(&Job{ID: "job-" + strconv.Itoa(i), Name: "burst", Status: StatusPending})
		}(i)
	}
	wg.Wait()

	time.Sleep(150 * time.Millisecond)

	writes := flushSeqOf(store)
	assert.Greater(t, writes, 0, "background flush should have written the state")
	assert.Less(t, writes, 20, "300 mutations must be coalesced, got %d writes", writes)
}

// TestJSONFileStore_InvalidIntervalFallsBack 非正数周期回退到默认值，
// Close 可重复调用。
func TestJSONFileStore_InvalidIntervalFallsBack(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "fallback.json")

	store, err := NewJSONFileStoreWithOptions(storePath, StoreOptions{Interval: 0})
	require.NoError(t, err)
	assert.Equal(t, DefaultFlushInterval, store.interval)

	require.NoError(t, store.Save(&Job{ID: "j", Name: "n", Status: StatusPending}))
	require.NoError(t, store.Close())
	require.NoError(t, store.Close(), "Close must be idempotent")

	reloaded, err := NewJSONFileStore(storePath)
	require.NoError(t, err)
	defer reloaded.Close()
	assert.Contains(t, reloaded.data, "j")
}

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

	require.NoError(t, store.Flush(), "Flush should succeed")

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

	require.NoError(t, store.Flush())

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

	require.NoError(t, store.Flush())

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

	require.NoError(t, store.Flush())

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
	defer store.Close()

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

	// 终态留痕也在其中：状态过滤由调用方负责
	assert.Len(t, loaded, len(jobs), "LoadAll must return every stored snapshot")

	// Verify loaded jobs
	loadedIDs := make(map[string]bool)
	for _, job := range loaded {
		loadedIDs[job.ID] = true
	}

	assert.True(t, loadedIDs["pending1"], "Should load pending job 1")
	assert.True(t, loadedIDs["running1"], "Should load running job")
	assert.True(t, loadedIDs["pending2"], "Should load pending job 2")
	assert.True(t, loadedIDs["success1"], "Should load completed job for history")
	assert.True(t, loadedIDs["failed1"], "Should load failed job for history")
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

	require.NoError(t, store.Flush(), "Flush should write the pending changes")

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

	// Save should mark dirty and keep it dirty until a flush happens
	err = store.Save(job)
	require.NoError(t, err)
	assert.True(t, store.dirty, "Store should be dirty after Save (debounced write)")

	require.NoError(t, store.Flush())
	assert.False(t, store.dirty, "Store should not be dirty after successful flush")

	// A flush without changes must not rewrite the file
	before := store.flushSeq
	require.NoError(t, store.Flush())
	assert.Equal(t, before, store.flushSeq, "Clean flush should not write again")
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
	require.NoError(t, store1.Close(), "Close should flush and stop the background writer")

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
	require.NoError(t, store.Flush())

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
	require.NoError(t, store.Flush())

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

// TestJSONFileStore_LegacySnapshotWithoutExec 读一份升级前写的数据文件：
// 记录里没有 exec 键，还带一个当前结构体不认识的 future_key。
// 解码必须照常成功、Exec 为 nil；随后写入带摘要的快照再读回，老记录不受影响。
// 这条是"无需迁移脚本即可上线"的证据，也是回滚路径（旧代码读带 exec 的文件）的镜像。
func TestJSONFileStore_LegacySnapshotWithoutExec(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "jobs.json")

	legacy := []byte(`{"job_old_1":{"id":"job_old_1","name":"payment_check","payload":"e30=",` +
		`"trigger_at":"2024-01-02T15:30:00+08:00","cron_expr":"","is_repeat":false,` +
		`"timeout":0,"max_retries":3,"retry_count":1,"retry_delay":60000000000,` +
		`"status":0,"created_at":"2024-01-02T15:20:00+08:00","updated_at":"2024-01-02T15:20:00+08:00",` +
		`"attempts":1,"future_key":123}}`)
	require.NoError(t, os.WriteFile(storePath, legacy, 0o644))

	store, err := NewJSONFileStore(storePath)
	require.NoError(t, err)

	items, err := store.LoadAll()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "payment_check", items[0].Name)
	assert.Equal(t, 3, items[0].MaxRetries, "老记录自身字段要完整读回")
	assert.Nil(t, items[0].Exec, "没有 exec 键的旧数据应解出 nil，而不是空摘要")

	// 在老记录旁边写入一条带摘要的终态记录，然后重开一次存储读回
	legacySnap := items[0]
	require.NoError(t, store.Update(legacySnap))
	require.NoError(t, store.Save(&Job{
		ID:        "job_exec_1",
		Name:      "exec.nightly_report",
		Status:    StatusSuccess,
		UpdatedAt: time.Now(),
		Exec:      &ExecMeta{Kind: "script", Profile: "nightly_report", DurationMs: 900, OutBytes: 20, ExitCode: 0, Preview: "done"},
	}))
	require.NoError(t, store.Close())

	reopened, err := NewJSONFileStore(storePath)
	require.NoError(t, err)
	defer reopened.Close()

	all, err := reopened.LoadAll()
	require.NoError(t, err)
	require.Len(t, all, 2)

	byID := make(map[string]JobSnapshot, len(all))
	for _, snap := range all {
		byID[snap.ID] = snap
	}
	assert.Nil(t, byID["job_old_1"].Exec, "重写之后老记录仍然不该有摘要")
	require.NotNil(t, byID["job_exec_1"].Exec)
	assert.Equal(t, int64(900), byID["job_exec_1"].Exec.DurationMs)
	assert.Equal(t, "done", byID["job_exec_1"].Exec.Preview)
}

// TestJSONFileStore_KeepsExecOutOfHotPathSize 是给"完整输出不进快照"这条决策上的量化守卫：
// 500 条终态记录各带 2KB 预览，文件仍应在几 MB 量级。
// 谁以后把 stdout 正文塞进快照，这条会先失败——那正是整文件重写扛不住的写法。
func TestJSONFileStore_KeepsExecOutOfHotPathSize(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "bulk.json")

	store, err := NewJSONFileStoreWithOptions(storePath, StoreOptions{
		Interval:     time.Hour,
		HistoryLimit: 1000,
	})
	require.NoError(t, err)

	preview := strings.Repeat("输出尾部预览", 114) // 约 2KB 的 UTF-8 文本，与 inline_preview 默认值同量级
	require.Equal(t, 2052, len(preview))

	for i := 0; i < 500; i++ {
		require.NoError(t, store.Save(&Job{
			ID:        "job-" + strconv.Itoa(i),
			Name:      "exec.nightly_report",
			Status:    StatusSuccess,
			TriggerAt: time.Now(),
			UpdatedAt: time.Now(),
			Exec: &ExecMeta{
				Kind:       "script",
				Profile:    "nightly_report",
				DurationMs: int64(i),
				OutBytes:   int64(len(preview)),
				Preview:    preview,
			},
		}))
	}

	require.NoError(t, store.Close())

	info, err := os.Stat(storePath)
	require.NoError(t, err)
	assert.Less(t, info.Size(), int64(5<<20), "500 条 2KB 预览不该把 jobs.json 推到 5MB 以上")
	assert.Greater(t, info.Size(), int64(500*len(preview)/2), "断言不能是空跑的：预览确实落进了文件")

	// 读回抽查一条，证明限制是"小"而不是"写坏了"
	reopened, err := NewJSONFileStore(storePath)
	require.NoError(t, err)
	defer reopened.Close()

	items, err := reopened.LoadAll()
	require.NoError(t, err)
	require.Len(t, items, 500)
	for _, snap := range items {
		require.NotNil(t, snap.Exec)
		assert.Equal(t, len(preview), len(snap.Exec.Preview))
	}
}
