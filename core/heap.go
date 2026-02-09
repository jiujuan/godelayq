package core

import (
	"container/heap"
	"sync"
	"time"
)

// Item 堆元素接口
type Item interface {
	// GetTriggerTime 返回触发时间，用于堆排序
	GetTriggerTime() time.Time
	// GetID 返回唯一标识
	GetID() string
}

// QuaternaryHeap 四叉堆 (4-ary heap)
// 每个节点有4个子节点，索引计算：
// parent = (i - 1) / 4
// children = 4*i + 1, 4*i + 2, 4*i + 3, 4*i + 4
type QuaternaryHeap struct {
	items []Item
	mu    sync.RWMutex
	// indexMap 用于O(1)查找元素位置，支持快速删除
	indexMap map[string]int
}

func NewQuaternaryHeap() *QuaternaryHeap {
	return &QuaternaryHeap{
		items:    make([]Item, 0),
		indexMap: make(map[string]int),
	}
}

// Len 实现 heap.Interface
func (h *QuaternaryHeap) Len() int { return len(h.items) }

// Less 按触发时间升序（最小堆）
func (h *QuaternaryHeap) Less(i, j int) bool {
	return h.items[i].GetTriggerTime().Before(h.items[j].GetTriggerTime())
}

// Swap 交换元素并更新索引
func (h *QuaternaryHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.indexMap[h.items[i].GetID()] = i
	h.indexMap[h.items[j].GetID()] = j
}

// Push 添加元素
func (h *QuaternaryHeap) Push(x interface{}) {
	item := x.(Item)
	h.indexMap[item.GetID()] = len(h.items)
	h.items = append(h.items, item)
}

// Pop 弹出最后一个元素（非堆顶）
func (h *QuaternaryHeap) Pop() interface{} {
	old := h.items
	n := len(old)
	item := old[n-1]
	h.items = old[:n-1]
	delete(h.indexMap, item.GetID())
	return item
}

// PushItem 线程安全插入
func (h *QuaternaryHeap) PushItem(item Item) {
	h.mu.Lock()
	defer h.mu.Unlock()
	heap.Push(h, item)
}

// PopItem 线程安全弹出堆顶
func (h *QuaternaryHeap) PopItem() Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.items) == 0 {
		return nil
	}
	return heap.Pop(h).(Item)
}

// Peek 查看堆顶（不弹出）
func (h *QuaternaryHeap) Peek() Item {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.items) == 0 {
		return nil
	}
	return h.items[0]
}

// Remove 通过ID删除指定任务 O(log n)
func (h *QuaternaryHeap) Remove(id string) Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	idx, ok := h.indexMap[id]
	if !ok {
		return nil
	}
	// 使用 heap.Remove 保持堆性质
	return heap.Remove(h, idx).(Item)
}

// Update 更新时间并重新堆化
func (h *QuaternaryHeap) Update(item Item) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if idx, ok := h.indexMap[item.GetID()]; ok {
		h.items[idx] = item
		heap.Fix(h, idx)
	}
}

// HeapifyUp 上浮操作（四叉堆版本）
func (h *QuaternaryHeap) heapifyUp(idx int) {
	if idx == 0 {
		return
	}
	parent := (idx - 1) / 4
	if h.Less(idx, parent) {
		h.Swap(idx, parent)
		h.heapifyUp(parent)
	}
}

// HeapifyDown 下沉操作（比较4个子节点）
func (h *QuaternaryHeap) heapifyDown(idx int) {
	n := len(h.items)
	minIdx := idx

	// 检查4个子节点
	for i := 1; i <= 4; i++ {
		child := 4*idx + i
		if child < n && h.Less(child, minIdx) {
			minIdx = child
		}
	}

	if minIdx != idx {
		h.Swap(idx, minIdx)
		h.heapifyDown(minIdx)
	}
}
