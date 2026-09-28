package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"godelayq/core"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sseStream 打开一条 SSE 长连接，把 data 帧解析成事件送到 channel。
type sseStream struct {
	events chan core.Event
	body   io.Closer
}

func openSSE(t *testing.T, url string) *sseStream {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	events := make(chan core.Event, 64)
	reader := bufio.NewReader(resp.Body)
	go func() {
		defer close(events)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event core.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				continue
			}
			select {
			case events <- event:
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()

	t.Cleanup(func() { _ = resp.Body.Close() })

	return &sseStream{events: events, body: resp.Body}
}

// publishUntil 每隔 interval 发布一批事件，直到返回的停止函数被调用。
func publishUntil(t *testing.T, bus *core.EventBus, interval time.Duration, events ...core.Event) func() {
	t.Helper()

	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				for _, event := range events {
					bus.Publish(event)
				}
			}
		}
	}()

	return func() {
		close(done)
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("publisher did not stop in time")
		}
	}
}

// collect 在窗口内收集事件，至少等到一个为止。
func collect(t *testing.T, stream *sseStream, want int, window time.Duration) []core.Event {
	t.Helper()

	var got []core.Event
	deadline := time.After(window)

	for len(got) < want {
		select {
		case event, ok := <-stream.events:
			if !ok {
				return got
			}
			got = append(got, event)
		case <-deadline:
			return got
		}
	}

	// 再排空窗口内已经到达的事件，便于断言没有多余类型漏出来
	drain := time.After(150 * time.Millisecond)
	for {
		select {
		case event, ok := <-stream.events:
			if !ok {
				return got
			}
			got = append(got, event)
		case <-drain:
			return got
		}
	}
}

func scheduledEvent(jobName string) core.Event {
	return core.Event{
		Type:      core.EventJobScheduled,
		JobID:     "job-" + jobName,
		JobName:   jobName,
		Timestamp: time.Now(),
	}
}

func failedEvent(jobName string) core.Event {
	return core.Event{
		Type:      core.EventJobFailed,
		JobID:     "job-" + jobName,
		JobName:   jobName,
		Timestamp: time.Now(),
	}
}

func TestParseSSEEventTypes(t *testing.T) {
	t.Run("empty means no type filter", func(t *testing.T) {
		types, err := parseSSEEventTypes(nil)
		require.NoError(t, err)
		assert.Empty(t, types)

		types, err = parseSSEEventTypes([]string{"", "  "})
		require.NoError(t, err)
		assert.Empty(t, types)
	})

	t.Run("repeated and comma separated forms", func(t *testing.T) {
		types, err := parseSSEEventTypes([]string{"job.failed", "job.completed,job.started"})
		require.NoError(t, err)
		assert.Equal(t, []core.EventType{core.EventJobStarted, core.EventJobCompleted, core.EventJobFailed}, types,
			"结果按已知类型固定顺序去重，避免同一通道重复注册")
	})

	t.Run("duplicates collapse to one", func(t *testing.T) {
		types, err := parseSSEEventTypes([]string{"job.failed,job.failed", "job.failed"})
		require.NoError(t, err)
		assert.Equal(t, []core.EventType{core.EventJobFailed}, types)
	})

	t.Run("unknown type is rejected", func(t *testing.T) {
		_, err := parseSSEEventTypes([]string{"job.exploded"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "job.exploded")
		assert.Contains(t, err.Error(), "supported")
	})
}

// TestSSESubscribesOnlyRequestedEventTypes 覆盖 #14：event_types 必须真实生效。
func TestSSESubscribesOnlyRequestedEventTypes(t *testing.T) {
	srv, baseURL := newStartedServer(t)
	defer srv.Stop(context.Background())

	stream := openSSE(t, baseURL+"/sse/events?event_types=job.failed")

	stop := publishUntil(t, srv.scheduler.GetEventBus(), 10*time.Millisecond,
		scheduledEvent("noise"), failedEvent("boom"))
	defer stop()

	got := collect(t, stream, 3, 2*time.Second)
	require.NotEmpty(t, got, "the requested event type must reach the client")
	for _, event := range got {
		assert.Equal(t, core.EventJobFailed, event.Type, "unrequested types must not be streamed")
	}
}

// TestSSEFiltersByJobType 覆盖 #14：job_types 按事件的 JobName 过滤。
func TestSSEFiltersByJobType(t *testing.T) {
	srv, baseURL := newStartedServer(t)
	defer srv.Stop(context.Background())

	stream := openSSE(t, baseURL+"/sse/events?job_types=important")

	stop := publishUntil(t, srv.scheduler.GetEventBus(), 10*time.Millisecond,
		scheduledEvent("ignored"), scheduledEvent("important"))
	defer stop()

	got := collect(t, stream, 3, 2*time.Second)
	require.NotEmpty(t, got, "the requested job type must reach the client")
	for _, event := range got {
		assert.Equal(t, "important", event.JobName, "other job types must be filtered out")
	}
}

// TestSSEWithoutFiltersReceivesEveryType 未给过滤参数时仍是全量订阅。
func TestSSEWithoutFiltersReceivesEveryType(t *testing.T) {
	srv, baseURL := newStartedServer(t)
	defer srv.Stop(context.Background())

	stream := openSSE(t, baseURL+"/sse/events")

	stop := publishUntil(t, srv.scheduler.GetEventBus(), 10*time.Millisecond,
		scheduledEvent("mixed"), failedEvent("mixed"))
	defer stop()

	got := collect(t, stream, 4, 2*time.Second)
	require.NotEmpty(t, got)

	seen := make(map[core.EventType]int, len(got))
	for _, event := range got {
		seen[event.Type]++
	}
	assert.Greater(t, seen[core.EventJobScheduled], 0)
	assert.Greater(t, seen[core.EventJobFailed], 0)
}

// TestSSEDeliversEachEventOnce 重复的类型参数不能让事件投递两次。
func TestSSEDeliversEachEventOnce(t *testing.T) {
	srv, baseURL := newStartedServer(t)
	defer srv.Stop(context.Background())

	stream := openSSE(t, baseURL+"/sse/events?event_types=job.failed,job.failed")

	// 等处理器完成订阅后再发布唯一一个事件，避免与持续发布的干扰
	time.Sleep(150 * time.Millisecond)
	srv.scheduler.GetEventBus().Publish(failedEvent("once"))

	select {
	case event := <-stream.events:
		assert.Equal(t, core.EventJobFailed, event.Type)
	case <-time.After(2 * time.Second):
		t.Fatal("Expected the failed event to be streamed")
	}

	select {
	case extra := <-stream.events:
		t.Fatalf("Expected exactly one frame for one published event, got a second one: %+v", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSSERejectsUnknownEventType(t *testing.T) {
	srv, baseURL := newStartedServer(t)
	defer srv.Stop(context.Background())

	resp, err := http.Get(baseURL + "/sse/events?event_types=job.exploded")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "非法过滤参数应在建流前拒绝，而不是挂着一条空流")
	assert.Contains(t, string(body), "invalid event_types")
}

// TestSSEUsesConfiguredCORS SSE 不再硬编码 Access-Control-Allow-Origin: *。
func TestSSEUsesConfiguredCORS(t *testing.T) {
	store, err := core.NewJSONFileStore(t.TempDir() + "/jobs.json")
	require.NoError(t, err)

	scheduler := core.NewScheduler(store, nil, nil)
	srv := NewServer(scheduler, store, "0", Security{AllowOrigins: []string{"https://app.example"}})
	require.NoError(t, srv.Start())
	defer srv.Stop(context.Background())

	req, err := http.NewRequest(http.MethodGet, "http://"+srv.ListenAddr()+"/sse/events", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://app.example")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "https://app.example", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.NotEqual(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
}
