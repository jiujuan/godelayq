package core

import (
	"encoding/json"
	"sync"
	"time"
)

// EventType 事件类型
type EventType string

const (
	EventJobScheduled EventType = "job.scheduled" // 任务已调度
	EventJobStarted   EventType = "job.started"   // 开始执行
	EventJobCompleted EventType = "job.completed" // 执行成功
	EventJobFailed    EventType = "job.failed"    // 执行失败
	EventJobCancelled EventType = "job.cancelled" // 已取消
	EventJobRetrying  EventType = "job.retrying"  // 重试中
	EventHeapUpdate   EventType = "heap.updated"  // 堆状态变更（用于监控）
)

// Event 任务事件
type Event struct {
	Type      EventType              `json:"type"`
	JobID     string                 `json:"job_id"`
	JobName   string                 `json:"job_name"`
	Status    JobStatus              `json:"status"`
	Timestamp time.Time              `json:"timestamp"`
	Data      json.RawMessage        `json:"data,omitempty"`     // 额外数据（如错误信息、执行结果）
	Metadata  map[string]interface{} `json:"metadata,omitempty"` // 元数据（执行时长、重试次数等）
}

// EventHandler 事件处理器接口
type EventHandler interface {
	Handle(event Event)
}

// EventBus 事件总线（支持多订阅者）
type EventBus struct {
	subscribers map[string][]chan Event
	mu          sync.RWMutex
	bufferSize  int
}

func NewEventBus(bufferSize int) *EventBus {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &EventBus{
		subscribers: make(map[string][]chan Event),
		bufferSize:  bufferSize,
	}
}

// Subscribe 订阅指定类型的事件，返回订阅ID和通道
func (eb *EventBus) Subscribe(eventTypes ...EventType) (string, <-chan Event) {
	id := generateID()
	ch := make(chan Event, eb.bufferSize)

	eb.mu.Lock()
	defer eb.mu.Unlock()

	for _, et := range eventTypes {
		eb.subscribers[string(et)] = append(eb.subscribers[string(et)], ch)
	}

	return id, ch
}

// SubscribeAll 订阅所有事件
func (eb *EventBus) SubscribeAll() (string, <-chan Event) {
	id := generateID()
	ch := make(chan Event, eb.bufferSize)

	eb.mu.Lock()
	defer eb.mu.Unlock()

	// 订阅所有已知事件类型
	allTypes := []EventType{
		EventJobScheduled, EventJobStarted, EventJobCompleted,
		EventJobFailed, EventJobCancelled, EventJobRetrying, EventHeapUpdate,
	}
	for _, et := range allTypes {
		eb.subscribers[string(et)] = append(eb.subscribers[string(et)], ch)
	}

	return id, ch
}

// Unsubscribe 取消订阅
func (eb *EventBus) Unsubscribe(id string, eventTypes ...EventType) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	// 简单实现：关闭对应channel并从列表移除
	// 实际生产环境需要更精确的ID匹配
}

// Publish 发布事件（异步，不阻塞）
func (eb *EventBus) Publish(event Event) {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	subs := eb.subscribers[string(event.Type)]
	for _, ch := range subs {
		select {
		case ch <- event: // 缓冲区未满，直接发送
		default: // 缓冲区已满，丢弃（避免阻塞调度器）
			// 可记录日志或扩展为环形缓冲区
		}
	}

	// 同时发送给订阅"all"的客户端
	if allSubs, ok := eb.subscribers["all"]; ok {
		for _, ch := range allSubs {
			select {
			case ch <- event:
			default:
			}
		}
	}
}

// Close 关闭总线
func (eb *EventBus) Close() {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	for _, subs := range eb.subscribers {
		for _, ch := range subs {
			close(ch)
		}
	}
	eb.subscribers = make(map[string][]chan Event)
}
