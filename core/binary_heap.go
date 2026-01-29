package core

// 程序基于执行时间的小顶堆，堆顶元素始终是执行时间最早的任务

type BinaryHeap struct {
	items []DelayedItem
}

func NewBinaryHeap() *BinaryHeap {
	return &BinaryHeap{
		items: make([]DelayedItem, 0),
	}
}

// 插入元素
func (h *BinaryHeap) Push(item DelayedItem) {
	item.SetIndex(h.Len())
	h.items = append(h.items, item)
	h.up(h.Len() - 1)
}

// 弹出元素
func (h *BinaryHeap) Pop() DelayedItem {
	if h.IsEmpty() {
		return nil
	}
	n := h.Len() - 1
	h.swap(0, n)
	h.down(0, n)
	return h.popBack()
}

func (h *BinaryHeap) Peek() DelayedItem {
	if h.IsEmpty() {
		return nil
	}
	return h.items[0]
}

func (h *BinaryHeap) Remove(item DelayedItem) DelayedItem {
	i := item.GetIndex()
	if i >= h.Len() || h.items[i] != item {
		return nil
	}
	n := h.Len() - 1
	if n != i {
		h.swap(i, n)
		if !h.down(i, n) {
			h.up(i)
		}
	}
	return h.popBack()
}

func (h *BinaryHeap) Update(item DelayedItem, priority int64) {
	i := item.GetIndex()
	if i >= h.Len() || h.items[i] != item {
		return
	}
	oldPriority := item.GetPriority()
	if priority < oldPriority {
		h.up(i)
	} else if priority > oldPriority {
		h.down(i, h.Len())
	}
}

func (h *BinaryHeap) Len() int {
	return len(h.items)
}

func (h *BinaryHeap) IsEmpty() bool {
	return h.Len() == 0
}

// 比较两个任务的执行时间, 执行时间更早的认为"更小", 在堆排序中，"更小"的元素会向堆顶移动
func (h *BinaryHeap) less(i, j int) bool {
	return h.items[i].GetPriority() < h.items[j].GetPriority()
}

// 交换两个元素的位置同时更新它们的索引
func (h *BinaryHeap) swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].SetIndex(i)
	h.items[j].SetIndex(j)
}

func (h *BinaryHeap) up(j int) {
	for {
		i := (j - 1) / 2
		if i == j || !h.less(j, i) {
			break
		}
		h.swap(i, j)
		j = i
	}
}

func (h *BinaryHeap) down(i0, n int) bool {
	i := i0
	for {
		j1 := 2*i + 1
		if j1 >= n || j1 < 0 {
			break
		}
		j := j1
		if j2 := j1 + 1; j2 < n && h.less(j2, j1) {
			j = j2
		}
		if !h.less(j, i) {
			break
		}
		h.swap(i, j)
		i = j
	}
	return i > i0
}

func (h *BinaryHeap) popBack() DelayedItem {
	n := h.Len() - 1
	item := h.items[n]
	h.items[n] = nil
	h.items = h.items[:n]
	item.SetIndex(-1)
	return item
}
