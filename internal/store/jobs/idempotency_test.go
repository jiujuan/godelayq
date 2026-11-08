package jobs

import (
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/require"
)

// backdateIdempotency 把一条登记的 created_at 拨到指定时刻：直写模型没有内存
// 缓存，测试直接改表即可模拟"登记写于 TTL 之前"。
func (s *Store) backdateIdempotency(key string, at time.Time) error {
	_, err := s.h.NewUpdate().Model((*idempotencyRow)(nil)).
		Set("created_at = ?", at.UnixNano()).
		Where("key = ?", key).
		Exec(bgCtx)
	return err
}

// idempotency_test.go 是幂等登记表的行为断言（TASK-RE03）；SQL 形状由快照钉住。

func TestIdempotencyRegistry(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)

		jobID, fingerprint, found, err := s.LookupIdempotency("order-1")
		require.NoError(t, err)
		require.False(t, found, "空表查键是未命中")

		require.NoError(t, s.PutIdempotency("order-1", "job-1", "fp-a"))
		jobID, fingerprint, found, err = s.LookupIdempotency("order-1")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "job-1", jobID)
		require.Equal(t, "fp-a", fingerprint)

		// 先到先得：同键再登记不改写（并发首提交的窗口里让先到者赢）
		require.NoError(t, s.PutIdempotency("order-1", "job-2", "fp-b"))
		jobID, fingerprint, found, err = s.LookupIdempotency("order-1")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "job-1", jobID)
		require.Equal(t, "fp-a", fingerprint)
	})
}

func TestIdempotencyPrune(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)

		require.NoError(t, s.PutIdempotency("old", "job-old", "fp-old"))
		// 把 created_at 手工拨回 TTL 之外：直写模型下没有内存缓存，直接改表
		require.NoError(t, s.backdateIdempotency("old", time.Now().Add(-25*time.Hour)))
		require.NoError(t, s.PutIdempotency("fresh", "job-fresh", "fp-fresh"))

		require.NoError(t, s.PruneExpiredIdempotency(24 * time.Hour))

		_, _, found, err := s.LookupIdempotency("old")
		require.NoError(t, err)
		require.False(t, found, "过期登记必须被淘汰")

		_, _, found, err = s.LookupIdempotency("fresh")
		require.NoError(t, err)
		require.True(t, found, "未过期的登记不受淘汰影响")
	})
}

func TestSnapshotReaderGet(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		now := time.Now()

		require.NoError(t, s.Update(snapshot("get-1", core.StatusPending, now)))

		snap, found, err := s.Get("get-1")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "get-1", snap.ID)

		_, found, err = s.Get("get-none")
		require.NoError(t, err)
		require.False(t, found, "不存在的 ID 是未命中，不是错误")
	})
}
