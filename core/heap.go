package core

import (
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

// branchFactor 四叉堆的分支因子：每个节点有 4 个子节点
const branchFactor = 4

// QuaternaryHeap 四叉堆 (4-ary heap)
// 索引计算：
// parent = (i - 1) / 4
// children = 4*i + 1, 4*i + 2, 4*i + 3, 4*i + 4
// 相比二叉堆层级减少约一半，父子节点在内存中更近，缓存局部性更好。
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

// Len 返回堆中元素数量
func (h *QuaternaryHeap) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.items)
}

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

// PushItem 线程安全插入
func (h *QuaternaryHeap) PushItem(item Item) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.indexMap[item.GetID()] = len(h.items)
	h.items = append(h.items, item)
	h.siftUp(len(h.items) - 1)
}

// PopIfDue 原子地弹出已到期的堆顶；堆顶未到期或堆为空时返回 nil。
// 相比 Peek 后再弹出，避免了两次加锁之间任务被 Cancel 的竞态，
// 因此不提供无条件的 Pop 出口。
func (h *QuaternaryHeap) PopIfDue(now time.Time) Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.items) == 0 || h.items[0].GetTriggerTime().After(now) {
		return nil
	}
	return h.popRoot()
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

// Get 按ID查找元素（O(1)，不弹出）
func (h *QuaternaryHeap) Get(id string) Item {
	h.mu.RLock()
	defer h.mu.RUnlock()
	idx, ok := h.indexMap[id]
	if !ok {
		return nil
	}
	return h.items[idx]
}

// Remove 通过ID删除指定任务 O(log n)
func (h *QuaternaryHeap) Remove(id string) Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	idx, ok := h.indexMap[id]
	if !ok {
		return nil
	}
	return h.removeAt(idx)
}

// Update 用新元素替换同ID元素并重新堆化
// Update 用 item 替换同 ID 的条目并重新定位。
// 返回 false 表示该 ID 已不在堆中（例如已被弹出执行），调用方据此判断是否成功。
func (h *QuaternaryHeap) Update(item Item) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	idx, ok := h.indexMap[item.GetID()]
	if !ok {
		return false
	}
	h.items[idx] = item
	h.siftRange(idx)
	return true
}

// popRoot 弹出堆顶，调用方须持有写锁
func (h *QuaternaryHeap) popRoot() Item {
	n := len(h.items)
	if n == 0 {
		return nil
	}
	return h.removeAt(0)
}

// removeAt 删除指定下标元素，调用方须持有写锁
func (h *QuaternaryHeap) removeAt(idx int) Item {
	last := len(h.items) - 1
	if idx != last {
		h.Swap(idx, last)
	}
	item := h.items[last]
	h.items = h.items[:last]
	delete(h.indexMap, item.GetID())
	if idx != last {
		h.siftRange(idx)
	}
	return item
}

// siftRange 从 idx 出发上浮或下沉，调用方须持有写锁
func (h *QuaternaryHeap) siftRange(idx int) {
	if idx > 0 && h.Less(idx, parentIndex(idx)) {
		h.siftUp(idx)
		return
	}
	h.siftDown(idx)
}

// siftUp 上浮（四叉堆版本），调用方须持有写锁
func (h *QuaternaryHeap) siftUp(idx int) {
	for idx > 0 {
		parent := parentIndex(idx)
		if !h.Less(idx, parent) {
			break
		}
		h.Swap(idx, parent)
		idx = parent
	}
}

// siftDown 下沉（一次比较4个子节点），调用方须持有写锁
func (h *QuaternaryHeap) siftDown(idx int) {
	n := len(h.items)
	for {
		minIdx := idx
		firstChild := branchFactor*idx + 1
		if firstChild >= n {
			return
		}
		last := firstChild + branchFactor
		if last > n {
			last = n
		}
		for child := firstChild; child < last; child++ {
			if h.Less(child, minIdx) {
				minIdx = child
			}
		}
		if minIdx == idx {
			return
		}
		h.Swap(idx, minIdx)
		idx = minIdx
	}
}

func parentIndex(i int) int {
	return (i - 1) / branchFactor
}
