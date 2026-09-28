package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// historyBase 是事件时间戳的固定起点：窗口顺序靠时间戳判定，
// 用真实时钟的话断言"第几条"要依赖纳秒取模，读起来像在猜。
var historyBase = time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)

// historyIndex 把事件时间戳还原成写入时的序号。
func historyIndex(event core.Event) int {
	return int(event.Timestamp.Sub(historyBase) / time.Millisecond)
}

func historyEvent(jobID string, index int) core.Event {
	return core.Event{
		Type:      core.EventJobStarted,
		JobID:     jobID,
		JobName:   "history-job",
		Status:    core.StatusRunning,
		Timestamp: historyBase.Add(time.Duration(index) * time.Millisecond),
	}
}

func newTestHistory() (*core.EventBus, *EventHistory) {
	bus := core.NewEventBus(100)
	return bus, NewEventHistory(bus)
}

// waitForHistory 等 drain 协程把发布出去的事件收进缓冲。
// 记录器是异步的，断言前必须等它跟上，否则测试会在竞态里随机失败。
func waitForHistory(t *testing.T, h *EventHistory, jobID string, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.Events(jobID, 0)) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("在 2s 内没有收到 %d 条事件（job_id=%s）", want, jobID)
}

func TestEventHistory_RecordsBusEventsInOrder(t *testing.T) {
	bus, history := newTestHistory()
	t.Cleanup(history.Stop)

	for i := 0; i < 3; i++ {
		bus.Publish(historyEvent("job-a", i))
	}
	waitForHistory(t, history, "job-a", 3)

	got := history.Events("job-a", 0)
	require.Len(t, got, 3)
	for i, event := range got {
		assert.Equal(t, i, historyIndex(event), "窗口必须按事件时间升序")
	}
	assert.Equal(t, core.EventJobStarted, got[0].Type)
}

func TestEventHistory_EventsIgnoreOtherJobs(t *testing.T) {
	bus, history := newTestHistory()
	t.Cleanup(history.Stop)

	bus.Publish(historyEvent("job-a", 0))
	bus.Publish(historyEvent("job-b", 0))
	waitForHistory(t, history, "job-a", 1)
	waitForHistory(t, history, "job-b", 1)

	got := history.Events("job-a", 0)
	require.Len(t, got, 1)
	assert.Equal(t, "job-a", got[0].JobID)

	assert.Empty(t, history.Events("job-missing", 0))
}

func TestEventHistory_LimitReturnsTheNewest(t *testing.T) {
	bus, history := newTestHistory()
	t.Cleanup(history.Stop)

	for i := 0; i < 5; i++ {
		bus.Publish(historyEvent("job-a", i))
	}
	waitForHistory(t, history, "job-a", 5)

	got := history.Events("job-a", 2)
	require.Len(t, got, 2)
	assert.Equal(t, 3, historyIndex(got[0]))
	assert.Equal(t, 4, historyIndex(got[1]))
}

func TestEventHistory_SkipsEventsWithoutJobID(t *testing.T) {
	_, history := newTestHistory()
	t.Cleanup(history.Stop)

	// heap.updated 这类事件不带任务归属，塞进某个任务的窗口只会污染时间线
	history.record(core.Event{Type: core.EventHeapUpdate, Timestamp: historyBase})

	assert.Empty(t, history.Events("", 0))
	assert.Empty(t, history.Recent(0))
	assert.Zero(t, history.Stats().Events)
}

// 窗口裁剪用直接写入来测：走总线要发上千条，而 Publish 缓冲满即丢，
// 断言会变成竞态掷硬币。
func TestEventHistory_PerJobWindowKeepsTheNewest(t *testing.T) {
	_, history := newTestHistory()
	t.Cleanup(history.Stop)

	for i := 0; i < historyPerJobLimit+5; i++ {
		history.record(historyEvent("job-a", i))
	}

	got := history.Events("job-a", 0)
	require.Len(t, got, historyPerJobLimit)
	assert.Equal(t, 5, historyIndex(got[0]), "超出的部分丢最前面的")
	assert.Equal(t, historyPerJobLimit+4, historyIndex(got[historyPerJobLimit-1]))
	assert.Equal(t, historyPerJobLimit, history.Stats().Events)
}

func TestEventHistory_GlobalWindowKeepsTheNewest(t *testing.T) {
	_, history := newTestHistory()
	t.Cleanup(history.Stop)

	for i := 0; i < historyGlobalLimit+10; i++ {
		history.record(historyEvent(fmt.Sprintf("job-%d", i%2), i))
	}

	got := history.Recent(0)
	require.Len(t, got, historyGlobalLimit)
	assert.Equal(t, 10, historyIndex(got[0]))
}

func TestEventHistory_RecentLimitTrimsToTheNewest(t *testing.T) {
	_, history := newTestHistory()
	t.Cleanup(history.Stop)

	for i := 0; i < 10; i++ {
		history.record(historyEvent("job-a", i))
	}

	got := history.Recent(3)
	require.Len(t, got, 3)
	assert.Equal(t, 7, historyIndex(got[0]))
}

func TestEventHistory_EvictsLeastRecentlyRecordedJobs(t *testing.T) {
	_, history := newTestHistory()
	t.Cleanup(history.Stop)

	for i := 0; i <= historyMaxJobs; i++ {
		history.record(historyEvent(fmt.Sprintf("job-%d", i), i))
	}

	stats := history.Stats()
	assert.Equal(t, historyMaxJobs, stats.Jobs, "任务数上限就是淘汰阈值，不该继续涨")
	assert.Equal(t, historyMaxJobs, stats.Events)
	assert.Empty(t, history.Events("job-0", 0), "最早记录的任务应被整体淘汰")
	assert.Len(t, history.Events(fmt.Sprintf("job-%d", historyMaxJobs), 0), 1)
}

func TestEventHistory_ClearDropsEveryWindow(t *testing.T) {
	_, history := newTestHistory()
	t.Cleanup(history.Stop)

	for i := 0; i < 4; i++ {
		history.record(historyEvent("job-a", i))
	}

	assert.Equal(t, 4, history.Clear())
	assert.Empty(t, history.Events("job-a", 0))
	assert.Empty(t, history.Recent(0))
	assert.Zero(t, history.Stats().Jobs)
	assert.Zero(t, history.Clear(), "空缓冲清空是幂等的，返回 0")
}

func TestEventHistory_StopUnsubscribes(t *testing.T) {
	bus, history := newTestHistory()

	history.Stop()
	// 订阅撤掉之后再发布不能 panic，也不该再被记录
	bus.Publish(historyEvent("job-a", 0))
	history.Stop()

	assert.Empty(t, history.Events("job-a", 0))
}
