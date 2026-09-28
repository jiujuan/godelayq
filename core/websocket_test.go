package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWSTestServer 启动一个仅暴露 /ws 的测试服务，返回 ws 地址与关闭函数
func newWSTestServer(t *testing.T, eb *EventBus) (string, *WSServer, func()) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	ws := NewWSServer(eb)

	r := gin.New()
	r.GET("/ws", ws.Handle)
	ts := httptest.NewServer(r)

	url := strings.Replace(ts.URL, "http", "ws", 1) + "/ws"
	return url, ws, func() {
		ts.Close()
		ws.Stop()
	}
}

func TestWebSocketServer(t *testing.T) {
	eb := NewEventBus(100)
	wsURL, wsServer, cleanup := newWSTestServer(t, eb)
	defer cleanup()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	// 发送订阅消息
	subMsg := WSMessage{
		Action: "subscribe",
		Filter: WSFilter{
			EventTypes: []string{string(EventJobScheduled)},
		},
	}
	require.NoError(t, conn.WriteJSON(subMsg))

	// 等待订阅确认
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(data), "subscribed")

	// 发布匹配事件
	eb.Publish(Event{Type: EventJobScheduled, JobID: "test-1", JobName: "test-job"})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err = conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(data), "test-1")
	assert.Contains(t, string(data), "job.scheduled")

	// 发布不匹配事件：不应推送给该客户端
	eb.Publish(Event{Type: EventJobCompleted, JobID: "test-2", JobName: "test-job"})
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, skipped, err := conn.ReadMessage()
	assert.Error(t, err, "filtered event should not be delivered")
	assert.NotContains(t, string(skipped), "test-2")

	// 连接仍在使用中
	assert.Equal(t, 1, wsServer.GetStats()["clients"])
}

func TestWebSocketFilter(t *testing.T) {
	client := &WSClient{}
	client.setFilter(WSFilter{
		JobTypes:   []string{"payment"},
		EventTypes: []string{"job.completed"},
	})

	// 匹配的事件
	assert.True(t, client.matchFilter(Event{Type: EventJobCompleted, JobName: "payment"}))

	// 类型不匹配
	assert.False(t, client.matchFilter(Event{Type: EventJobStarted, JobName: "payment"}))

	// 名称不匹配
	assert.False(t, client.matchFilter(Event{Type: EventJobCompleted, JobName: "email"}))

	// 清空后不再过滤
	client.setFilter(WSFilter{})
	assert.True(t, client.matchFilter(Event{Type: EventJobStarted, JobName: "email"}))
}

func TestWebSocketPingPong(t *testing.T) {
	eb := NewEventBus(10)
	wsURL, _, cleanup := newWSTestServer(t, eb)
	defer cleanup()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.WriteJSON(WSMessage{Action: "ping"}))

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(data), "pong")

	require.NoError(t, conn.WriteJSON(WSMessage{Action: "get_stats"}))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err = conn.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(data), "stats")
}

// TestWebSocket_StopUnsubscribesAndIsIdempotent 关停必须退订事件总线（否则
// 订阅表随连接数无界增长），且重复 Stop 不得 panic。
func TestWebSocket_StopUnsubscribesAndIsIdempotent(t *testing.T) {
	eb := NewEventBus(50)
	wsURL, wsServer, shutdown := newWSTestServer(t, eb)

	conns := make([]*websocket.Conn, 0, 5)
	for i := 0; i < 5; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		conns = append(conns, conn)
	}

	waitFor := func(pred func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if pred() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("condition not met in time")
	}

	waitFor(func() bool { return wsServer.GetStats()["clients"] == 5 })

	eb.mu.RLock()
	subsWhileConnected := len(eb.subscriptions)
	eb.mu.RUnlock()
	assert.Equal(t, 5, subsWhileConnected, "each client should hold exactly one subscription")

	shutdown() // ws.Stop()

	waitFor(func() bool { return wsServer.GetStats()["clients"] == 0 })

	eb.mu.RLock()
	remaining := len(eb.subscriptions)
	eb.mu.RUnlock()
	assert.Equal(t, 0, remaining, "Stop must unsubscribe every client")

	assert.NotPanics(t, func() { wsServer.Stop() })

	for _, conn := range conns {
		assert.NoError(t, conn.Close())
	}
}

// TestWebSocket_ConcurrentChurn 并发建连/发布/关停，需在 -race 下通过：
// 覆盖 clients 集合、filter 读写、done/closeOnce 与心跳协程的交互。
func TestWebSocket_ConcurrentChurn(t *testing.T) {
	eb := NewEventBus(20)
	wsURL, wsServer, shutdown := newWSTestServer(t, eb)
	defer shutdown()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				return
			}
			defer conn.Close()

			// 反复更新过滤器，与写协程的匹配读取并发
			for j := 0; j < 5; j++ {
				_ = conn.WriteJSON(WSMessage{Action: "subscribe", Filter: WSFilter{
					EventTypes: []string{string(EventJobScheduled)},
					JobIDs:     []string{"job-x"},
				}})
				conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				_, _, _ = conn.ReadMessage()
			}
		}(i)
	}

	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			eb.Publish(Event{Type: EventJobScheduled, JobID: "job-x", JobName: "burst"})
		}
	}()

	time.Sleep(150 * time.Millisecond)
	assert.NotPanics(t, func() { wsServer.Stop() })
	close(stop)
	wg.Wait()

	assert.Equal(t, 0, wsServer.GetStats()["clients"])
}

// TestWebSocket_HandleAfterStop 关停后到达的连接不应进入客户端集合。
func TestWebSocket_HandleAfterStop(t *testing.T) {
	eb := NewEventBus(10)
	gin.SetMode(gin.TestMode)
	ws := NewWSServer(eb)
	r := gin.New()
	r.GET("/ws", ws.Handle)
	ts := httptest.NewServer(r)
	defer ts.Close()

	ws.Stop()

	wsURL := strings.Replace(ts.URL, "http", "ws", 1) + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		defer conn.Close()
		// 升级成功后立即被服务端关闭
		conn.SetReadDeadline(time.Now().Add(time.Second))
		_, _, err = conn.ReadMessage()
		assert.Error(t, err)
	}

	assert.Equal(t, 0, ws.GetStats()["clients"])
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	assert.Equal(t, 0, len(eb.subscriptions))
}

// TestWSClient_TrySendDropsWhenFull 发送缓冲满时丢弃而非阻塞/关闭通道，
// 这是原实现自死锁的根因。
func TestWSClient_TrySendDropsWhenFull(t *testing.T) {
	eb := NewEventBus(1)
	client := &WSClient{
		ID:     "c1",
		Conn:   &websocket.Conn{},
		server: NewWSServer(eb),
		send:   make(chan []byte, 1),
		done:   make(chan struct{}),
	}

	payload, err := json.Marshal(map[string]string{"a": "b"})
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		for i := 0; i < 10; i++ {
			client.trySend(payload)
		}
	})
	assert.Equal(t, 1, len(client.send))
}

// TestWSServerOriginPolicy 校验握手来源限制：默认放开，配置白名单后精确匹配。
func TestWSServerOriginPolicy(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		origin  string
		want    bool
	}{
		{name: "未配置即接受任意来源", allowed: nil, origin: "https://evil.example", want: true},
		{name: "显式通配", allowed: []string{"*"}, origin: "https://evil.example", want: true},
		{name: "非浏览器客户端无 Origin 头", allowed: []string{"https://good.example"}, origin: "", want: true},
		{name: "白名单命中", allowed: []string{"https://good.example"}, origin: "https://good.example", want: true},
		{name: "白名单忽略大小写", allowed: []string{"https://Good.example"}, origin: "https://good.EXAMPLE", want: true},
		{name: "白名单之外拒绝", allowed: []string{"https://good.example"}, origin: "https://evil.example", want: false},
		{name: "本地打开的页面（null origin）被拒", allowed: []string{"https://good.example"}, origin: "null", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := NewWSServer(NewEventBus(16), tc.allowed...)

			req, err := http.NewRequest(http.MethodGet, "http://localhost/ws", nil)
			require.NoError(t, err)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}

			assert.Equal(t, tc.want, ws.upgrader.CheckOrigin(req))
		})
	}
}
