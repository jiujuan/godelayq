package api

import (
	"container/list"
	"sync"

	"godelayq/core"
)

// 事件历史的三档上限。都是"观测辅助"级别的量：宁可少留也不要撑爆内存，
// 因此先用常量，等真有人抱怨再开配置项（本仓库惯例：未生效的选项不进配置）。
const (
	// historyPerJobLimit 单个任务保留的最近事件数
	historyPerJobLimit = 100
	// historyMaxJobs 内存里保留多少个任务的事件，超出按 LRU 淘汰整个任务
	historyMaxJobs = 2000
	// historyGlobalLimit 全局最近事件数，供 Dashboard 首屏回灌
	historyGlobalLimit = 500
)

// EventHistory 是事件总线的内存订阅者，按任务保留最近若干条事件。
//
// 语义要说清楚：它跟着进程走，重启即清空，不是审计日志。
// 任务详情页的时间线、Dashboard 的事件流都读它；
// 需要持久化的运行历史在观测层里（`observability.events.enabled`，
// 见 docs/design/sqlite-observability-design.md §6.1 与 TASK-S03/S04）：
// 装配了事件库时两个事件端点改读库，这里退成兜底数据源，本类型自身的行为不变。
type EventHistory struct {
	bus   *core.EventBus
	subID string

	mu     sync.Mutex
	perJob map[string]*list.Element // jobID -> LRU 链表元素，元素值是 *jobEvents
	lru    *list.List               // 队首最近使用，队尾待淘汰
	global []core.Event             // 跨任务的全局最近窗口（升序）
	total  int                      // 全部任务的事件条数合计，用于观测占用
}

// jobEvents 是单个任务的事件窗口（按时间升序）。
type jobEvents struct {
	jobID string
	items []core.Event
}

// NewEventHistory 订阅事件总线并开始收纳事件。
//
// 总线 Publish 是非阻塞的（缓冲区满即丢弃该事件），这里的 drain 协程只做
// 内存写入，绝不回压总线：丢掉的是一条记录，不是调度事实。
func NewEventHistory(bus *core.EventBus) *EventHistory {
	h := &EventHistory{
		bus:    bus,
		perJob: make(map[string]*list.Element),
		lru:    list.New(),
	}

	subID, events := bus.SubscribeAll()
	h.subID = subID
	go func() {
		for event := range events {
			h.record(event)
		}
	}()

	return h
}

// record 写入一个事件：全局窗口尾部追加，任务窗口尾部追加并按 LRU 记账。
func (h *EventHistory) record(event core.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if event.JobID == "" {
		return
	}

	// 全局窗口与任务窗口各自独立裁剪：Dashboard 首屏要的是"刚才发生了什么"，
	// 与某个任务的历史不是同一份数据。
	h.global = append(h.global, event)
	if overflow := len(h.global) - historyGlobalLimit; overflow > 0 {
		h.global = append(h.global[:0], h.global[overflow:]...)
	}

	element, ok := h.perJob[event.JobID]
	if !ok {
		if len(h.perJob) >= historyMaxJobs {
			h.evictOldestLocked()
		}
		entry := &jobEvents{jobID: event.JobID}
		element = h.lru.PushFront(entry)
		h.perJob[event.JobID] = element
	} else {
		h.lru.MoveToFront(element)
	}

	bucket := element.Value.(*jobEvents)
	bucket.items = append(bucket.items, event)
	if overflow := len(bucket.items) - historyPerJobLimit; overflow > 0 {
		// 丢最前面的：窗口是"最近 N 条"，保留的必须是尾部
		bucket.items = append(bucket.items[:0], bucket.items[overflow:]...)
		h.total -= overflow
	}
	h.total++
}

// evictOldestLocked 淘汰最久未写入的任务窗口。调用方需持锁。
func (h *EventHistory) evictOldestLocked() {
	back := h.lru.Back()
	if back == nil {
		return
	}
	bucket := back.Value.(*jobEvents)
	h.lru.Remove(back)
	delete(h.perJob, bucket.jobID)
	h.total -= len(bucket.items)
}

// Events 返回某个任务的最近事件，按时间升序；没有记录时返回空切片而不是 nil，
// 前端可以直接铺列表，不必区分"没订阅到"与"还没发生"。
// limit <= 0 表示取回全部已留存的事件。
func (h *EventHistory) Events(jobID string, limit int) []core.Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	element, ok := h.perJob[jobID]
	if !ok {
		return []core.Event{}
	}
	bucket := element.Value.(*jobEvents)
	if limit <= 0 {
		limit = len(bucket.items)
	}
	return tail(bucket.items, limit)
}

// Recent 返回全局最近的任意任务事件，按时间升序；同样保证非 nil。
// limit <= 0 表示取回全部已留存的 global 窗口。
func (h *EventHistory) Recent(limit int) []core.Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	if limit <= 0 {
		limit = len(h.global)
	}
	return tail(h.global, limit)
}

// Stats 是缓冲占用，供运维端点显示。
type EventHistoryStats struct {
	// Jobs 当前记录了事件的任务数
	Jobs int `json:"jobs"`
	// Events 任务窗口里的事件总数
	Events int `json:"events"`
	// GlobalCapacity 全局窗口容量，即 Recent 最多能返回多少条
	GlobalCapacity int `json:"global_capacity"`
	// PerJobCapacity 单个任务窗口容量
	PerJobCapacity int `json:"per_job_capacity"`
	// JobCapacity 任务数上限，超出后按 LRU 整体淘汰
	JobCapacity int `json:"job_capacity"`
}

// Stats 返回当前占用。
func (h *EventHistory) Stats() EventHistoryStats {
	h.mu.Lock()
	defer h.mu.Unlock()

	return EventHistoryStats{
		Jobs:           len(h.perJob),
		Events:         h.total,
		GlobalCapacity: historyGlobalLimit,
		PerJobCapacity: historyPerJobLimit,
		JobCapacity:    historyMaxJobs,
	}
}

// Clear 清空全部已记录的事件，返回被清掉的事件条数。
func (h *EventHistory) Clear() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	cleared := h.total
	// global 与 perJob 一起清：只留一半会让"清空缓冲"名不副实
	h.global = nil
	h.perJob = make(map[string]*list.Element)
	h.lru = list.New()
	h.total = 0

	return cleared
}

// Stop 取消对事件总线的订阅，drain 协程随通道关闭退出。可重复调用。
func (h *EventHistory) Stop() {
	h.bus.Unsubscribe(h.subID)
}

// tail 取升序切片的最后 n 条（副本，避免调用方持有内部切片）。
func tail(items []core.Event, n int) []core.Event {
	if n > len(items) {
		n = len(items)
	}
	out := make([]core.Event, n)
	copy(out, items[len(items)-n:])
	return out
}
