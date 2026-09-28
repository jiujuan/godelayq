package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWatcherLoader 启动带实时监控的加载器，返回其调度器以便断言
func newWatcherLoader(t *testing.T, dir string) (*DirectoryLoader, *Scheduler) {
	t.Helper()

	store, err := NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	scheduler := NewScheduler(store, nil, nil)
	loader, err := NewDirectoryLoader(scheduler, LoaderOptions{
		Dir:            dir,
		PostLoadAction: KeepAfterLoad,
		EnableWatcher:  true,
	})
	require.NoError(t, err)
	require.NoError(t, loader.Start())
	t.Cleanup(loader.Stop)

	return loader, scheduler
}

func waitForJobs(t *testing.T, scheduler *Scheduler, want int) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if scheduler.HeapLen() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d job(s) in heap, got %d", want, scheduler.HeapLen())
}

func writeJobFile(t *testing.T, path, id string, delay time.Duration) {
	t.Helper()

	data, err := json.Marshal(FileJobFormat{ID: id, Name: "watched", Delay: delay.String()})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

// 事件循环不再串行 Sleep：新文件在静默窗口后被加载
func TestDirectoryLoader_WatcherLoadsFileAfterDebounce(t *testing.T) {
	dir := t.TempDir()
	_, scheduler := newWatcherLoader(t, dir)

	writeJobFile(t, filepath.Join(dir, "watch-1.json"), "watch-1", time.Minute)
	waitForJobs(t, scheduler, 1)

	snapshots, err := scheduler.store.LoadAll()
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "watch-1", snapshots[0].ID)
}

// 同一文件在窗口内被反复写入时，只加载最后一次内容；
// 旧实现逐个事件串行 Sleep，第二次事件会被 processedFiles 去重挡掉，读到的是旧内容。
func TestDirectoryLoader_WatcherUsesLastWrite(t *testing.T) {
	dir := t.TempDir()
	loader, scheduler := newWatcherLoader(t, dir)

	path := filepath.Join(dir, "rewrite.json")
	writeJobFile(t, path, "first", time.Minute)
	writeJobFile(t, path, "second", time.Hour)

	// 期间不应有加载（静默窗口未过）
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, 0, scheduler.HeapLen(), "load must wait for the debounce window")

	waitForJobs(t, scheduler, 1)

	snapshots, err := scheduler.store.LoadAll()
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "second", snapshots[0].ID, "debounce 之后应读到最后一次写入的内容")

	loader.mu.Lock()
	defer loader.mu.Unlock()
	assert.Empty(t, loader.pendingLoads, "触发后定时器记录必须摘除")
}

// 关停时未触发的 debounce 加载要被取消
func TestDirectoryLoader_StopCancelsPendingLoads(t *testing.T) {
	dir := t.TempDir()

	store, err := NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	scheduler := NewScheduler(store, nil, nil)
	loader, err := NewDirectoryLoader(scheduler, LoaderOptions{Dir: dir, PostLoadAction: KeepAfterLoad})
	require.NoError(t, err)

	path := filepath.Join(dir, "pending.json")
	writeJobFile(t, path, "pending-1", time.Minute)

	loader.scheduleLoad(path)
	loader.mu.Lock()
	require.Len(t, loader.pendingLoads, 1)
	loader.mu.Unlock()

	loader.Stop()

	time.Sleep(loaderDebounceInterval + 200*time.Millisecond)
	assert.Equal(t, 0, scheduler.HeapLen(), "Stop 之后不应再加载")

	loader.mu.Lock()
	defer loader.mu.Unlock()
	assert.Empty(t, loader.pendingLoads)
}

// 多次事件只排一次队：定时器被 Reset 而不是新建
func TestDirectoryLoader_ScheduleLoadMergesEvents(t *testing.T) {
	dir := t.TempDir()

	store, err := NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	loader, err := NewDirectoryLoader(NewScheduler(store, nil, nil), LoaderOptions{Dir: dir})
	require.NoError(t, err)
	defer loader.Stop()

	path := filepath.Join(dir, "merge.json")
	writeJobFile(t, path, "merge-1", time.Minute)

	for i := 0; i < 5; i++ {
		loader.scheduleLoad(path)
	}

	loader.mu.Lock()
	defer loader.mu.Unlock()
	require.Len(t, loader.pendingLoads, 1)
}
