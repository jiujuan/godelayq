package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"godelayq/core"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebsocketActionsOverRealConn 用真实协议栈验证 core 的读写泵与 api 的
// gorilla 适配器接线正确：控制消息有回执、订阅过滤生效、写出的是文本帧。
func TestWebsocketActionsOverRealConn(t *testing.T) {
	srv, _ := newStartedServer(t)
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	// 未配置来源白名单时，浏览器跨域握手应放行（证明 gorilla 默认同源校验已被适配器关闭）
	conn, resp, err := websocket.DefaultDialer.Dial("ws://"+srv.ListenAddr()+"/ws",
		http.Header{"Origin": {"https://anywhere.example"}})
	require.NoError(t, err, "GET /ws 应完成握手")
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	defer conn.Close()

	bus := srv.scheduler.GetEventBus()

	require.NoError(t, conn.WriteJSON(core.WSMessage{Action: "ping"}))
	assert.Contains(t, readWSFrame(t, conn), "pong")

	require.NoError(t, conn.WriteJSON(core.WSMessage{
		Action: "subscribe",
		Filter: core.WSFilter{EventTypes: []string{string(core.EventJobScheduled)}},
	}))
	assert.Contains(t, readWSFrame(t, conn), "subscribed")

	bus.Publish(core.Event{Type: core.EventJobCompleted, JobID: "ws-skip"})
	bus.Publish(core.Event{Type: core.EventJobScheduled, JobID: "ws-keep"})
	assert.Contains(t, readWSFrame(t, conn), "ws-keep")

	require.NoError(t, conn.WriteJSON(core.WSMessage{Action: "get_stats"}))
	assert.Contains(t, readWSFrame(t, conn), "stats")

	assert.Equal(t, 1, srv.wsServer.GetStats()["clients"])
}

// readWSFrame 读取一帧文本消息，超时即失败
func readWSFrame(t *testing.T, conn *websocket.Conn) string {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	messageType, data, err := conn.ReadMessage()
	require.NoError(t, err, "expected a frame from the server")
	assert.Equal(t, websocket.TextMessage, messageType)

	return string(data)
}
