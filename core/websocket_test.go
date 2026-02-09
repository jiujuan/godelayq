package core

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebSocketServer(t *testing.T) {
	gin.SetMode(gin.TestMode)

	eb := NewEventBus(100)
	wsServer := NewWSServer(eb)
	wsServer.Start()
	defer wsServer.Stop()

	// 创建Gin路由
	r := gin.New()
	r.GET("/ws", wsServer.Handle)

	// 创建测试服务器
	ts := httptest.NewServer(r)
	defer ts.Close()

	// 转换 http:// -> ws://
	wsURL := strings.Replace(ts.URL, "http", "ws", 1) + "/ws"

	// 连接WebSocket
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer ws.Close()

	// 发送订阅消息
	subMsg := WSMessage{
		Action: "subscribe",
		Filter: WSFilter{
			EventTypes: []string{string(EventJobScheduled)},
		},
	}
	err = ws.WriteJSON(subMsg)
	require.NoError(t, err)

	// 等待订阅确认
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := ws.ReadMessage()
	assert.NoError(t, err)
	assert.Contains(t, string(data), "subscribed")

	// 发布事件
	eb.Publish(Event{
		Type:    EventJobScheduled,
		JobID:   "test-1",
		JobName: "test-job",
	})

	// 验证收到推送
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err = ws.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(data), "test-1")
	assert.Contains(t, string(data), "job.scheduled")
}

func TestWebSocketFilter(t *testing.T) {
	client := &WSClient{
		Filter: WSFilter{
			JobTypes:   []string{"payment"},
			EventTypes: []string{"job.completed"},
		},
	}

	// 匹配的事件
	matching := Event{
		Type:    EventJobCompleted,
		JobName: "payment",
	}
	assert.True(t, client.matchFilter(matching))

	// 类型不匹配
	wrongType := Event{
		Type:    EventJobStarted,
		JobName: "payment",
	}
	assert.False(t, client.matchFilter(wrongType))

	// 名称不匹配
	wrongName := Event{
		Type:    EventJobCompleted,
		JobName: "email",
	}
	assert.False(t, client.matchFilter(wrongName))
}

func TestWebSocketPingPong(t *testing.T) {
	gin.SetMode(gin.TestMode)
	eb := NewEventBus(10)
	ws := NewWSServer(eb)
	ws.Start()

	r := gin.New()
	r.GET("/ws", ws.Handle)
	ts := httptest.NewServer(r)
	defer ts.Close()

	wsURL := strings.Replace(ts.URL, "http", "ws", 1) + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	// 发送ping
	ping := WSMessage{Action: "ping"}
	err = conn.WriteJSON(ping)
	require.NoError(t, err)

	// 接收pong
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := conn.ReadMessage()
	assert.NoError(t, err)
	assert.Contains(t, string(data), "pong")
}
