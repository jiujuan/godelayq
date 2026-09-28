package core

import (
	"encoding/json"
	"sync"
	"time"
)

// EventType 表示系统内可广播的事件类型。
type EventType string

const (
	// EventJobScheduled 表示任务已进入调度队列。
	EventJobScheduled EventType = "job.scheduled"
	// EventJobStarted 表示任务开始执行。
	EventJobStarted EventType = "job.started"
	// EventJobCompleted 表示任务执行成功结束。
	EventJobCompleted EventType = "job.completed"
	// EventJobFailed 表示任务执行失败。
	EventJobFailed EventType = "job.failed"
	// EventJobCancelled 表示任务被取消。
	EventJobCancelled EventType = "job.cancelled"
	// EventJobRetrying 表示任务进入重试流程。
	EventJobRetrying EventType = "job.retrying"
	// EventHeapUpdate 预留给堆状态变化或监控场景使用。
	EventHeapUpdate EventType = "heap.updated"
)

// Event 是在调度生命周期中流转的标准事件结构。
type Event struct {
	// Type 表示事件类别，例如调度、开始、完成、失败等。
	Type EventType `json:"type"`
	// JobID 是任务的唯一标识。
	JobID string `json:"job_id"`
	// JobName 是任务名称或类型名。
	JobName string `json:"job_name"`
	// Status 表示事件发生时任务的状态。
	Status JobStatus `json:"status"`
	// Timestamp 记录事件产生的时间。
	Timestamp time.Time `json:"timestamp"`
	// Data 保存事件的原始附加数据，例如错误信息。
	Data json.RawMessage `json:"data,omitempty"`
	// Metadata 保存结构化附加信息，例如重试次数、耗时等。
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// eventSubscription 保存单个订阅的内部元数据。
type eventSubscription struct {
	// ch 是当前订阅接收事件的通道。
	ch chan Event
	// eventTypes 记录当前订阅已绑定的事件类型键。
	eventTypes []string
}

// EventBus 是一个线程安全的内存事件总线。
type EventBus struct {
	// subscribers 按事件类型维护订阅通道列表。
	subscribers map[string][]chan Event
	// subscriptions 按订阅ID维护订阅详情，用于精准取消订阅。
	subscriptions map[string]eventSubscription
	// mu 保护订阅表和订阅详情的并发访问。
	mu sync.RWMutex
	// bufferSize 是每个订阅通道的默认缓冲区大小。
	bufferSize int
}

// NewEventBus 创建一个新的事件总线。
// 当 bufferSize 小于等于 0 时，会回退到默认缓冲区大小 100。
func NewEventBus(bufferSize int) *EventBus {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &EventBus{
		subscribers:   make(map[string][]chan Event),
		subscriptions: make(map[string]eventSubscription),
		bufferSize:    bufferSize,
	}
}

// Subscribe 订阅指定事件类型，并返回订阅ID与只读事件通道。
func (eb *EventBus) Subscribe(eventTypes ...EventType) (string, <-chan Event) {
	ch := make(chan Event, eb.bufferSize)

	eb.mu.Lock()
	defer eb.mu.Unlock()
	id := eb.nextSubscriptionIDLocked()

	keys := make([]string, 0, len(eventTypes))
	for _, et := range eventTypes {
		key := string(et)
		eb.subscribers[key] = append(eb.subscribers[key], ch)
		keys = append(keys, key)
	}

	eb.subscriptions[id] = eventSubscription{
		ch:         ch,
		eventTypes: keys,
	}

	return id, ch
}

// SubscribeAll 订阅所有事件类型。
// 内部通过特殊键 "all" 挂载，发布任意事件时都会广播给它。
func (eb *EventBus) SubscribeAll() (string, <-chan Event) {
	ch := make(chan Event, eb.bufferSize)

	eb.mu.Lock()
	defer eb.mu.Unlock()
	id := eb.nextSubscriptionIDLocked()

	eb.subscribers["all"] = append(eb.subscribers["all"], ch)
	eb.subscriptions[id] = eventSubscription{
		ch:         ch,
		eventTypes: []string{"all"},
	}

	return id, ch
}

// Unsubscribe 按订阅ID取消订阅。
// 如果未传 eventTypes，则取消该订阅关联的全部事件类型；
// 如果传入 eventTypes，则只移除对应类型，其余类型继续保留。
func (eb *EventBus) Unsubscribe(id string, eventTypes ...EventType) {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	sub, ok := eb.subscriptions[id]
	if !ok {
		return
	}

	removeAll := len(eventTypes) == 0
	removeSet := make(map[string]struct{}, len(eventTypes))
	for _, et := range eventTypes {
		removeSet[string(et)] = struct{}{}
	}

	remaining := make([]string, 0, len(sub.eventTypes))
	for _, key := range sub.eventTypes {
		if removeAll {
			eb.subscribers[key] = removeSubscriberChannel(eb.subscribers[key], sub.ch)
			if len(eb.subscribers[key]) == 0 {
				delete(eb.subscribers, key)
			}
			continue
		}

		if _, shouldRemove := removeSet[key]; shouldRemove {
			eb.subscribers[key] = removeSubscriberChannel(eb.subscribers[key], sub.ch)
			if len(eb.subscribers[key]) == 0 {
				delete(eb.subscribers, key)
			}
			continue
		}

		remaining = append(remaining, key)
	}

	if removeAll || len(remaining) == 0 {
		close(sub.ch)
		delete(eb.subscriptions, id)
		return
	}

	sub.eventTypes = remaining
	eb.subscriptions[id] = sub
}

// Publish 向匹配的订阅者广播事件。
// 发送采用非阻塞方式，订阅者缓冲区满时会直接丢弃该事件，避免拖慢调度主流程。
func (eb *EventBus) Publish(event Event) {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	subs := eb.subscribers[string(event.Type)]
	for _, ch := range subs {
		select {
		case ch <- event:
		default:
		}
	}

	if allSubs, ok := eb.subscribers["all"]; ok {
		for _, ch := range allSubs {
			select {
			case ch <- event:
			default:
			}
		}
	}
}

// Close 关闭事件总线。
// 它会关闭所有仍然存活的订阅通道，并清空内部状态。
func (eb *EventBus) Close() {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	closed := make(map[chan Event]struct{})
	for _, sub := range eb.subscriptions {
		if _, ok := closed[sub.ch]; ok {
			continue
		}
		close(sub.ch)
		closed[sub.ch] = struct{}{}
	}

	eb.subscribers = make(map[string][]chan Event)
	eb.subscriptions = make(map[string]eventSubscription)
}

// removeSubscriberChannel 从某个事件类型的订阅通道列表中移除目标通道。
func removeSubscriberChannel(subs []chan Event, target chan Event) []chan Event {
	result := subs[:0]
	for _, ch := range subs {
		if ch != target {
			result = append(result, ch)
		}
	}
	return result
}

// nextSubscriptionIDLocked 生成一个当前 EventBus 内唯一的订阅ID。
// 调用方必须已经持有写锁。
func (eb *EventBus) nextSubscriptionIDLocked() string {
	id := generateID()
	for {
		if _, exists := eb.subscriptions[id]; !exists {
			return id
		}
		id = id + "-sub"
	}
}
