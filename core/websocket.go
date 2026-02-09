package core

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // 生产环境应限制来源
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

// WSMessage WebSocket消息格式
type WSMessage struct {
	Action    string          `json:"action"` // subscribe, unsubscribe, ping
	Filter    WSFilter        `json:"filter"` // 订阅过滤条件
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type WSFilter struct {
	JobTypes   []string `json:"job_types,omitempty"`   // 按任务类型过滤
	JobIDs     []string `json:"job_ids,omitempty"`     // 关注特定任务
	Status     []string `json:"status,omitempty"`      // 按状态过滤
	EventTypes []string `json:"event_types,omitempty"` // 事件类型过滤
}

// WSServer WebSocket服务器
type WSServer struct {
	eventBus   *EventBus
	clients    map[*WSClient]bool // 所有连接
	register   chan *WSClient     // 新连接注册
	unregister chan *WSClient     // 连接断开
	broadcast  chan Event         // 广播通道
	mu         sync.RWMutex
}

// WSClient WebSocket客户端连接
type WSClient struct {
	ID      string
	Conn    *websocket.Conn
	Server  *WSServer
	Send    chan []byte  // 发送缓冲
	Filter  WSFilter     // 客户端订阅过滤
	eventCh <-chan Event // 订阅的事件通道
	subIDs  []string     // 订阅ID列表（用于取消）
	stopCh  chan struct{}
}

func NewWSServer(eventBus *EventBus) *WSServer {
	return &WSServer{
		eventBus:   eventBus,
		clients:    make(map[*WSClient]bool),
		register:   make(chan *WSClient, 100),
		unregister: make(chan *WSClient, 100),
		broadcast:  make(chan Event, 1000),
	}
}

// Start 启动 WebSocket 管理循环
func (ws *WSServer) Start() {
	go ws.run()
}

func (ws *WSServer) run() {
	for {
		select {
		case client := <-ws.register:
			ws.mu.Lock()
			ws.clients[client] = true
			ws.mu.Unlock()
			log.Printf("WebSocket client connected: %s (total: %d)", client.ID, len(ws.clients))

			// 订阅所有事件（带过滤）
			subID, eventCh := ws.eventBus.SubscribeAll()
			client.subIDs = append(client.subIDs, subID)
			client.eventCh = eventCh

			// 启动客户端读写协程
			go client.writePump()
			go client.readPump()

		case client := <-ws.unregister:
			ws.mu.Lock()
			if _, ok := ws.clients[client]; ok {
				delete(ws.clients, client)
				close(client.Send)
				close(client.stopCh)
				ws.mu.Unlock()
				log.Printf("WebSocket client disconnected: %s (total: %d)", client.ID, len(ws.clients))
			} else {
				ws.mu.Unlock()
			}

		case event := <-ws.broadcast:
			// 广播给所有客户端（通过各自的writePump发送）
			ws.mu.RLock()
			clients := make(map[*WSClient]bool, len(ws.clients))
			for k, v := range ws.clients {
				clients[k] = v
			}
			ws.mu.RUnlock()

			data, err := json.Marshal(event)
			if err != nil {
				continue
			}

			for client := range clients {
				if client.matchFilter(event) {
					select {
					case client.Send <- data:
					default:
						// 客户端发送缓冲已满，关闭连接
						ws.unregister <- client
					}
				}
			}
		}
	}
}

// Handle 处理 HTTP 升级请求
func (ws *WSServer) Handle(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	client := &WSClient{
		ID:     generateID(),
		Conn:   conn,
		Server: ws,
		Send:   make(chan []byte, 256),
		stopCh: make(chan struct{}),
	}

	ws.register <- client
}

// 检查事件是否匹配客户端过滤条件
func (c *WSClient) matchFilter(event Event) bool {
	// 按事件类型过滤
	if len(c.Filter.EventTypes) > 0 {
		matched := false
		for _, et := range c.Filter.EventTypes {
			if et == string(event.Type) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// 按任务ID过滤
	if len(c.Filter.JobIDs) > 0 {
		matched := false
		for _, id := range c.Filter.JobIDs {
			if id == event.JobID {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// 按任务类型过滤
	if len(c.Filter.JobTypes) > 0 {
		matched := false
		for _, jt := range c.Filter.JobTypes {
			if jt == event.JobName {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

// readPump 读取客户端消息（处理订阅/取消订阅/心跳）
func (c *WSClient) readPump() {
	defer func() {
		c.Server.unregister <- c
		c.Conn.Close()
	}()

	c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		var msg WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		switch msg.Action {
		case "ping":
			c.Send <- []byte(`{"action":"pong","timestamp":"` + time.Now().Format(time.RFC3339) + `"}`)

		case "subscribe":
			// 更新过滤条件
			c.Filter = msg.Filter
			c.Send <- []byte(`{"action":"subscribed","filter":` + string(message) + `}`)

		case "unsubscribe":
			// 清空过滤或取消特定订阅
			c.Filter = WSFilter{}

		case "get_stats":
			// 立即获取统计信息（请求-响应模式）
			stats := c.Server.getStats()
			data, _ := json.Marshal(map[string]interface{}{
				"action": "stats",
				"data":   stats,
			})
			c.Send <- data
		}
	}
}

// writePump 写入消息到客户端（处理事件推送和心跳）
func (c *WSClient) writePump() {
	ticker := time.NewTicker(30 * time.Second) // 心跳间隔
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-c.stopCh:
			return

		case event := <-c.eventCh:
			// 从EventBus接收事件并发送
			if c.matchFilter(event) {
				data, err := json.Marshal(event)
				if err != nil {
					continue
				}
				select {
				case c.Send <- data:
				default:
					return // 缓冲已满
				}
			}
		}
	}
}

func (ws *WSServer) getStats() map[string]interface{} {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	return map[string]interface{}{
		"clients":   len(ws.clients),
		"timestamp": time.Now(),
	}
}

// Stop 关闭所有连接
func (ws *WSServer) Stop() {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	for client := range ws.clients {
		close(client.stopCh)
		client.Conn.Close()
	}
	ws.clients = make(map[*WSClient]bool)
}
