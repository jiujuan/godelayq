package core

import (
	"errors"
	"testing"
	"time"
)

// newUpdateTestScheduler 构造一个未启动执行循环的调度器，只验证堆与存储的原地改动。
func newUpdateTestScheduler(t *testing.T) (*Scheduler, *mockStore) {
	t.Helper()

	store := newMockStore()
	return NewScheduler(store, nil, NewEventBus(8)), store
}

func TestScheduler_UpdatePendingReordersHeapPosition(t *testing.T) {
	scheduler, store := newUpdateTestScheduler(t)

	if err := scheduler.Schedule(&Job{ID: "early", Name: "task", TriggerAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}
	if err := scheduler.Schedule(&Job{ID: "late", Name: "task", TriggerAt: time.Now().Add(time.Hour), Payload: []byte("old")}); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}
	if top := scheduler.heap.Peek(); top.GetID() != "early" {
		t.Fatalf("Expected early to be on top, got %s", top.GetID())
	}

	deadline := time.Now().Add(10 * time.Second)
	updated, err := scheduler.UpdatePending("late", func(job *Job) error {
		job.TriggerAt = deadline
		job.Payload = []byte("new")
		job.MaxRetries = 7
		return nil
	})
	if err != nil {
		t.Fatalf("UpdatePending failed: %v", err)
	}

	if updated.ID != "late" {
		t.Errorf("Expected the same job id, got %s", updated.ID)
	}
	if string(updated.Payload) != "new" || updated.MaxRetries != 7 {
		t.Errorf("Expected new payload and retries, got %q / %d", updated.Payload, updated.MaxRetries)
	}
	if !updated.TriggerAt.Equal(deadline) {
		t.Errorf("Expected trigger %v, got %v", deadline, updated.TriggerAt)
	}

	// 更早的条目必须上浮，且原来的堆顶不受影响
	if top := scheduler.heap.Peek(); top.GetID() != "late" {
		t.Errorf("Expected updated job to float to the top, got %s", top.GetID())
	}

	snapshot, ok := store.jobs["late"]
	if !ok {
		t.Fatal("Expected the updated snapshot to be persisted")
	}
	if string(snapshot.Payload) != "new" || snapshot.MaxRetries != 7 {
		t.Errorf("Expected persisted payload/retries to match, got %q / %d", snapshot.Payload, snapshot.MaxRetries)
	}
	if scheduler.heap.Len() != 2 {
		t.Errorf("Expected an in-place update to keep the heap size, got %d", scheduler.heap.Len())
	}
}

func TestScheduler_UpdatePendingKeepsUntouchedFields(t *testing.T) {
	scheduler, _ := newUpdateTestScheduler(t)

	createdAt := time.Now().Add(-3 * time.Hour)
	if err := scheduler.Schedule(&Job{
		ID:        "keep",
		Name:      "nightly",
		Type:      "report",
		CreatedAt: createdAt,
		TriggerAt: time.Now().Add(time.Hour),
		Payload:   []byte("payload"),
		CronExpr:  "0 3 * * *",
		IsRepeat:  true,
	}); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}

	updated, err := scheduler.UpdatePending("keep", func(job *Job) error {
		job.Payload = []byte("changed")
		return nil
	})
	if err != nil {
		t.Fatalf("UpdatePending failed: %v", err)
	}

	if updated.Name != "nightly" || updated.Type != "report" || updated.CronExpr != "0 3 * * *" || !updated.IsRepeat {
		t.Errorf("Expected identity and cron fields preserved, got %+v", updated)
	}
	if !updated.CreatedAt.Equal(createdAt) {
		t.Errorf("Expected CreatedAt preserved, got %v", updated.CreatedAt)
	}
	if updated.Status != StatusPending {
		t.Errorf("Expected status pending, got %v", updated.Status)
	}
}

func TestScheduler_UpdatePendingNotFound(t *testing.T) {
	scheduler, _ := newUpdateTestScheduler(t)

	if _, err := scheduler.UpdatePending("missing", nil); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Expected ErrJobNotFound, got %v", err)
	}
}

// 任务已出堆正在执行时，原地更新必须显式失败，而不是把改动写进一个没人看的副本。
func TestScheduler_UpdatePendingRejectsDispatchedJob(t *testing.T) {
	scheduler, store := newUpdateTestScheduler(t)

	if err := scheduler.Schedule(&Job{ID: "fired", Name: "task", TriggerAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}
	if popTop(scheduler.heap) == nil {
		t.Fatal("Expected to pop the job")
	}

	if _, err := scheduler.UpdatePending("fired", func(job *Job) error {
		job.Payload = []byte("too late")
		return nil
	}); !errors.Is(err, ErrJobNotPending) {
		t.Fatalf("Expected ErrJobNotPending, got %v", err)
	}
	stored, ok := store.jobs["fired"]
	if !ok {
		t.Fatal("Expected the original schedule record to remain")
	}
	if string(stored.Payload) != "" {
		t.Errorf("Expected no update written, got payload %q", stored.Payload)
	}
}

// apply 报错时不能留下半个改动，堆里的条目与存储快照都保持原样。
func TestScheduler_UpdatePendingRollsBackOnApplyError(t *testing.T) {
	scheduler, store := newUpdateTestScheduler(t)

	boom := errors.New("boom")
	if err := scheduler.Schedule(&Job{ID: "safe", Name: "task", TriggerAt: time.Now().Add(time.Hour), Payload: []byte("original"), MaxRetries: 2}); err != nil {
		t.Fatalf("Schedule failed: %v", err)
	}
	before, _ := store.LoadAll()

	if _, err := scheduler.UpdatePending("safe", func(job *Job) error {
		job.Payload = []byte("mutated")
		job.MaxRetries = 99
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Expected apply error to surface, got %v", err)
	}

	current := scheduler.heap.Get("safe").(*Job)
	if string(current.Payload) != "original" || current.MaxRetries != 2 {
		t.Errorf("Expected the heap entry untouched, got %q / %d", current.Payload, current.MaxRetries)
	}
	after, _ := store.LoadAll()
	if len(before) != len(after) || string(after[0].Payload) != "original" {
		t.Errorf("Expected the stored snapshot untouched, got %+v", after)
	}
}
