package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"godelayq/core"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStartedServer 在随机空闲端口上启动 API 服务器，返回实例与基础 URL
func newStartedServer(t *testing.T) (*Server, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	store, err := core.NewJSONFileStore(t.TempDir() + "/jobs.json")
	require.NoError(t, err)

	scheduler := core.NewScheduler(store, nil, nil)
	srv := NewServer(scheduler, store, "0", Security{}, newTestLogger())
	require.NoError(t, srv.Start())

	return srv, "http://" + srv.ListenAddr()
}

// TestServerStopClosesListener 验证 Stop 关的是真正在监听的那个 http.Server：
// 关停后端口不再接受新连接。旧实现新建了一个空 http.Server 调 Shutdown，等于没关。
func TestServerStopClosesListener(t *testing.T) {
	srv, baseURL := newStartedServer(t)

	resp, err := http.Get(baseURL + "/api/v1/health")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 200, resp.StatusCode)

	require.NoError(t, srv.Stop(context.Background()))

	client := &http.Client{Timeout: 500 * time.Millisecond}
	_, err = client.Get(baseURL + "/api/v1/health")
	assert.Error(t, err, "server must refuse connections after Stop")
}

// TestServerStopIsIdempotent 重复 Stop 不应 panic 或报错
func TestServerStopIsIdempotent(t *testing.T) {
	srv, _ := newStartedServer(t)

	require.NoError(t, srv.Stop(context.Background()))
	assert.NotPanics(t, func() {
		require.NoError(t, srv.Stop(context.Background()))
	})
}

// TestServerSSEStreamAndStop SSE 应能推流可解析的事件帧，
// 且 Stop 必须让该长连接立即收尾，而不是等关停超时。
func TestServerSSEStreamAndStop(t *testing.T) {
	srv, baseURL := newStartedServer(t)

	req, err := http.NewRequest("GET", baseURL+"/sse/events", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "SSE headers must arrive before the first event")
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	reader := bufio.NewReader(resp.Body)

	// 事件只在处理器订阅之后才投递，故持续发布直到读到目标帧
	stopPublishing := make(chan struct{})
	var stopOnce sync.Once
	stopPublisher := func() { stopOnce.Do(func() { close(stopPublishing) }) }
	t.Cleanup(stopPublisher)

	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopPublishing:
				return
			case <-ticker.C:
				srv.scheduler.GetEventBus().Publish(core.Event{
					Type:      core.EventJobScheduled,
					JobID:     "sse-1",
					JobName:   "demo",
					Timestamp: time.Now(),
				})
			}
		}
	}()

	// 首包是 ": connected" 注释帧，跳过它读出第一个 data 帧
	var line string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(line, "sse-1") {
		var err error
		line, err = reader.ReadString('\n')
		require.NoError(t, err, "expected an SSE frame")
	}
	require.Contains(t, line, "sse-1", "expected SSE data frame, got %q", line)
	require.True(t, strings.HasPrefix(line, "data:"))

	var ev core.Event
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "data: ")), &ev))
	assert.Equal(t, core.EventJobScheduled, ev.Type)
	assert.Equal(t, "sse-1", ev.JobID)

	start := time.Now()
	require.NoError(t, srv.Stop(context.Background()))
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 3*time.Second, "Stop should not wait for the shutdown timeout")

	stopPublisher()

	// 关停后：排空已缓冲的帧，流必须结束（处理器应已随请求上下文退出）
	if b, ok := resp.Body.(interface{ SetReadDeadline(time.Time) error }); ok {
		require.NoError(t, b.SetReadDeadline(time.Now().Add(3*time.Second)))
	}
	streamEnded := false
	for {
		if _, err := reader.ReadString('\n'); err != nil {
			streamEnded = true
			break
		}
	}
	assert.True(t, streamEnded, "SSE stream should be closed after Stop")

	require.NoError(t, resp.Body.Close())
}

// TestServerStopClosesWebSocketClients 验证 /ws 已接线，且 Stop 会关闭全部客户端
func TestServerStopClosesWebSocketClients(t *testing.T) {
	srv, _ := newStartedServer(t)

	wsURL := "ws://" + srv.ListenAddr() + "/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err, "GET /ws should upgrade")
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	defer conn.Close()

	srv.scheduler.GetEventBus().Publish(core.Event{Type: core.EventJobScheduled, JobID: "ws-1", JobName: "demo"})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(data), "ws-1")

	require.NoError(t, srv.Stop(context.Background()))

	// 关停后连接不可再用
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	assert.Error(t, err, "connection must be closed by Stop")

	assert.Equal(t, 0, srv.wsServer.GetStats()["clients"])
}

// TestServerStartReturnsListenError 端口被占用时 Start 应返回错误，
// 以便上层（cmd/server）回滚已启动的调度器。
func TestServerStartReturnsListenError(t *testing.T) {
	first, _ := newStartedServer(t)
	defer first.Stop(context.Background())

	store, err := core.NewJSONFileStore(t.TempDir() + "/jobs.json")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(first.ListenAddr())
	require.NoError(t, err)

	second := NewServer(core.NewScheduler(store, nil, nil), store, port, Security{}, newTestLogger())

	err = second.Start()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen on")
}
