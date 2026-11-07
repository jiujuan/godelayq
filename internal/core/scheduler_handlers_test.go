package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSchedulerWithStore(t *testing.T) *Scheduler {
	t.Helper()

	store, err := NewJSONFileStore(t.TempDir() + "/jobs.json")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	return NewScheduler(store, nil, nil)
}

func TestScheduler_LookupHandler(t *testing.T) {
	scheduler := newTestSchedulerWithStore(t)

	noop := func(ctx context.Context, job *Job) error { return nil }
	scheduler.RegisterHandler("payment_check", noop)

	h, ok := scheduler.LookupHandler("payment_check")
	require.True(t, ok)
	assert.NotNil(t, h)

	_, ok = scheduler.LookupHandler("not_registered")
	assert.False(t, ok)

	// 同名重复注册覆盖旧值
	called := ""
	scheduler.RegisterHandler("payment_check", func(ctx context.Context, job *Job) error {
		called = "second"
		return nil
	})
	h, ok = scheduler.LookupHandler("payment_check")
	require.True(t, ok)
	require.NoError(t, h(context.Background(), &Job{}))
	assert.Equal(t, "second", called)
}

// 注册表键与执行侧一致：Type 优先，回退 Name
func TestScheduler_HandlerKeyFollowsHandlerKey(t *testing.T) {
	scheduler := newTestSchedulerWithStore(t)
	scheduler.RegisterHandler("email", func(ctx context.Context, job *Job) error { return nil })

	byName := &Job{Name: "email"}
	byType := &Job{Name: "别名", Type: "email"}
	shadowed := &Job{Name: "email", Type: "other"}

	assert.Equal(t, "email", byName.HandlerKey())
	assert.Equal(t, "email", byType.HandlerKey())
	assert.Equal(t, "other", shadowed.HandlerKey())

	for _, job := range []*Job{byName, byType} {
		_, ok := scheduler.LookupHandler(job.HandlerKey())
		assert.True(t, ok, "key %q should resolve", job.HandlerKey())
	}
	_, ok := scheduler.LookupHandler(shadowed.HandlerKey())
	assert.False(t, ok, "Type 存在时 Name 不再参与查找")
}

func TestScheduler_HandlerNames(t *testing.T) {
	scheduler := newTestSchedulerWithStore(t)
	assert.Empty(t, scheduler.HandlerNames())

	noop := func(ctx context.Context, job *Job) error { return nil }
	scheduler.RegisterHandler("report_generate", noop)
	scheduler.RegisterHandler("payment_check", noop)
	scheduler.RegisterHandler("email_send", noop)

	// 输出有序，接口返回值可预期
	assert.Equal(t, []string{"email_send", "payment_check", "report_generate"}, scheduler.HandlerNames())

	// 返回值是快照，注册表后续变化不影响已取出的列表
	names := scheduler.HandlerNames()
	scheduler.RegisterHandler("data_sync", noop)
	assert.Len(t, names, 3)
	assert.Len(t, scheduler.HandlerNames(), 4)
}

// 并发注册/查找/列举，需在 -race 下通过
func TestScheduler_HandlerRegistryConcurrent(t *testing.T) {
	scheduler := newTestSchedulerWithStore(t)

	noop := func(ctx context.Context, job *Job) error { return nil }
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		key := "handler-" + string(rune('a'+i%26))

		wg.Add(3)
		go func() {
			defer wg.Done()
			scheduler.RegisterHandler(key, noop)
		}()
		go func() {
			defer wg.Done()
			scheduler.LookupHandler(key)
		}()
		go func() {
			defer wg.Done()
			scheduler.HandlerNames()
		}()
	}
	wg.Wait()

	assert.Len(t, scheduler.HandlerNames(), 26)
}
