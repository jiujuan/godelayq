package core

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateIDIsUUIDv7 ID 必须是 UUIDv7：时间有序、随机位来自 crypto/rand。
func TestGenerateIDIsUUIDv7(t *testing.T) {
	id := generateID()

	parsed, err := uuid.Parse(id)
	require.NoError(t, err, "generated id must be a valid UUID: %q", id)
	assert.Equal(t, 36, len(id))
	assert.Equal(t, id, strings.ToLower(id), "text form must stay lowercase")
	assert.Equal(t, uuid.Version(7), parsed.Version(), "id must carry a millisecond timestamp prefix")
	assert.Equal(t, uuid.RFC4122, parsed.Variant())
}

// TestGenerateIDUniqueUnderBurst 是旧实现真实丢任务的场景：
// 同一瞬间连续生成也不能出现重复。
func TestGenerateIDUniqueUnderBurst(t *testing.T) {
	const count = 50000

	seen := make(map[string]struct{}, count)
	for i := 0; i < count; i++ {
		id := generateID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q generated at iteration %d", id, i)
		}
		seen[id] = struct{}{}
	}

	assert.Len(t, seen, count)
}

// TestGenerateIDTimeOrdered 字符串序即时间序，便于按 ID 排序查看历史。
func TestGenerateIDTimeOrdered(t *testing.T) {
	first := generateID()
	last := generateID()
	for i := 0; i < 200; i++ {
		last = generateID()
	}

	assert.True(t, first < last, "expected %s to sort before %s", first, last)
}

// TestScheduleAssignsUUIDv7ID 未指定 ID 的任务在入队时拿到合法 UUIDv7。
func TestScheduleAssignsUUIDv7ID(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	job := &Job{Name: "task", TriggerAt: time.Now()}

	require.NoError(t, scheduler.Schedule(job))

	parsed, err := uuid.Parse(job.ID)
	require.NoError(t, err, "job id %q must be a UUID", job.ID)
	assert.Equal(t, uuid.Version(7), parsed.Version())
}

// TestEventBusSubscriptionIDsAreUnique 订阅 ID 同样来自 generateID，
// 碰撞会让 Unsubscribe 误删他人订阅。
func TestEventBusSubscriptionIDsAreUnique(t *testing.T) {
	bus := NewEventBus(1)

	ids := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		id, ch := bus.Subscribe(EventJobScheduled)
		if _, dup := ids[id]; dup {
			t.Fatalf("duplicate subscription id %q", id)
		}
		ids[id] = struct{}{}
		bus.Unsubscribe(id)
		_ = ch
	}

	assert.Len(t, ids, 200)
}
