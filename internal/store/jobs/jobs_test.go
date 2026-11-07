package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openStore 建一个落在临时目录的任务库，测试结束自动关闭。
func openStore(t *testing.T, opts ...func(*core.StoreSQLiteConfig)) *Store {
	t.Helper()
	cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
	for _, opt := range opts {
		opt(&cfg)
	}
	s, err := Open(cfg, 0, 0, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func snapshot(id string, status core.JobStatus, updatedAt time.Time) core.JobSnapshot {
	return core.JobSnapshot{
		ID:        id,
		Name:      "job-" + id,
		TriggerAt: updatedAt,
		Status:    int(status),
		CreatedAt: updatedAt,
		UpdatedAt: updatedAt,
	}
}

func ids(snapshots []core.JobSnapshot) []string {
	out := make([]string, 0, len(snapshots))
	for _, snap := range snapshots {
		out = append(out, snap.ID)
	}
	sort.Strings(out)
	return out
}

func TestStoreRoundTrip(t *testing.T) {
	s := openStore(t)
	now := time.Now()

	// Save 入口：Job → 快照
	require.NoError(t, s.Save(&core.Job{ID: "j1", Name: "job-j1", TriggerAt: now, Status: core.StatusPending}))
	require.NoError(t, s.Update(snapshot("j2", core.StatusPending, now)))

	loaded, err := s.LoadAll()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"j1", "j2"}, ids(loaded))

	// Update 覆盖同 ID：状态与内容都换掉
	updated := snapshot("j1", core.StatusRunning, now.Add(time.Second))
	require.NoError(t, s.Update(updated))
	loaded, err = s.LoadAll()
	require.NoError(t, err)
	assert.Len(t, loaded, 2)
	for _, snap := range loaded {
		if snap.ID == "j1" {
			assert.Equal(t, core.StatusRunning, core.JobStatus(snap.Status))
			assert.Equal(t, updated.UpdatedAt.UnixNano(), snap.UpdatedAt.UnixNano())
		}
	}

	// Delete：删除缺失的 ID 不是错误
	require.NoError(t, s.Delete("j2"))
	require.NoError(t, s.Delete("j-not-exist"))
	loaded, err = s.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, []string{"j1"}, ids(loaded))
}

func TestStoreFlushIsNoOpAndCloseIdempotent(t *testing.T) {
	cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
	s, err := Open(cfg, 0, 0, nil)
	require.NoError(t, err)

	require.NoError(t, s.Flush()) // 直写下恒为 no-op，任何时刻调用都安全
	require.NoError(t, s.Close())
	require.NoError(t, s.Close()) // 幂等
	require.NoError(t, s.Flush()) // 关闭后同样无害

	// 关闭后再读是错误（连接已断），但绝不能 panic
	_, err = s.LoadAll()
	assert.Error(t, err)
}

func TestStoreHistoryRetention(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-time.Minute)

	t.Run("条数上限保留最新", func(t *testing.T) {
		cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
		s, err := Open(cfg, 2, 0, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		require.NoError(t, s.Update(snapshot("t1", core.StatusSuccess, old)))
		require.NoError(t, s.Update(snapshot("t2", core.StatusSuccess, recent)))
		require.NoError(t, s.Update(snapshot("t3", core.StatusSuccess, now)))
		// pending 永不淘汰，即使超出条数上限
		require.NoError(t, s.Update(snapshot("p1", core.StatusPending, old)))

		loaded, err := s.LoadAll()
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"t2", "t3", "p1"}, ids(loaded))
	})

	t.Run("TTL 淘汰过期终态", func(t *testing.T) {
		cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
		s, err := Open(cfg, 0, time.Hour, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		require.NoError(t, s.Update(snapshot("t1", core.StatusFailed, old)))     // 超时
		require.NoError(t, s.Update(snapshot("t2", core.StatusCancelled, now))) // 未超时
		require.NoError(t, s.Update(snapshot("p1", core.StatusPending, old)))   // 终态才淘汰

		loaded, err := s.LoadAll()
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"t2", "p1"}, ids(loaded))
	})

	t.Run("负数不留痕", func(t *testing.T) {
		cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
		s, err := Open(cfg, -1, 0, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		require.NoError(t, s.Update(snapshot("t1", core.StatusSuccess, now)))
		require.NoError(t, s.Update(snapshot("p1", core.StatusPending, now)))
		loaded, err := s.LoadAll()
		require.NoError(t, err)
		assert.Equal(t, []string{"p1"}, ids(loaded))
	})

	t.Run("运行期调整生效", func(t *testing.T) {
		cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
		s, err := Open(cfg, 100, 0, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		require.NoError(t, s.Update(snapshot("t1", core.StatusSuccess, now)))
		s.SetHistoryRetention(-1, 0) // 下一笔终态写入起不留痕（写入即删除）
		require.NoError(t, s.Update(snapshot("t2", core.StatusFailed, now)))

		// 新策略在 t2 这笔终态写入的 trim 上生效：删光全部终态——包括
		// 旧策略下写入的 t1 与刚落库的 t2 本身。"写入即删除"是字面义。
		loaded, err := s.LoadAll()
		require.NoError(t, err)
		assert.Empty(t, ids(loaded))
	})
}

func TestImportOnce(t *testing.T) {
	newSource := func(t *testing.T, jobs map[string]core.JobSnapshot) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "jobs.json")
		if jobs == nil {
			require.NoError(t, os.WriteFile(path, nil, 0o644))
			return path
		}
		data, err := json.Marshal(jobs)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o644))
		return path
	}

	t.Run("空库加有文件则导入并打标", func(t *testing.T) {
		cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
		s, err := Open(cfg, 0, 0, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		now := time.Now()
		source := newSource(t, map[string]core.JobSnapshot{
			"j1": snapshot("j1", core.StatusSuccess, now),
			"j2": snapshot("j2", core.StatusPending, now),
		})
		require.NoError(t, ImportOnce(s.h, source))

		loaded, err := s.LoadAll()
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"j1", "j2"}, ids(loaded))

		imported, err := alreadyImported(s.h)
		require.NoError(t, err)
		assert.True(t, imported)
	})

	t.Run("已打标后不再导入", func(t *testing.T) {
		cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
		s, err := Open(cfg, 0, 0, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		source := newSource(t, map[string]core.JobSnapshot{"j1": snapshot("j1", core.StatusPending, time.Now())})
		require.NoError(t, ImportOnce(s.h, source))
		// 源文件内容变了也不影响：标记已生效
		source2 := newSource(t, map[string]core.JobSnapshot{"jX": snapshot("jX", core.StatusPending, time.Now())})
		require.NoError(t, ImportOnce(s.h, source2))

		loaded, err := s.LoadAll()
		require.NoError(t, err)
		assert.Equal(t, []string{"j1"}, ids(loaded))
	})

	t.Run("库非空不导入但补标记", func(t *testing.T) {
		cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
		s, err := Open(cfg, 0, 0, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		require.NoError(t, s.Update(snapshot("j1", core.StatusPending, time.Now())))
		source := newSource(t, map[string]core.JobSnapshot{"jX": snapshot("jX", core.StatusPending, time.Now())})
		require.NoError(t, ImportOnce(s.h, source))

		loaded, err := s.LoadAll()
		require.NoError(t, err)
		assert.Equal(t, []string{"j1"}, ids(loaded), "库里有数据就不动它")
	})

	t.Run("无来源文件打空标", func(t *testing.T) {
		cfg := core.StoreSQLiteConfig{Path: filepath.Join(t.TempDir(), "jobs.sqlite")}
		s, err := Open(cfg, 0, 0, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		require.NoError(t, ImportOnce(s.h, filepath.Join(t.TempDir(), "absent.json")))
		imported, err := alreadyImported(s.h)
		require.NoError(t, err)
		assert.True(t, imported)
	})
}

// TestParityWithJSONStore 用同一操作序列喂两个后端，断言可见快照集合一致。
// 留痕语义只有 core.TrimTerminal 一份，这里防的是"语义共享之外"的漂移：
// 触发时机、可见范围、删除的完整性。
func TestParityWithJSONStore(t *testing.T) {
	now := time.Now()
	sequence := []core.JobSnapshot{
		snapshot("a", core.StatusSuccess, now.Add(-3*time.Hour)),
		snapshot("b", core.StatusFailed, now.Add(-2*time.Hour)),
		snapshot("c", core.StatusSuccess, now.Add(-time.Hour)),
		snapshot("d", core.StatusPending, now),
		snapshot("e", core.StatusCancelled, now),
		snapshot("f", core.StatusSuccess, now),
	}

	jsonPath := filepath.Join(t.TempDir(), "jobs.json")
	jsonStore, err := core.NewJSONFileStoreWithOptions(jsonPath, core.StoreOptions{HistoryLimit: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = jsonStore.Close() })

	jobsStore := openStore(t, func(cfg *core.StoreSQLiteConfig) {})
	// openStore 用默认留痕（1000），这里重设成与 JSON 侧相同
	jobsStore.SetHistoryRetention(2, 0)

	for _, snap := range sequence {
		require.NoError(t, jsonStore.Update(snap))
		require.NoError(t, jobsStore.Update(snap))
	}

	jsonLoaded, err := jsonStore.LoadAll()
	require.NoError(t, err)
	jobsLoaded, err := jobsStore.LoadAll()
	require.NoError(t, err)
	assert.Equal(t, ids(jsonLoaded), ids(jobsLoaded))
}
