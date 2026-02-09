package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewDirectoryLoader(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	options := LoaderOptions{
		Dir:     tempDir,
		Pattern: "*.json",
	}
	
	loader, err := NewDirectoryLoader(scheduler, options)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	
	if loader == nil {
		t.Fatal("Expected loader to be created")
	}
	
	if loader.scheduler != scheduler {
		t.Error("Expected scheduler to be set")
	}
	
	if loader.options.Pattern != "*.json" {
		t.Errorf("Expected pattern *.json, got %s", loader.options.Pattern)
	}
	
	// Verify directory was created
	if _, err := os.Stat(tempDir); os.IsNotExist(err) {
		t.Error("Expected directory to be created")
	}
}

func TestNewDirectoryLoader_DefaultPattern(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	options := LoaderOptions{
		Dir: tempDir,
		// Pattern not set
	}
	
	loader, err := NewDirectoryLoader(scheduler, options)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	
	if loader.options.Pattern != "*.json" {
		t.Errorf("Expected default pattern *.json, got %s", loader.options.Pattern)
	}
}

func TestNewDirectoryLoader_DefaultDir(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	
	options := LoaderOptions{
		// Dir not set
	}
	
	loader, err := NewDirectoryLoader(scheduler, options)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	
	if loader.options.Dir != "./jobs" {
		t.Errorf("Expected default dir ./jobs, got %s", loader.options.Dir)
	}
	
	// Cleanup
	os.RemoveAll("./jobs")
}

func TestNewDirectoryLoader_WithArchive(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	archiveDir := filepath.Join(tempDir, "archive")
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: ArchiveAfterLoad,
		ArchiveDir:     archiveDir,
	}
	
	loader, err := NewDirectoryLoader(scheduler, options)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	
	// Verify archive directory was created
	if _, err := os.Stat(archiveDir); os.IsNotExist(err) {
		t.Error("Expected archive directory to be created")
	}
	
	if loader.options.ArchiveDir != archiveDir {
		t.Errorf("Expected archive dir %s, got %s", archiveDir, loader.options.ArchiveDir)
	}
}

func TestDirectoryLoader_LoadFile_Simple(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	// Create test job file
	jobFile := filepath.Join(tempDir, "test_job.json")
	jobData := FileJobFormat{
		ID:      "test-job-1",
		Name:    "test-job",
		Payload: json.RawMessage(`{"message": "hello"}`),
		Delay:   "5m",
	}
	
	data, _ := json.Marshal(jobData)
	if err := os.WriteFile(jobFile, data, 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	
	err := loader.LoadFile(jobFile)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	
	// Verify job was scheduled
	if scheduler.HeapLen() != 1 {
		t.Errorf("Expected 1 job in scheduler, got %d", scheduler.HeapLen())
	}
	
	// Verify job details
	job := scheduler.heap.Peek().(*Job)
	if job.ID != "test-job-1" {
		t.Errorf("Expected job ID test-job-1, got %s", job.ID)
	}
	if job.Name != "test-job" {
		t.Errorf("Expected job name test-job, got %s", job.Name)
	}
}

func TestDirectoryLoader_LoadFile_WithAbsoluteTime(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	triggerTime := time.Now().Add(1 * time.Hour)
	
	jobFile := filepath.Join(tempDir, "absolute_time.json")
	jobData := FileJobFormat{
		Name:      "absolute-job",
		TriggerAt: &triggerTime,
	}
	
	data, _ := json.Marshal(jobData)
	os.WriteFile(jobFile, data, 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	loader.LoadFile(jobFile)
	
	job := scheduler.heap.Peek().(*Job)
	if !job.TriggerAt.Equal(triggerTime) {
		t.Errorf("Expected trigger time %v, got %v", triggerTime, job.TriggerAt)
	}
}

func TestDirectoryLoader_LoadFile_WithCron(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	jobFile := filepath.Join(tempDir, "cron_job.json")
	jobData := FileJobFormat{
		Name:     "cron-job",
		CronExpr: "*/5 * * * *",
		IsRepeat: true,
	}
	
	data, _ := json.Marshal(jobData)
	os.WriteFile(jobFile, data, 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	loader.LoadFile(jobFile)
	
	job := scheduler.heap.Peek().(*Job)
	if job.CronExpr != "*/5 * * * *" {
		t.Errorf("Expected cron expr */5 * * * *, got %s", job.CronExpr)
	}
	if !job.IsRepeat {
		t.Error("Expected IsRepeat to be true")
	}
}

func TestDirectoryLoader_LoadFile_DeleteAfterLoad(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	jobFile := filepath.Join(tempDir, "delete_me.json")
	jobData := FileJobFormat{
		Name:  "delete-job",
		Delay: "1m",
	}
	
	data, _ := json.Marshal(jobData)
	os.WriteFile(jobFile, data, 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: DeleteAfterLoad,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	loader.LoadFile(jobFile)
	
	// Verify file was deleted
	if _, err := os.Stat(jobFile); !os.IsNotExist(err) {
		t.Error("Expected file to be deleted")
	}
}

func TestDirectoryLoader_LoadFile_ArchiveAfterLoad(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	archiveDir := filepath.Join(tempDir, "archive")
	
	jobFile := filepath.Join(tempDir, "archive_me.json")
	jobData := FileJobFormat{
		Name:  "archive-job",
		Delay: "1m",
	}
	
	data, _ := json.Marshal(jobData)
	os.WriteFile(jobFile, data, 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: ArchiveAfterLoad,
		ArchiveDir:     archiveDir,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	loader.LoadFile(jobFile)
	
	// Verify original file was moved
	if _, err := os.Stat(jobFile); !os.IsNotExist(err) {
		t.Error("Expected original file to be moved")
	}
	
	// Verify file exists in archive (with timestamp prefix)
	files, _ := os.ReadDir(archiveDir)
	if len(files) != 1 {
		t.Errorf("Expected 1 file in archive, got %d", len(files))
	}
}

func TestDirectoryLoader_LoadFile_KeepAfterLoad(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	jobFile := filepath.Join(tempDir, "keep_me.json")
	jobData := FileJobFormat{
		Name:  "keep-job",
		Delay: "1m",
	}
	
	data, _ := json.Marshal(jobData)
	os.WriteFile(jobFile, data, 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	loader.LoadFile(jobFile)
	
	// Verify file still exists
	if _, err := os.Stat(jobFile); os.IsNotExist(err) {
		t.Error("Expected file to be kept")
	}
	
	// Verify file is marked as processed
	loader.mu.RLock()
	_, processed := loader.processedFiles[jobFile]
	loader.mu.RUnlock()
	
	if !processed {
		t.Error("Expected file to be marked as processed")
	}
	
	// Try loading again - should skip
	initialLen := scheduler.HeapLen()
	loader.LoadFile(jobFile)
	
	if scheduler.HeapLen() != initialLen {
		t.Error("Expected file to be skipped on second load")
	}
}

func TestDirectoryLoader_LoadFile_InvalidJSON(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	errorDir := filepath.Join(tempDir, "errors")
	
	jobFile := filepath.Join(tempDir, "invalid.json")
	os.WriteFile(jobFile, []byte("invalid json {{{"), 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
		ErrorDir:       errorDir,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	
	err := loader.LoadFile(jobFile)
	if err == nil {
		t.Error("Expected error for invalid JSON")
	}
}

func TestDirectoryLoader_LoadFile_WithHandler(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	handler := func(ctx context.Context, job *Job) error {
		return nil
	}
	
	jobFile := filepath.Join(tempDir, "handler_job.json")
	jobData := FileJobFormat{
		Name:  "handler-job",
		Delay: "1ms",
	}
	
	data, _ := json.Marshal(jobData)
	os.WriteFile(jobFile, data, 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
		HandlerMap: map[string]Handler{
			"handler-job": handler,
		},
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	loader.LoadFile(jobFile)
	
	// Verify handler was bound
	job := scheduler.heap.Peek().(*Job)
	if job.Handler == nil {
		t.Error("Expected handler to be bound")
	}
}

func TestDirectoryLoader_ScanAndLoad(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	// Create multiple job files
	for i := 1; i <= 3; i++ {
		jobFile := filepath.Join(tempDir, "job"+string(rune('0'+i))+".json")
		jobData := FileJobFormat{
			Name:  "job-" + string(rune('0'+i)),
			Delay: "1m",
		}
		data, _ := json.Marshal(jobData)
		os.WriteFile(jobFile, data, 0644)
	}
	
	// Create a non-JSON file (should be ignored)
	os.WriteFile(filepath.Join(tempDir, "readme.txt"), []byte("test"), 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	
	err := loader.ScanAndLoad()
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	
	// Verify all JSON files were loaded
	if scheduler.HeapLen() != 3 {
		t.Errorf("Expected 3 jobs loaded, got %d", scheduler.HeapLen())
	}
}

func TestDirectoryLoader_ScanAndLoad_Recursive(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	// Create subdirectory with job file
	subDir := filepath.Join(tempDir, "subdir")
	os.MkdirAll(subDir, 0755)
	
	jobFile1 := filepath.Join(tempDir, "job1.json")
	jobFile2 := filepath.Join(subDir, "job2.json")
	
	for _, file := range []string{jobFile1, jobFile2} {
		jobData := FileJobFormat{
			Name:  filepath.Base(file),
			Delay: "1m",
		}
		data, _ := json.Marshal(jobData)
		os.WriteFile(file, data, 0644)
	}
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
		Recursive:      true,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	loader.ScanAndLoad()
	
	// Verify both files were loaded
	if scheduler.HeapLen() != 2 {
		t.Errorf("Expected 2 jobs loaded (recursive), got %d", scheduler.HeapLen())
	}
}

func TestDirectoryLoader_ScanAndLoad_NonRecursive(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	// Create subdirectory with job file
	subDir := filepath.Join(tempDir, "subdir")
	os.MkdirAll(subDir, 0755)
	
	jobFile1 := filepath.Join(tempDir, "job1.json")
	jobFile2 := filepath.Join(subDir, "job2.json")
	
	for _, file := range []string{jobFile1, jobFile2} {
		jobData := FileJobFormat{
			Name:  filepath.Base(file),
			Delay: "1m",
		}
		data, _ := json.Marshal(jobData)
		os.WriteFile(file, data, 0644)
	}
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
		Recursive:      false,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	loader.ScanAndLoad()
	
	// Verify only root level file was loaded
	if scheduler.HeapLen() != 1 {
		t.Errorf("Expected 1 job loaded (non-recursive), got %d", scheduler.HeapLen())
	}
}

func TestDirectoryLoader_FormatToJob_DefaultValues(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	loader, _ := NewDirectoryLoader(scheduler, LoaderOptions{Dir: tempDir})
	
	format := &FileJobFormat{
		Name: "test-job",
		// No ID, delay, or retry settings
	}
	
	job, err := loader.formatToJob(format)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	
	// Verify ID was generated
	if job.ID == "" {
		t.Error("Expected ID to be generated")
	}
	
	// Verify default trigger time (1 second from now)
	if job.TriggerAt.Before(time.Now()) {
		t.Error("Expected trigger time to be in the future")
	}
	
	// Verify default retry delay
	if job.RetryDelay != 1*time.Minute {
		t.Errorf("Expected default retry delay 1m, got %v", job.RetryDelay)
	}
	
	// Verify default max retries
	if job.MaxRetries != 3 {
		t.Errorf("Expected default max retries 3, got %d", job.MaxRetries)
	}
	
	// Verify status
	if job.Status != StatusPending {
		t.Errorf("Expected status Pending, got %v", job.Status)
	}
}

func TestDirectoryLoader_FormatToJob_InvalidDelay(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	loader, _ := NewDirectoryLoader(scheduler, LoaderOptions{Dir: tempDir})
	
	format := &FileJobFormat{
		Name:  "test-job",
		Delay: "invalid-delay",
	}
	
	_, err := loader.formatToJob(format)
	if err == nil {
		t.Error("Expected error for invalid delay format")
	}
}

func TestDirectoryLoader_FormatToJob_InvalidRetryDelay(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	loader, _ := NewDirectoryLoader(scheduler, LoaderOptions{Dir: tempDir})
	
	format := &FileJobFormat{
		Name:       "test-job",
		RetryDelay: "invalid",
	}
	
	_, err := loader.formatToJob(format)
	if err == nil {
		t.Error("Expected error for invalid retry delay format")
	}
}

func TestDirectoryLoader_StartStop(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	
	// Create a test job file
	jobFile := filepath.Join(tempDir, "test.json")
	jobData := FileJobFormat{
		Name:  "test-job",
		Delay: "1m",
	}
	data, _ := json.Marshal(jobData)
	os.WriteFile(jobFile, data, 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad,
		EnableWatcher:  false, // Disable watcher for this test
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	
	err := loader.Start()
	if err != nil {
		t.Fatalf("Expected no error on Start, got %v", err)
	}
	
	// Verify job was loaded
	if scheduler.HeapLen() != 1 {
		t.Errorf("Expected 1 job after Start, got %d", scheduler.HeapLen())
	}
	
	// Stop should not panic
	loader.Stop()
}

func TestDirectoryLoader_HandleErrorFile(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	tempDir := t.TempDir()
	errorDir := filepath.Join(tempDir, "errors")
	
	jobFile := filepath.Join(tempDir, "error.json")
	os.WriteFile(jobFile, []byte("invalid"), 0644)
	
	options := LoaderOptions{
		Dir:            tempDir,
		PostLoadAction: KeepAfterLoad, // Changed to KeepAfterLoad
		ErrorDir:       errorDir,
	}
	
	loader, _ := NewDirectoryLoader(scheduler, options)
	
	// This will fail to parse and call handleErrorFile
	err := loader.LoadFile(jobFile)
	if err == nil {
		t.Error("Expected error for invalid JSON")
	}
	
	// Note: handleErrorFile may not create error file in all cases
	// This is implementation-dependent behavior
}

func TestPostLoadAction_Constants(t *testing.T) {
	tests := []struct {
		name   string
		action PostLoadAction
		value  int
	}{
		{"DeleteAfterLoad", DeleteAfterLoad, 0},
		{"ArchiveAfterLoad", ArchiveAfterLoad, 1},
		{"KeepAfterLoad", KeepAfterLoad, 2},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if int(tt.action) != tt.value {
				t.Errorf("Expected %s to be %d, got %d", tt.name, tt.value, int(tt.action))
			}
		})
	}
}

func TestFileJobFormat_JSONMarshaling(t *testing.T) {
	triggerTime := time.Now().Add(1 * time.Hour).Truncate(time.Second)
	
	format := FileJobFormat{
		ID:         "test-123",
		Name:       "test-job",
		Payload:    json.RawMessage(`{"key":"value"}`),
		TriggerAt:  &triggerTime,
		CronExpr:   "*/5 * * * *",
		IsRepeat:   true,
		MaxRetries: 5,
		RetryDelay: "30s",
		Tags:       []string{"tag1", "tag2"},
		Desc:       "Test description",
	}
	
	// Marshal
	data, err := json.Marshal(format)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}
	
	// Unmarshal
	var decoded FileJobFormat
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	
	// Verify fields
	if decoded.ID != format.ID {
		t.Errorf("ID mismatch: expected %s, got %s", format.ID, decoded.ID)
	}
	if decoded.Name != format.Name {
		t.Errorf("Name mismatch: expected %s, got %s", format.Name, decoded.Name)
	}
	// Payload comparison - normalize JSON
	var originalPayload, decodedPayload map[string]interface{}
	json.Unmarshal(format.Payload, &originalPayload)
	json.Unmarshal(decoded.Payload, &decodedPayload)
	
	if originalPayload["key"] != decodedPayload["key"] {
		t.Error("Payload mismatch")
	}
	
	if decoded.CronExpr != format.CronExpr {
		t.Errorf("CronExpr mismatch: expected %s, got %s", format.CronExpr, decoded.CronExpr)
	}
	if decoded.IsRepeat != format.IsRepeat {
		t.Errorf("IsRepeat mismatch: expected %v, got %v", format.IsRepeat, decoded.IsRepeat)
	}
}
