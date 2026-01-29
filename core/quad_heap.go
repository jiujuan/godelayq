package core

// 四叉堆
type QuadHeap struct {
	items []DelayedItem
}

func NewQuadHeap() *QuadHeap {
	return &QuadHeap{
		items: make([]DelayedItem, 0),
	}
}

func (h *QuadHeap) Push(item DelayedItem) {
	item.SetIndex(h.Len())
	h.items = append(h.items, item)
	h.up(h.Len() - 1)
}

func (h *QuadHeap) Pop() DelayedItem {
	if h.IsEmpty() {
		return nil
	}
	n := h.Len() - 1
	h.swap(0, n)
	h.down(0, n)
	return h.popBack()
}

func (h *QuadHeap) Peek() DelayedItem {
	if h.IsEmpty() {
		return nil
	}
	return h.items[0]
}

func (h *QuadHeap) Remove(item DelayedItem) DelayedItem {
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

func (h *QuadHeap) Update(item DelayedItem, priority int64) {
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

func (h *QuadHeap) Len() int {
	return len(h.items)
}

func (h *QuadHeap) IsEmpty() bool {
	return h.Len() == 0
}

func (h *QuadHeap) less(i, j int) bool {
	return h.items[i].GetPriority() < h.items[j].GetPriority()
}

func (h *QuadHeap) swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].SetIndex(i)
	h.items[j].SetIndex(j)
}

func (h *QuadHeap) up(j int) {
	for {
		i := (j - 1) / 4
		if i == j || !h.less(j, i) {
			break
		}
		h.swap(i, j)
		j = i
	}
}

func (h *QuadHeap) down(i0, n int) bool {
	i := i0
	for {
		best := i
		firstChild := 4*i + 1

		for child := firstChild; child < firstChild+4 && child < n; child++ {
			if h.less(child, best) {
				best = child
			}
		}

		if best == i {
			break
		}

		h.swap(i, best)
		i = best
	}
	return i > i0
}

func (h *QuadHeap) popBack() DelayedItem {
	n := h.Len() - 1
	item := h.items[n]
	h.items[n] = nil
	h.items = h.items[:n]
	item.SetIndex(-1)
	return item
}
