package api

import (
	"context"
	"sync"
	"testing"

	"godelayq/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewJobRegistry(t *testing.T) {
	registry := NewJobRegistry()

	require.NotNil(t, registry)
	assert.NotNil(t, registry.handlers)
	assert.Empty(t, registry.handlers)
}

func TestJobRegistry_Register(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	registry.Register("test-job", handler)

	assert.Len(t, registry.handlers, 1)
	assert.Contains(t, registry.handlers, "test-job")
}

func TestJobRegistry_Register_Multiple(t *testing.T) {
	registry := NewJobRegistry()

	handler1 := func(ctx context.Context, job *core.Job) error {
		return nil
	}
	handler2 := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	registry.Register("job-1", handler1)
	registry.Register("job-2", handler2)

	assert.Len(t, registry.handlers, 2)
	assert.Contains(t, registry.handlers, "job-1")
	assert.Contains(t, registry.handlers, "job-2")
}

func TestJobRegistry_Register_Overwrite(t *testing.T) {
	registry := NewJobRegistry()

	handler1 := func(ctx context.Context, job *core.Job) error {
		return nil
	}
	handler2 := func(ctx context.Context, job *core.Job) error {
		return assert.AnError
	}

	registry.Register("test-job", handler1)
	registry.Register("test-job", handler2) // Overwrite

	assert.Len(t, registry.handlers, 1)

	// Verify it's the second handler
	h, ok := registry.Get("test-job")
	require.True(t, ok)
	err := h(context.Background(), &core.Job{})
	assert.Error(t, err) // Second handler returns error
}

func TestJobRegistry_Get_Exists(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	registry.Register("test-job", handler)

	h, ok := registry.Get("test-job")
	assert.True(t, ok)
	assert.NotNil(t, h)
}

func TestJobRegistry_Get_NotExists(t *testing.T) {
	registry := NewJobRegistry()

	h, ok := registry.Get("nonexistent")
	assert.False(t, ok)
	assert.Nil(t, h)
}

func TestJobRegistry_List_Empty(t *testing.T) {
	registry := NewJobRegistry()

	names := registry.List()
	assert.Empty(t, names)
}

func TestJobRegistry_List_Multiple(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	registry.Register("job-1", handler)
	registry.Register("job-2", handler)
	registry.Register("job-3", handler)

	names := registry.List()
	assert.Len(t, names, 3)
	assert.Contains(t, names, "job-1")
	assert.Contains(t, names, "job-2")
	assert.Contains(t, names, "job-3")
}

func TestJobRegistry_Concurrent(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	var wg sync.WaitGroup
	numGoroutines := 10

	// Concurrent writes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			registry.Register(string(rune('a'+id)), handler)
		}(i)
	}

	wg.Wait()

	// Concurrent reads
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			registry.Get(string(rune('a' + id)))
			registry.List()
		}(i)
	}

	wg.Wait()

	assert.Len(t, registry.handlers, numGoroutines)
}

func TestJobRegistry_ThreadSafety(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 100; i++ {
			registry.Register("job", handler)
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 100; i++ {
			registry.Get("job")
			registry.List()
		}
		done <- true
	}()

	// Wait for both
	<-done
	<-done

	// Should not panic and should have the handler
	h, ok := registry.Get("job")
	assert.True(t, ok)
	assert.NotNil(t, h)
}

func TestJobRegistry_HandlerExecution(t *testing.T) {
	registry := NewJobRegistry()

	executed := false
	handler := func(ctx context.Context, job *core.Job) error {
		executed = true
		return nil
	}

	registry.Register("exec-test", handler)

	h, ok := registry.Get("exec-test")
	require.True(t, ok)

	err := h(context.Background(), &core.Job{})
	assert.NoError(t, err)
	assert.True(t, executed)
}

func TestJobRegistry_HandlerWithError(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return assert.AnError
	}

	registry.Register("error-test", handler)

	h, ok := registry.Get("error-test")
	require.True(t, ok)

	err := h(context.Background(), &core.Job{})
	assert.Error(t, err)
}

func TestJobRegistry_CaseSensitive(t *testing.T) {
	registry := NewJobRegistry()

	handler1 := func(ctx context.Context, job *core.Job) error {
		return nil
	}
	handler2 := func(ctx context.Context, job *core.Job) error {
		return assert.AnError
	}

	registry.Register("TestJob", handler1)
	registry.Register("testjob", handler2)

	assert.Len(t, registry.handlers, 2)

	h1, ok1 := registry.Get("TestJob")
	assert.True(t, ok1)
	assert.NoError(t, h1(context.Background(), &core.Job{}))

	h2, ok2 := registry.Get("testjob")
	assert.True(t, ok2)
	assert.Error(t, h2(context.Background(), &core.Job{}))
}

func TestJobRegistry_EmptyName(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	// Should allow empty name (though not recommended)
	registry.Register("", handler)

	h, ok := registry.Get("")
	assert.True(t, ok)
	assert.NotNil(t, h)
}

func TestJobRegistry_SpecialCharacters(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	specialNames := []string{
		"job-with-dash",
		"job_with_underscore",
		"job.with.dot",
		"job:with:colon",
		"job/with/slash",
	}

	for _, name := range specialNames {
		registry.Register(name, handler)
	}

	assert.Len(t, registry.handlers, len(specialNames))

	for _, name := range specialNames {
		h, ok := registry.Get(name)
		assert.True(t, ok, "Should find handler for: "+name)
		assert.NotNil(t, h)
	}
}

func TestJobRegistry_LargeNumberOfHandlers(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	numHandlers := 1000
	for i := 0; i < numHandlers; i++ {
		registry.Register(string(rune(i)), handler)
	}

	assert.Len(t, registry.handlers, numHandlers)

	names := registry.List()
	assert.Len(t, names, numHandlers)
}

func TestJobRegistry_NilHandler(t *testing.T) {
	registry := NewJobRegistry()

	// Should allow nil handler (though not recommended)
	registry.Register("nil-handler", nil)

	h, ok := registry.Get("nil-handler")
	assert.True(t, ok)
	assert.Nil(t, h)
}

func TestJobRegistry_ListOrder(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	names := []string{"zebra", "apple", "banana", "cherry"}
	for _, name := range names {
		registry.Register(name, handler)
	}

	result := registry.List()
	assert.Len(t, result, len(names))

	// Verify all names are present (order may vary due to map iteration)
	for _, name := range names {
		assert.Contains(t, result, name)
	}
}

func TestJobRegistry_GetAfterMultipleRegistrations(t *testing.T) {
	registry := NewJobRegistry()

	handler1 := func(ctx context.Context, job *core.Job) error {
		return nil
	}
	handler2 := func(ctx context.Context, job *core.Job) error {
		return assert.AnError
	}

	registry.Register("job-1", handler1)
	registry.Register("job-2", handler2)
	registry.Register("job-3", handler1)

	// Get job-2 specifically
	h, ok := registry.Get("job-2")
	require.True(t, ok)

	err := h(context.Background(), &core.Job{})
	assert.Error(t, err) // Should be handler2 which returns error
}

func TestJobRegistry_ConcurrentRegisterAndList(t *testing.T) {
	registry := NewJobRegistry()

	handler := func(ctx context.Context, job *core.Job) error {
		return nil
	}

	done := make(chan bool, 2)

	// Register goroutine
	go func() {
		for i := 0; i < 50; i++ {
			registry.Register(string(rune('a'+i%26)), handler)
		}
		done <- true
	}()

	// List goroutine
	go func() {
		for i := 0; i < 50; i++ {
			registry.List()
		}
		done <- true
	}()

	<-done
	<-done

	// Should not panic
	names := registry.List()
	assert.NotEmpty(t, names)
}
