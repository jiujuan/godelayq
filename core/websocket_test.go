package core

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本包不再依赖 gin/gorilla：连接与握手由 fake 提供，
// 真实协议栈的端到端握手在 api 包测试中覆盖。

var errFakeConnClosed = errors.New("fake websocket connection closed")

type fakeFrame struct {
	messageType int
	data        []byte
}

// fakeConn 是内存版 WSConn：incoming 模拟客户端帧，
// written 收集服务端写出的帧。
type fakeConn struct {
	incoming  chan []byte
	closeCh   chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	written []fakeFrame
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		incoming: make(chan []byte, 32),
		closeCh:  make(chan struct{}),
	}
}

func (c *fakeConn) ReadMessage() (int, []byte, error) {
	select {
	case data, ok := <-c.incoming:
		if !ok {
			return 0, nil, errFakeConnClosed
		}
		return WSTextMessage, data, nil
	case <-c.closeCh:
		return 0, nil, errFakeConnClosed
	}
}

func (c *fakeConn) WriteMessage(messageType int, data []byte) error {
	select {
	case <-c.closeCh:
		return errFakeConnClosed
	default:
	}

	buf := make([]byte, len(data))
	copy(buf, data)

	c.mu.Lock()
	c.written = append(c.written, fakeFrame{messageType: messageType, data: buf})
	c.mu.Unlock()
	return nil
}

func (c *fakeConn) SetReadDeadline(time.Time) error { return nil }

func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func (c *fakeConn) SetPongHandler(func(string) error) {}

// UnexpectedClose 恒为 false：fake 不产生协议关闭码，测试也不关心日志分级
func (c *fakeConn) UnexpectedClose(error) bool { return false }

func (c *fakeConn) Close() error {
	c.closeOnce.Do(func() { close(c.closeCh) })
	return nil
}

func (c *fakeConn) isClosed() bool {
	select {
	case <-c.closeCh:
		return true
	default:
		return false
	}
}

// writeClientFrame 把一个客户端消息投递给 readPump
func (c *fakeConn) writeClientFrame(t *testing.T, msg WSMessage) {
	t.Helper()
	data, err := json.Marshal(msg)
	require.NoError(t, err)
	c.incoming <- data
}

// pushClientFrame 是并发版本：不做断言，缓冲满时丢弃
func (c *fakeConn) pushClientFrame(msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case c.incoming <- data:
	default:
	}
}

// frames 返回已写出的帧快照
func (c *fakeConn) frames() []fakeFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fakeFrame(nil), c.written...)
}

// waitForFrame 轮询等待满足条件的帧
func (c *fakeConn) waitForFrame(t *testing.T, want string) fakeFrame {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range c.frames() {
			if strings.Contains(string(f.data), want) {
				return f
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("frame containing %q was not written; got %v", want, c.frames())
	return fakeFrame{}
}

// fakeUpgrader 实现 WSUpgrader：err 非空时模拟握手失败，
// 否则新建 fakeConn 并记录
type fakeUpgrader struct {
	mu     sync.Mutex
	err    error
	conns  []*fakeConn
	closed bool
}

func (u *fakeUpgrader) Upgrade(w http.ResponseWriter, r *http.Request) (WSConn, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.err != nil {
		return nil, u.err
	}
	if u.closed {
		return nil, errFakeConnClosed
	}

	conn := newFakeConn()
	u.conns = append(u.conns, conn)
	return conn, nil
}

func (u *fakeUpgrader) latest() *fakeConn {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.conns) == 0 {
		return nil
	}
	return u.conns[len(u.conns)-1]
}

// newTestWSServer 构造使用 fake 握手的 WSServer
func newTestWSServer(t *testing.T, eb *EventBus, allowedOrigins ...string) (*WSServer, *fakeUpgrader) {
	t.Helper()
	u := &fakeUpgrader{}
	return NewWSServer(eb, u, allowedOrigins...), u
}

// connect 走完整 Handle 流程建立一条 fake 连接
func connect(t *testing.T, ws *WSServer, u *fakeUpgrader, origin string) *fakeConn {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	require.NoError(t, err)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}

	ws.Handle(httptest.NewRecorder(), req)
	conn := u.latest()
	require.NotNil(t, conn, "upgrade must produce a connection")
	return conn
}

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
			ws, _ := newTestWSServer(t, NewEventBus(16), tc.allowed...)

			req, err := http.NewRequest(http.MethodGet, "http://localhost/ws", nil)
			require.NoError(t, err)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}

			assert.Equal(t, tc.want, ws.allowOrigin(req))
		})
	}
}

// 被拒的来源不应触发握手，也不应留下订阅
func TestWSServerHandleRejectsForeignOrigin(t *testing.T) {
	eb := NewEventBus(16)
	ws, u := newTestWSServer(t, eb, "https://good.example")

	req, err := http.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://evil.example")

	recorder := httptest.NewRecorder()
	ws.Handle(recorder, req)

	assert.Equal(t, http.StatusForbidden, recorder.Code)
	u.mu.Lock()
	defer u.mu.Unlock()
	assert.Empty(t, u.conns, "rejected origin must not upgrade")
	assert.Equal(t, 0, ws.GetStats()["clients"])

	eb.mu.RLock()
	defer eb.mu.RUnlock()
	assert.Equal(t, 0, len(eb.subscriptions))
}

func TestWebSocketServer(t *testing.T) {
	eb := NewEventBus(100)
	ws, u := newTestWSServer(t, eb)
	defer ws.Stop()

	conn := connect(t, ws, u, "")

	// 订阅只收 job.scheduled
	conn.writeClientFrame(t, WSMessage{
		Action: "subscribe",
		Filter: WSFilter{EventTypes: []string{string(EventJobScheduled)}},
	})
	conn.waitForFrame(t, "subscribed")

	// 匹配事件应推送
	eb.Publish(Event{Type: EventJobScheduled, JobID: "test-1", JobName: "test-job"})
	frame := conn.waitForFrame(t, "test-1")
	assert.Equal(t, WSTextMessage, frame.messageType)
	assert.Contains(t, string(frame.data), "job.scheduled")

	// 不匹配事件不推送
	eb.Publish(Event{Type: EventJobCompleted, JobID: "test-2", JobName: "test-job"})
	time.Sleep(100 * time.Millisecond)
	for _, f := range conn.frames() {
		assert.NotContains(t, string(f.data), "test-2")
	}

	assert.Equal(t, 1, ws.GetStats()["clients"])
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
	ws, u := newTestWSServer(t, eb)
	defer ws.Stop()

	conn := connect(t, ws, u, "")

	conn.writeClientFrame(t, WSMessage{Action: "ping"})
	conn.waitForFrame(t, "pong")

	conn.writeClientFrame(t, WSMessage{Action: "get_stats"})
	conn.waitForFrame(t, "stats")
}

// unsubscribe 应清空过滤器：此后再收到任意事件都会推送
func TestWebSocketUnsubscribeClearsFilter(t *testing.T) {
	eb := NewEventBus(10)
	ws, u := newTestWSServer(t, eb)
	defer ws.Stop()

	conn := connect(t, ws, u, "")
	conn.writeClientFrame(t, WSMessage{
		Action: "subscribe",
		Filter: WSFilter{EventTypes: []string{string(EventJobScheduled)}},
	})
	conn.waitForFrame(t, "subscribed")

	conn.writeClientFrame(t, WSMessage{Action: "unsubscribe"})
	conn.waitForFrame(t, "unsubscribed")

	eb.Publish(Event{Type: EventJobFailed, JobID: "after-unsub", JobName: "x"})
	conn.waitForFrame(t, "after-unsub")
}

// TestWebSocket_StopUnsubscribesAndIsIdempotent 关停必须退订事件总线（否则
// 订阅表随连接数无界增长），且重复 Stop 不得 panic。
func TestWebSocket_StopUnsubscribesAndIsIdempotent(t *testing.T) {
	eb := NewEventBus(50)
	ws, u := newTestWSServer(t, eb)

	conns := make([]*fakeConn, 0, 5)
	for i := 0; i < 5; i++ {
		conns = append(conns, connect(t, ws, u, ""))
	}

	waitFor(t, func() bool { return ws.GetStats()["clients"] == 5 })

	eb.mu.RLock()
	subsWhileConnected := len(eb.subscriptions)
	eb.mu.RUnlock()
	assert.Equal(t, 5, subsWhileConnected, "each client should hold exactly one subscription")

	ws.Stop()

	waitFor(t, func() bool { return ws.GetStats()["clients"] == 0 })

	eb.mu.RLock()
	remaining := len(eb.subscriptions)
	eb.mu.RUnlock()
	assert.Equal(t, 0, remaining, "Stop must unsubscribe every client")

	assert.NotPanics(t, func() { ws.Stop() })

	for _, conn := range conns {
		assert.True(t, conn.isClosed(), "Stop must close every connection")
	}
}

// TestWebSocket_ConcurrentChurn 并发建连/发布/关停，需在 -race 下通过：
// 覆盖 clients 集合、filter 读写、done/closeOnce 与心跳协程的交互。
func TestWebSocket_ConcurrentChurn(t *testing.T) {
	eb := NewEventBus(20)
	ws, u := newTestWSServer(t, eb)

	conns := make([]*fakeConn, 20)
	for i := range conns {
		conns[i] = connect(t, ws, u, "")
	}
	require.Equal(t, 20, ws.GetStats()["clients"])

	var wg sync.WaitGroup
	for _, conn := range conns {
		wg.Add(1)
		go func(conn *fakeConn) {
			defer wg.Done()
			// 反复更新过滤器，与写协程的匹配读取并发
			for j := 0; j < 5; j++ {
				conn.pushClientFrame(WSMessage{
					Action: "subscribe",
					Filter: WSFilter{
						EventTypes: []string{string(EventJobScheduled)},
						JobIDs:     []string{"job-x"},
					},
				})
				time.Sleep(2 * time.Millisecond)
			}
		}(conn)
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
	assert.NotPanics(t, func() { ws.Stop() })
	close(stop)
	wg.Wait()

	assert.Equal(t, 0, ws.GetStats()["clients"])
}

// TestWebSocket_HandleAfterStop 关停后到达的连接不应进入客户端集合。
func TestWebSocket_HandleAfterStop(t *testing.T) {
	eb := NewEventBus(10)
	ws, u := newTestWSServer(t, eb)

	ws.Stop()

	conn := connect(t, ws, u, "")
	assert.True(t, conn.isClosed(), "late connection must be closed")
	assert.Equal(t, 0, ws.GetStats()["clients"])

	eb.mu.RLock()
	defer eb.mu.RUnlock()
	assert.Equal(t, 0, len(eb.subscriptions))
}

// TestWSServerHandleUpgradeFailureUnsubscribes 握手失败必须回滚订阅。
func TestWSServerHandleUpgradeFailureUnsubscribes(t *testing.T) {
	eb := NewEventBus(10)
	u := &fakeUpgrader{err: errors.New("handshake rejected")}
	ws := NewWSServer(eb, u)

	req, err := http.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	require.NoError(t, err)
	ws.Handle(httptest.NewRecorder(), req)

	assert.Equal(t, 0, ws.GetStats()["clients"])

	eb.mu.RLock()
	defer eb.mu.RUnlock()
	assert.Equal(t, 0, len(eb.subscriptions), "failed upgrade must unsubscribe")
}

// TestWSClient_TrySendDropsWhenFull 发送缓冲满时丢弃而非阻塞/关闭通道，
// 这是原实现自死锁的根因。
func TestWSClient_TrySendDropsWhenFull(t *testing.T) {
	eb := NewEventBus(1)
	client := &WSClient{
		ID:     "c1",
		Conn:   newFakeConn(),
		server: NewWSServer(eb, &fakeUpgrader{}),
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

// 心跳帧必须使用 ping 帧类型，客户端库据此自动回 pong
func TestWSClient_WriteUsesTextFrame(t *testing.T) {
	eb := NewEventBus(1)
	ws, _ := newTestWSServer(t, eb)
	conn := newFakeConn()
	client := &WSClient{
		ID:      "c2",
		Conn:    conn,
		server:  ws,
		send:    make(chan []byte, 4),
		done:    make(chan struct{}),
		eventCh: make(chan Event, 4),
	}

	require.True(t, ws.register(client))
	require.NoError(t, client.write([]byte("hello")))

	frame := conn.waitForFrame(t, "hello")
	assert.Equal(t, WSTextMessage, frame.messageType)

	ws.Stop()
}

// waitFor 轮询等待条件成立
func waitFor(t *testing.T, pred func() bool) {
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
