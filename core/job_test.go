package core

import (
	"context"
	"testing"
	"time"
)

func TestJobStatus_Constants(t *testing.T) {
	tests := []struct {
		name   string
		status JobStatus
		value  int
	}{
		{"StatusPending", StatusPending, 0},
		{"StatusRunning", StatusRunning, 1},
		{"StatusSuccess", StatusSuccess, 2},
		{"StatusFailed", StatusFailed, 3},
		{"StatusCancelled", StatusCancelled, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if int(tt.status) != tt.value {
				t.Errorf("Expected %s to be %d, got %d", tt.name, tt.value, int(tt.status))
			}
		})
	}
}

func TestJob_GetTriggerTime(t *testing.T) {
	triggerTime := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
	job := &Job{
		ID:        "test-job-1",
		TriggerAt: triggerTime,
	}

	result := job.GetTriggerTime()
	if !result.Equal(triggerTime) {
		t.Errorf("Expected trigger time %v, got %v", triggerTime, result)
	}
}

func TestJob_GetID(t *testing.T) {
	job := &Job{
		ID: "test-job-123",
	}

	result := job.GetID()
	if result != "test-job-123" {
		t.Errorf("Expected ID 'test-job-123', got '%s'", result)
	}
}

func TestJob_CloneForRetry(t *testing.T) {
	originalTime := time.Now()
	nextTime := originalTime.Add(5 * time.Minute)
	
	handler := func(ctx context.Context, job *Job) error {
		return nil
	}

	original := &Job{
		ID:         "original-job",
		Name:       "Test Job",
		Payload:    []byte("test payload"),
		TriggerAt:  originalTime,
		Handler:    handler,
		CronExpr:   "*/5 * * * *",
		IsRepeat:   true,
		MaxRetries: 3,
		RetryCount: 1,
		RetryDelay: 10 * time.Second,
		Status:     StatusFailed,
		CreatedAt:  originalTime.Add(-1 * time.Hour),
		UpdatedAt:  originalTime,
		Attempts:   2,
	}

	clone := original.CloneForRetry(nextTime)

	// Verify ID is different and contains retry suffix
	if clone.ID == original.ID {
		t.Error("Clone ID should be different from original")
	}
	if len(clone.ID) <= len(original.ID) {
		t.Error("Clone ID should contain retry suffix")
	}

	// Verify copied fields
	if clone.Name != original.Name {
		t.Errorf("Expected Name %s, got %s", original.Name, clone.Name)
	}
	if string(clone.Payload) != string(original.Payload) {
		t.Errorf("Expected Payload %s, got %s", original.Payload, clone.Payload)
	}
	if !clone.TriggerAt.Equal(nextTime) {
		t.Errorf("Expected TriggerAt %v, got %v", nextTime, clone.TriggerAt)
	}
	if clone.CronExpr != original.CronExpr {
		t.Errorf("Expected CronExpr %s, got %s", original.CronExpr, clone.CronExpr)
	}
	if clone.IsRepeat != original.IsRepeat {
		t.Errorf("Expected IsRepeat %v, got %v", original.IsRepeat, clone.IsRepeat)
	}
	if clone.MaxRetries != original.MaxRetries {
		t.Errorf("Expected MaxRetries %d, got %d", original.MaxRetries, clone.MaxRetries)
	}

	// Verify incremented retry count
	if clone.RetryCount != original.RetryCount+1 {
		t.Errorf("Expected RetryCount %d, got %d", original.RetryCount+1, clone.RetryCount)
	}

	// Verify exponential backoff
	expectedDelay := original.RetryDelay * 2
	if clone.RetryDelay != expectedDelay {
		t.Errorf("Expected RetryDelay %v, got %v", expectedDelay, clone.RetryDelay)
	}

	// Verify status reset to pending
	if clone.Status != StatusPending {
		t.Errorf("Expected Status %v, got %v", StatusPending, clone.Status)
	}

	// Verify CreatedAt preserved
	if !clone.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("Expected CreatedAt %v, got %v", original.CreatedAt, clone.CreatedAt)
	}

	// Verify UpdatedAt is recent
	if clone.UpdatedAt.Before(original.UpdatedAt) {
		t.Error("Clone UpdatedAt should be after or equal to original UpdatedAt")
	}
}

func TestJob_ToSnapshot(t *testing.T) {
	now := time.Now()
	handler := func(ctx context.Context, job *Job) error {
		return nil
	}

	job := &Job{
		ID:         "job-123",
		Name:       "Test Job",
		Payload:    []byte("test data"),
		TriggerAt:  now,
		Handler:    handler,
		Ctx:        context.Background(),
		CronExpr:   "0 * * * *",
		IsRepeat:   true,
		MaxRetries: 5,
		RetryCount: 2,
		RetryDelay: 30 * time.Second,
		Status:     StatusRunning,
		CreatedAt:  now.Add(-1 * time.Hour),
		UpdatedAt:  now,
		Attempts:   3,
	}

	snapshot := job.ToSnapshot()

	// Verify all fields are correctly copied
	if snapshot.ID != job.ID {
		t.Errorf("Expected ID %s, got %s", job.ID, snapshot.ID)
	}
	if snapshot.Name != job.Name {
		t.Errorf("Expected Name %s, got %s", job.Name, snapshot.Name)
	}
	if string(snapshot.Payload) != string(job.Payload) {
		t.Errorf("Expected Payload %s, got %s", job.Payload, snapshot.Payload)
	}
	if !snapshot.TriggerAt.Equal(job.TriggerAt) {
		t.Errorf("Expected TriggerAt %v, got %v", job.TriggerAt, snapshot.TriggerAt)
	}
	if snapshot.CronExpr != job.CronExpr {
		t.Errorf("Expected CronExpr %s, got %s", job.CronExpr, snapshot.CronExpr)
	}
	if snapshot.IsRepeat != job.IsRepeat {
		t.Errorf("Expected IsRepeat %v, got %v", job.IsRepeat, snapshot.IsRepeat)
	}
	if snapshot.MaxRetries != job.MaxRetries {
		t.Errorf("Expected MaxRetries %d, got %d", job.MaxRetries, snapshot.MaxRetries)
	}
	if snapshot.RetryCount != job.RetryCount {
		t.Errorf("Expected RetryCount %d, got %d", job.RetryCount, snapshot.RetryCount)
	}
	if snapshot.RetryDelay != int64(job.RetryDelay) {
		t.Errorf("Expected RetryDelay %d, got %d", int64(job.RetryDelay), snapshot.RetryDelay)
	}
	if snapshot.Status != int(job.Status) {
		t.Errorf("Expected Status %d, got %d", int(job.Status), snapshot.Status)
	}
	if !snapshot.CreatedAt.Equal(job.CreatedAt) {
		t.Errorf("Expected CreatedAt %v, got %v", job.CreatedAt, snapshot.CreatedAt)
	}
	if !snapshot.UpdatedAt.Equal(job.UpdatedAt) {
		t.Errorf("Expected UpdatedAt %v, got %v", job.UpdatedAt, snapshot.UpdatedAt)
	}
	if snapshot.Attempts != job.Attempts {
		t.Errorf("Expected Attempts %d, got %d", job.Attempts, snapshot.Attempts)
	}
}

func TestJob_FromSnapshot(t *testing.T) {
	now := time.Now()
	snapshot := JobSnapshot{
		ID:         "snapshot-job-456",
		Name:       "Snapshot Job",
		Payload:    []byte("snapshot data"),
		TriggerAt:  now,
		CronExpr:   "*/10 * * * *",
		IsRepeat:   false,
		MaxRetries: 3,
		RetryCount: 1,
		RetryDelay: int64(15 * time.Second),
		Status:     int(StatusFailed),
		CreatedAt:  now.Add(-2 * time.Hour),
		UpdatedAt:  now.Add(-1 * time.Hour),
		Attempts:   2,
	}

	job := &Job{}
	job.FromSnapshot(snapshot)

	// Verify all fields are correctly restored
	if job.ID != snapshot.ID {
		t.Errorf("Expected ID %s, got %s", snapshot.ID, job.ID)
	}
	if job.Name != snapshot.Name {
		t.Errorf("Expected Name %s, got %s", snapshot.Name, job.Name)
	}
	if string(job.Payload) != string(snapshot.Payload) {
		t.Errorf("Expected Payload %s, got %s", snapshot.Payload, job.Payload)
	}
	if !job.TriggerAt.Equal(snapshot.TriggerAt) {
		t.Errorf("Expected TriggerAt %v, got %v", snapshot.TriggerAt, job.TriggerAt)
	}
	if job.CronExpr != snapshot.CronExpr {
		t.Errorf("Expected CronExpr %s, got %s", snapshot.CronExpr, job.CronExpr)
	}
	if job.IsRepeat != snapshot.IsRepeat {
		t.Errorf("Expected IsRepeat %v, got %v", snapshot.IsRepeat, job.IsRepeat)
	}
	if job.MaxRetries != snapshot.MaxRetries {
		t.Errorf("Expected MaxRetries %d, got %d", snapshot.MaxRetries, job.MaxRetries)
	}
	if job.RetryCount != snapshot.RetryCount {
		t.Errorf("Expected RetryCount %d, got %d", snapshot.RetryCount, job.RetryCount)
	}
	if job.RetryDelay != time.Duration(snapshot.RetryDelay) {
		t.Errorf("Expected RetryDelay %v, got %v", time.Duration(snapshot.RetryDelay), job.RetryDelay)
	}
	if !job.CreatedAt.Equal(snapshot.CreatedAt) {
		t.Errorf("Expected CreatedAt %v, got %v", snapshot.CreatedAt, job.CreatedAt)
	}
	if !job.UpdatedAt.Equal(snapshot.UpdatedAt) {
		t.Errorf("Expected UpdatedAt %v, got %v", snapshot.UpdatedAt, job.UpdatedAt)
	}
	if job.Attempts != snapshot.Attempts {
		t.Errorf("Expected Attempts %d, got %d", snapshot.Attempts, job.Attempts)
	}

	// Verify status is reset to pending
	if job.Status != StatusPending {
		t.Errorf("Expected Status to be reset to %v, got %v", StatusPending, job.Status)
	}
}

func TestJob_SnapshotRoundTrip(t *testing.T) {
	now := time.Now()
	original := &Job{
		ID:         "roundtrip-job",
		Name:       "Round Trip Test",
		Payload:    []byte("round trip data"),
		TriggerAt:  now,
		CronExpr:   "0 0 * * *",
		IsRepeat:   true,
		MaxRetries: 10,
		RetryCount: 5,
		RetryDelay: 60 * time.Second,
		Status:     StatusSuccess,
		CreatedAt:  now.Add(-3 * time.Hour),
		UpdatedAt:  now.Add(-30 * time.Minute),
		Attempts:   7,
	}

	// Convert to snapshot and back
	snapshot := original.ToSnapshot()
	restored := &Job{}
	restored.FromSnapshot(snapshot)

	// Verify key fields match (except Status which is reset)
	if restored.ID != original.ID {
		t.Errorf("ID mismatch: expected %s, got %s", original.ID, restored.ID)
	}
	if restored.Name != original.Name {
		t.Errorf("Name mismatch: expected %s, got %s", original.Name, restored.Name)
	}
	if string(restored.Payload) != string(original.Payload) {
		t.Errorf("Payload mismatch")
	}
	if !restored.TriggerAt.Equal(original.TriggerAt) {
		t.Errorf("TriggerAt mismatch")
	}
	if restored.RetryDelay != original.RetryDelay {
		t.Errorf("RetryDelay mismatch: expected %v, got %v", original.RetryDelay, restored.RetryDelay)
	}
	if restored.Status != StatusPending {
		t.Errorf("Status should be reset to Pending, got %v", restored.Status)
	}
}

func TestJob_CloneForRetry_ExponentialBackoff(t *testing.T) {
	job := &Job{
		ID:         "backoff-test",
		RetryDelay: 1 * time.Second,
		RetryCount: 0,
	}

	delays := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
	}

	current := job
	for i, expectedDelay := range delays {
		if current.RetryDelay != expectedDelay {
			t.Errorf("Retry %d: expected delay %v, got %v", i, expectedDelay, current.RetryDelay)
		}
		current = current.CloneForRetry(time.Now())
	}
}

func TestJob_EmptyPayload(t *testing.T) {
	job := &Job{
		ID:      "empty-payload-job",
		Payload: nil,
	}

	snapshot := job.ToSnapshot()
	if snapshot.Payload != nil {
		t.Error("Expected nil payload in snapshot")
	}

	restored := &Job{}
	restored.FromSnapshot(snapshot)
	if restored.Payload != nil {
		t.Error("Expected nil payload after restoration")
	}
}

func TestJob_ZeroValues(t *testing.T) {
	job := &Job{}
	
	if job.ID != "" {
		t.Error("Expected empty ID")
	}
	if job.Status != StatusPending {
		t.Errorf("Expected default status to be Pending (0), got %v", job.Status)
	}
	if job.MaxRetries != 0 {
		t.Errorf("Expected MaxRetries to be 0, got %d", job.MaxRetries)
	}
	if job.RetryCount != 0 {
		t.Errorf("Expected RetryCount to be 0, got %d", job.RetryCount)
	}
}
