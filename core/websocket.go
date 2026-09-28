package core

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// WSMessage WebSocket消息格式
type WSMessage struct {
	Action    string          `json:"action"` // subscribe, unsubscribe, ping, get_stats
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

// WSServer 维护一组客户端连接，每个客户端自带事件订阅与读写协程。
// 客户端集合与关停状态由 mu/quit 统一保护，不使用中心化的消息泵协程，
// 以免广播回路向自身的注册通道回填造成死锁。
type WSServer struct {
	eventBus *EventBus
	clients  map[*WSClient]bool
	mu       sync.RWMutex
	quit     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// upgrader 按来源白名单决定是否放行握手，创建后不再变更
	upgrader websocket.Upgrader
}

// WSClient WebSocket客户端连接
type WSClient struct {
	ID     string
	Conn   *websocket.Conn
	server *WSServer

	// send 承载请求-响应类消息；只有 writePump 读取，且从不关闭
	send chan []byte

	// filter 由读协程更新、由写协程匹配，须持 filterMu 访问
	filterMu sync.RWMutex
	filter   WSFilter

	// 事件订阅（关停时必须退订，否则 EventBus 订阅表随连接数无界增长）
	subID   string
	eventCh <-chan Event

	done      chan struct{} // 关闭后所有协程退出
	closeOnce sync.Once
}

// NewWSServer 创建 WebSocket 服务。allowedOrigins 限制浏览器跨域握手来源：
// 为空或含 "*" 表示接受任意来源（沿用历史行为），否则要求 Origin 精确匹配；
// 非浏览器客户端不发 Origin 头，始终允许。
func NewWSServer(eventBus *EventBus, allowedOrigins ...string) *WSServer {
	return &WSServer{
		eventBus: eventBus,
		clients:  make(map[*WSClient]bool),
		quit:     make(chan struct{}),
		upgrader: websocket.Upgrader{
			CheckOrigin:     originChecker(allowedOrigins),
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
	}
}

// originChecker 生成 websocket.Upgrader 的 CheckOrigin 实现
func originChecker(allowed []string) func(*http.Request) bool {
	wildcard := len(allowed) == 0
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		if o == "*" {
			wildcard = true
			continue
		}
		set[strings.ToLower(o)] = true
	}

	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// 浏览器以外的客户端（Go/curl）不发 Origin
			return true
		}
		if wildcard {
			return true
		}
		return set[strings.ToLower(origin)]
	}
}

// Handle 处理 HTTP 升级请求。
// 先订阅事件总线再升级，避免客户端拿到 101 后立即发布时落在订阅窗口之前。
func (ws *WSServer) Handle(c *gin.Context) {
	subID, eventCh := ws.eventBus.SubscribeAll()

	conn, err := ws.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		ws.eventBus.Unsubscribe(subID)
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	client := &WSClient{
		ID:      generateID(),
		Conn:    conn,
		server:  ws,
		send:    make(chan []byte, 256),
		done:    make(chan struct{}),
		subID:   subID,
		eventCh: eventCh,
	}

	if !ws.register(client) {
		client.close()
	}
}

// register 把连接纳入集合并启动读写协程。
// 服务器已关停时拒绝注册，返回 false。
func (ws *WSServer) register(client *WSClient) bool {
	ws.mu.Lock()
	select {
	case <-ws.quit:
		ws.mu.Unlock()
		return false
	default:
	}
	ws.clients[client] = true
	total := len(ws.clients)
	ws.wg.Add(2)
	ws.mu.Unlock()

	go client.writePump()
	go client.readPump()

	log.Printf("WebSocket client connected: %s (total: %d)", client.ID, total)
	return true
}

// unregister 从客户端集合中移除连接
func (ws *WSServer) unregister(client *WSClient) {
	ws.mu.Lock()
	_, ok := ws.clients[client]
	delete(ws.clients, client)
	total := len(ws.clients)
	ws.mu.Unlock()

	if ok {
		log.Printf("WebSocket client disconnected: %s (total: %d)", client.ID, total)
	}
}

// Stop 关闭所有客户端连接并等待读写协程退出（幂等）
func (ws *WSServer) Stop() {
	ws.stopOnce.Do(func() { close(ws.quit) })

	ws.mu.RLock()
	clients := make([]*WSClient, 0, len(ws.clients))
	for client := range ws.clients {
		clients = append(clients, client)
	}
	ws.mu.RUnlock()

	for _, client := range clients {
		client.close()
	}

	ws.wg.Wait()
}

// close 幂等地关停单条连接：退订事件、通知协程退出、关闭底层连接。
// 不关闭 send 通道，避免仍在写入的协程触发 send on closed channel。
func (c *WSClient) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.subID != "" {
			c.server.eventBus.Unsubscribe(c.subID)
		}
		c.Conn.Close()
		c.server.unregister(c)
	})
}

// trySend 非阻塞投递控制消息；队列满或连接已关闭时丢弃并返回 false
func (c *WSClient) trySend(data []byte) {
	select {
	case c.send <- data:
	default:
		log.Printf("WebSocket client %s send buffer full, dropping message", c.ID)
	}
}

func (c *WSClient) setFilter(f WSFilter) {
	c.filterMu.Lock()
	defer c.filterMu.Unlock()
	c.filter = f
}

func (c *WSClient) currentFilter() WSFilter {
	c.filterMu.RLock()
	defer c.filterMu.RUnlock()
	return c.filter
}

// matchFilter 检查事件是否匹配客户端过滤条件
func (c *WSClient) matchFilter(event Event) bool {
	return filterMatches(event, c.currentFilter())
}

func filterMatches(event Event, f WSFilter) bool {
	if len(f.EventTypes) > 0 && !containsString(f.EventTypes, string(event.Type)) {
		return false
	}
	if len(f.JobIDs) > 0 && !containsString(f.JobIDs, event.JobID) {
		return false
	}
	if len(f.JobTypes) > 0 && !containsString(f.JobTypes, event.JobName) {
		return false
	}
	return true
}

func containsString(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

// readPump 读取客户端消息（处理订阅/取消订阅/心跳/请求-响应）
func (c *WSClient) readPump() {
	defer c.server.wg.Done()
	defer c.close()

	c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			return
		}

		var msg WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		switch msg.Action {
		case "ping":
			c.trySend([]byte(`{"action":"pong","timestamp":"` + time.Now().Format(time.RFC3339) + `"}`))

		case "subscribe":
			c.setFilter(msg.Filter)
			c.trySend([]byte(`{"action":"subscribed","timestamp":"` + time.Now().Format(time.RFC3339) + `"}`))

		case "unsubscribe":
			c.setFilter(WSFilter{})
			c.trySend([]byte(`{"action":"unsubscribed","timestamp":"` + time.Now().Format(time.RFC3339) + `"}`))

		case "get_stats":
			stats := c.server.GetStats()
			data, _ := json.Marshal(map[string]interface{}{
				"action": "stats",
				"data":   stats,
			})
			c.trySend(data)
		}
	}
}

// writePump 是连接上唯一的写方：控制消息、事件推送与心跳都经由此协程发出
func (c *WSClient) writePump() {
	defer c.server.wg.Done()

	ticker := time.NewTicker(30 * time.Second) // 心跳间隔
	defer func() {
		ticker.Stop()
		c.close()
	}()

	for {
		select {
		case <-c.done:
			return

		case message := <-c.send:
			if err := c.write(message); err != nil {
				return
			}

		case event := <-c.eventCh:
			if !c.matchFilter(event) {
				continue
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if err := c.write(data); err != nil {
				return
			}

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *WSClient) write(message []byte) error {
	c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
		log.Printf("WebSocket write failed for %s: %v", c.ID, err)
		return err
	}
	return nil
}

// GetStats 返回连接统计信息
func (ws *WSServer) GetStats() map[string]interface{} {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	return map[string]interface{}{
		"clients":   len(ws.clients),
		"timestamp": time.Now(),
	}
}
