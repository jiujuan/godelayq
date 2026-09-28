package api

import (
	"net/http"
	"time"

	"godelayq/core"

	"github.com/gorilla/websocket"
)

// wsUpgrader 用 gorilla/websocket 实现 core.WSUpgrader，
// 让 core 不必依赖具体协议库。
type wsUpgrader struct {
	upgrader websocket.Upgrader
}

// newWSUpgrader 创建握手实现。来源白名单已由 core.WSServer 在升级前判定，
// 这里的 CheckOrigin 恒真，只为跳过 gorilla 默认的同源限制。
func newWSUpgrader() *wsUpgrader {
	return &wsUpgrader{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
	}
}

func (u *wsUpgrader) Upgrade(w http.ResponseWriter, r *http.Request) (core.WSConn, error) {
	conn, err := u.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	return &wsConn{conn: conn}, nil
}

// wsConn 把 gorilla 连接适配成 core.WSConn，方法签名一致，仅做转发
type wsConn struct {
	conn *websocket.Conn
}

func (c *wsConn) ReadMessage() (int, []byte, error) {
	return c.conn.ReadMessage()
}

func (c *wsConn) WriteMessage(messageType int, data []byte) error {
	return c.conn.WriteMessage(messageType, data)
}

func (c *wsConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *wsConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (c *wsConn) SetPongHandler(h func(appData string) error) {
	c.conn.SetPongHandler(h)
}

func (c *wsConn) UnexpectedClose(err error) bool {
	return websocket.IsUnexpectedCloseError(err,
		websocket.CloseGoingAway,
		websocket.CloseNormalClosure,
		websocket.CloseAbnormalClosure,
	)
}

func (c *wsConn) Close() error {
	return c.conn.Close()
}
