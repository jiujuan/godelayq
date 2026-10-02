package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newHistoryStore(t *testing.T, opts StoreOptions) *JSONFileStore {
	t.Helper()

	store, err := NewJSONFileStoreWithOptions(filepath.Join(t.TempDir(), "jobs.json"), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	return store
}

func terminalSnapshot(id string, status JobStatus, at time.Time) JobSnapshot {
	return JobSnapshot{ID: id, Name: "task", Status: int(status), UpdatedAt: at}
}

func snapshotIDs(snapshots []JobSnapshot) map[string]bool {
	ids := make(map[string]bool, len(snapshots))
	for _, snap := range snapshots {
		ids[snap.ID] = true
	}
	return ids
}

// TestJSONFileStore_HistoryLimitKeepsNewestTerminal 终态留痕按条数淘汰最旧的，
// 未完成任务永不受影响。
func TestJSONFileStore_HistoryLimitKeepsNewestTerminal(t *testing.T) {
	store := newHistoryStore(t, StoreOptions{HistoryLimit: 2})
	base := time.Now()

	require.NoError(t, store.Save(&Job{ID: "waiting", Name: "task", Status: StatusPending, TriggerAt: base.Add(time.Hour)}))
	for i := 0; i < 5; i++ {
		require.NoError(t, store.Update(terminalSnapshot(
			string(rune('a'+i)), StatusSuccess, base.Add(time.Duration(i)*time.Minute))))
	}

	ids := snapshotIDs(mustLoadAll(t, store))
	assert.Len(t, ids, 3, "two newest terminal records plus the pending job")
	assert.True(t, ids["waiting"], "pending jobs must never be trimmed")
	assert.True(t, ids["e"])
	assert.True(t, ids["d"])
	assert.False(t, ids["a"])
	assert.False(t, ids["b"])
	assert.False(t, ids["c"])
}

// TestJSONFileStore_HistoryTTLExpiresTerminal 超过保留时长的终态记录被清掉。
func TestJSONFileStore_HistoryTTLExpiresTerminal(t *testing.T) {
	store := newHistoryStore(t, StoreOptions{HistoryTTL: time.Hour})
	now := time.Now()

	require.NoError(t, store.Update(terminalSnapshot("stale", StatusFailed, now.Add(-2*time.Hour))))
	require.NoError(t, store.Update(terminalSnapshot("fresh", StatusSuccess, now.Add(-time.Minute))))
	require.NoError(t, store.Update(terminalSnapshot("ancient-pending", StatusPending, now.Add(-30*24*time.Hour))))

	ids := snapshotIDs(mustLoadAll(t, store))
	assert.False(t, ids["stale"])
	assert.True(t, ids["fresh"])
	assert.True(t, ids["ancient-pending"], "the TTL only applies to terminal records")
}

// TestJSONFileStore_HistoryLimitOffDropsTerminalImmediately 负数即关闭留痕。
func TestJSONFileStore_HistoryLimitOffDropsTerminalImmediately(t *testing.T) {
	store := newHistoryStore(t, StoreOptions{HistoryLimit: -1})
	now := time.Now()

	require.NoError(t, store.Save(&Job{ID: "waiting", Name: "task", Status: StatusPending, TriggerAt: now.Add(time.Hour)}))
	require.NoError(t, store.Update(terminalSnapshot("done", StatusSuccess, now)))

	ids := snapshotIDs(mustLoadAll(t, store))
	assert.False(t, ids["done"], "a store without history must not keep terminal records")
	assert.True(t, ids["waiting"])
}

func TestJSONFileStore_ZeroHistoryLimitUsesDefault(t *testing.T) {
	store := newHistoryStore(t, StoreOptions{})

	// 两个策略位现在是原子字段（重载链会运行期写它们），断言改成读 Load，
	// 长度与补齐口径一条都没变。
	assert.Equal(t, int64(DefaultHistoryLimit), store.historyLimit.Load())
	assert.Equal(t, DefaultFlushInterval, store.interval)
	assert.Zero(t, store.historyTTL.Load())
}

func mustLoadAll(t *testing.T, store *JSONFileStore) []JobSnapshot {
	t.Helper()

	snapshots, err := store.LoadAll()
	require.NoError(t, err)

	return snapshots
}
