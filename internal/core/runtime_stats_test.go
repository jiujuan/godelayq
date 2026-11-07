package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScheduler_RuntimeStats(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(4)

	idle := scheduler.RuntimeStats()
	assert.False(t, idle.Started)
	assert.Equal(t, 4, idle.Workers)
	assert.Equal(t, 4, idle.QueueCapacity, "未配置队列容量时与 worker 数相等")

	scheduler.SetQueueCapacity(9)
	scheduler.RegisterHandler("slow", func(ctx context.Context, job *Job) error {
		<-ctx.Done()
		return nil
	})
	require.NoError(t, scheduler.Schedule(&Job{ID: "rs-1", Name: "slow", Type: "slow", TriggerAt: time.Now().Add(time.Hour)}))

	scheduler.Start()
	defer scheduler.Stop()

	stats := scheduler.RuntimeStats()
	assert.True(t, stats.Started)
	assert.Equal(t, 9, stats.QueueCapacity)
	assert.Equal(t, 1, stats.HeapSize)

	scheduler.Suspend()
	assert.True(t, scheduler.RuntimeStats().Suspended)

	scheduler.Unsuspend()
	assert.False(t, scheduler.RuntimeStats().Suspended)
}
