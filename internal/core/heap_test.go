package core

import (
	"math/rand"
	"strconv"
	"sync"
	"testing"
	"time"
)

// mockItem 用于测试的 Item 实现
type mockItem struct {
	id        string
	triggerAt time.Time
}

func (m *mockItem) GetTriggerTime() time.Time {
	return m.triggerAt
}

func (m *mockItem) GetID() string {
	return m.id
}

// popTop 在测试里无条件弹出堆顶：生产路径只有到点才弹的 PopIfDue，
// 堆不再提供 PopItem 这类绕过到期判断的出口。
func popTop(h *QuaternaryHeap) Item {
	return h.PopIfDue(time.Now().Add(100 * 365 * 24 * time.Hour))
}

func TestNewQuaternaryHeap(t *testing.T) {
	h := NewQuaternaryHeap()

	if h == nil {
		t.Fatal("Expected heap to be created, got nil")
	}
	if h.Len() != 0 {
		t.Errorf("Expected empty heap, got length %d", h.Len())
	}
	if h.items == nil {
		t.Error("Expected items slice to be initialized")
	}
	if h.indexMap == nil {
		t.Error("Expected indexMap to be initialized")
	}
}

func TestQuaternaryHeap_PushAndPop(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	// Push items
	items := []*mockItem{
		{id: "item1", triggerAt: now.Add(5 * time.Minute)},
		{id: "item2", triggerAt: now.Add(1 * time.Minute)},
		{id: "item3", triggerAt: now.Add(10 * time.Minute)},
		{id: "item4", triggerAt: now.Add(3 * time.Minute)},
	}

	for _, item := range items {
		h.PushItem(item)
	}

	if h.Len() != 4 {
		t.Errorf("Expected heap length 4, got %d", h.Len())
	}

	// Pop items - should come out in sorted order
	expectedOrder := []string{"item2", "item4", "item1", "item3"}
	for i, expectedID := range expectedOrder {
		item := popTop(h)
		if item == nil {
			t.Fatalf("Expected item at position %d, got nil", i)
		}
		if item.GetID() != expectedID {
			t.Errorf("Position %d: expected %s, got %s", i, expectedID, item.GetID())
		}
	}

	// Heap should be empty
	if h.Len() != 0 {
		t.Errorf("Expected empty heap, got length %d", h.Len())
	}

	// Pop from empty heap should return nil
	item := popTop(h)
	if item != nil {
		t.Error("Expected nil from empty heap")
	}
}

func TestQuaternaryHeap_Peek(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	// Peek empty heap
	if item := h.Peek(); item != nil {
		t.Error("Expected nil from empty heap peek")
	}

	// Add items
	h.PushItem(&mockItem{id: "item1", triggerAt: now.Add(5 * time.Minute)})
	h.PushItem(&mockItem{id: "item2", triggerAt: now.Add(1 * time.Minute)})
	h.PushItem(&mockItem{id: "item3", triggerAt: now.Add(10 * time.Minute)})

	// Peek should return earliest item without removing it
	item := h.Peek()
	if item == nil {
		t.Fatal("Expected item from peek, got nil")
	}
	if item.GetID() != "item2" {
		t.Errorf("Expected item2, got %s", item.GetID())
	}

	// Length should remain unchanged
	if h.Len() != 3 {
		t.Errorf("Expected length 3 after peek, got %d", h.Len())
	}

	// Peek again should return same item
	item2 := h.Peek()
	if item2.GetID() != "item2" {
		t.Errorf("Expected item2 on second peek, got %s", item2.GetID())
	}
}

func TestQuaternaryHeap_Remove(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	items := []*mockItem{
		{id: "item1", triggerAt: now.Add(5 * time.Minute)},
		{id: "item2", triggerAt: now.Add(1 * time.Minute)},
		{id: "item3", triggerAt: now.Add(10 * time.Minute)},
		{id: "item4", triggerAt: now.Add(3 * time.Minute)},
		{id: "item5", triggerAt: now.Add(7 * time.Minute)},
	}

	for _, item := range items {
		h.PushItem(item)
	}

	// Remove middle item
	removed := h.Remove("item1")
	if removed == nil {
		t.Fatal("Expected removed item, got nil")
	}
	if removed.GetID() != "item1" {
		t.Errorf("Expected item1, got %s", removed.GetID())
	}
	if h.Len() != 4 {
		t.Errorf("Expected length 4 after remove, got %d", h.Len())
	}

	// Remove non-existent item
	removed = h.Remove("nonexistent")
	if removed != nil {
		t.Error("Expected nil when removing non-existent item")
	}

	// Verify heap property maintained
	prev := popTop(h)
	for h.Len() > 0 {
		current := popTop(h)
		if current.GetTriggerTime().Before(prev.GetTriggerTime()) {
			t.Error("Heap property violated after remove")
		}
		prev = current
	}
}

func TestQuaternaryHeap_Update(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	items := []*mockItem{
		{id: "item1", triggerAt: now.Add(5 * time.Minute)},
		{id: "item2", triggerAt: now.Add(10 * time.Minute)},
		{id: "item3", triggerAt: now.Add(15 * time.Minute)},
	}

	for _, item := range items {
		h.PushItem(item)
	}

	// Update item2 to have earliest time
	updatedItem := &mockItem{id: "item2", triggerAt: now.Add(1 * time.Minute)}
	if !h.Update(updatedItem) {
		t.Error("Expected Update of a present item to report success")
	}

	// item2 should now be at top
	top := h.Peek()
	if top.GetID() != "item2" {
		t.Errorf("Expected item2 at top after update, got %s", top.GetID())
	}

	// Update non-existent item: reports failure and leaves the heap untouched
	if h.Update(&mockItem{id: "nonexistent", triggerAt: now}) {
		t.Error("Expected Update of a missing item to report failure")
	}

	// Verify heap still works
	if h.Len() != 3 {
		t.Errorf("Expected length 3, got %d", h.Len())
	}
}

func TestQuaternaryHeap_HeapProperty(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	// Add many items in random order
	for i := 0; i < 100; i++ {
		h.PushItem(&mockItem{
			id:        string(rune('a' + i)),
			triggerAt: now.Add(time.Duration(100-i) * time.Minute),
		})
	}

	// Pop all items and verify they come out in sorted order
	var prev Item
	for h.Len() > 0 {
		current := popTop(h)
		if prev != nil {
			if current.GetTriggerTime().Before(prev.GetTriggerTime()) {
				t.Error("Heap property violated: items not in sorted order")
			}
		}
		prev = current
	}
}

func TestQuaternaryHeap_Concurrent(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	var wg sync.WaitGroup
	numGoroutines := 10
	itemsPerGoroutine := 10

	// Concurrent pushes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := 0; j < itemsPerGoroutine; j++ {
				h.PushItem(&mockItem{
					id:        string(rune('a' + offset*itemsPerGoroutine + j)),
					triggerAt: now.Add(time.Duration(offset*itemsPerGoroutine+j) * time.Second),
				})
			}
		}(i)
	}

	wg.Wait()

	expectedLen := numGoroutines * itemsPerGoroutine
	if h.Len() != expectedLen {
		t.Errorf("Expected length %d, got %d", expectedLen, h.Len())
	}

	// Concurrent pops
	results := make(chan Item, expectedLen)
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < itemsPerGoroutine; j++ {
				if item := popTop(h); item != nil {
					results <- item
				}
			}
		}()
	}

	wg.Wait()
	close(results)

	// Verify all items were popped
	count := 0
	for range results {
		count++
	}

	if count != expectedLen {
		t.Errorf("Expected %d items popped, got %d", expectedLen, count)
	}

	if h.Len() != 0 {
		t.Errorf("Expected empty heap, got length %d", h.Len())
	}
}

func TestQuaternaryHeap_Less(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	h.items = []Item{
		&mockItem{id: "item1", triggerAt: now.Add(5 * time.Minute)},
		&mockItem{id: "item2", triggerAt: now.Add(1 * time.Minute)},
	}

	if !h.Less(1, 0) {
		t.Error("Expected item2 (index 1) to be less than item1 (index 0)")
	}

	if h.Less(0, 1) {
		t.Error("Expected item1 (index 0) to not be less than item2 (index 1)")
	}
}

func TestQuaternaryHeap_Swap(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	item1 := &mockItem{id: "item1", triggerAt: now.Add(5 * time.Minute)}
	item2 := &mockItem{id: "item2", triggerAt: now.Add(1 * time.Minute)}

	h.items = []Item{item1, item2}
	h.indexMap = map[string]int{
		"item1": 0,
		"item2": 1,
	}

	h.Swap(0, 1)

	// Verify items swapped
	if h.items[0].GetID() != "item2" {
		t.Errorf("Expected item2 at index 0, got %s", h.items[0].GetID())
	}
	if h.items[1].GetID() != "item1" {
		t.Errorf("Expected item1 at index 1, got %s", h.items[1].GetID())
	}

	// Verify index map updated
	if h.indexMap["item1"] != 1 {
		t.Errorf("Expected item1 index 1, got %d", h.indexMap["item1"])
	}
	if h.indexMap["item2"] != 0 {
		t.Errorf("Expected item2 index 0, got %d", h.indexMap["item2"])
	}
}

func TestQuaternaryHeap_DuplicateTimes(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()
	sameTime := now.Add(5 * time.Minute)

	// Add items with same trigger time
	h.PushItem(&mockItem{id: "item1", triggerAt: sameTime})
	h.PushItem(&mockItem{id: "item2", triggerAt: sameTime})
	h.PushItem(&mockItem{id: "item3", triggerAt: sameTime})

	if h.Len() != 3 {
		t.Errorf("Expected length 3, got %d", h.Len())
	}

	// All items should be poppable
	ids := make(map[string]bool)
	for i := 0; i < 3; i++ {
		item := popTop(h)
		if item == nil {
			t.Fatalf("Expected item at position %d, got nil", i)
		}
		ids[item.GetID()] = true
	}

	// Verify all unique IDs were popped
	if len(ids) != 3 {
		t.Errorf("Expected 3 unique items, got %d", len(ids))
	}
}

func TestQuaternaryHeap_RemoveFromSingleItem(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	h.PushItem(&mockItem{id: "only-item", triggerAt: now})

	removed := h.Remove("only-item")
	if removed == nil {
		t.Fatal("Expected removed item, got nil")
	}
	if removed.GetID() != "only-item" {
		t.Errorf("Expected only-item, got %s", removed.GetID())
	}
	if h.Len() != 0 {
		t.Errorf("Expected empty heap, got length %d", h.Len())
	}
}

func TestQuaternaryHeap_FourAryStructure(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	for i := 0; i < 200; i++ {
		h.PushItem(&mockItem{
			id:        "item-" + string(rune('A'+i%26)) + "-" + strconv.Itoa(i),
			triggerAt: now.Add(time.Duration(rand.Intn(1000)) * time.Minute),
		})
	}

	// 每个节点的父节点必须不大于自身：parent = (i-1)/4
	h.mu.RLock()
	defer h.mu.RUnlock()
	for i := 1; i < len(h.items); i++ {
		parent := (i - 1) / 4
		if h.Less(i, parent) {
			t.Fatalf("4-ary heap property violated: child %d < parent %d", i, parent)
		}
	}
}

func TestQuaternaryHeap_PopIfDue(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	// 空堆
	if item := h.PopIfDue(now); item != nil {
		t.Error("Expected nil from empty heap")
	}

	h.PushItem(&mockItem{id: "future", triggerAt: now.Add(1 * time.Hour)})
	if item := h.PopIfDue(now); item != nil {
		t.Error("Expected nil when head is not due")
	}
	if h.Len() != 1 {
		t.Error("Non-due PopIfDue must not remove the item")
	}

	due := &mockItem{id: "due", triggerAt: now.Add(-1 * time.Second)}
	h.PushItem(due)
	if item := h.PopIfDue(now); item == nil || item.GetID() != "due" {
		t.Errorf("Expected due item, got %v", item)
	}
	if h.Len() != 1 {
		t.Errorf("Expected 1 remaining, got %d", h.Len())
	}
}

// TestQuaternaryHeap_PopIfDueWhere 覆盖 E13 加的"绕开被挡住的堆顶"这条出口。
func TestQuaternaryHeap_PopIfDueWhere(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	blocked := func(item Item) bool { return item.GetID() != "exec" }

	// 空堆
	if item := h.PopIfDueWhere(now, blocked); item != nil {
		t.Error("空堆不该弹出任何东西")
	}

	// 堆顶未到期：即使 allow 认可它，也不能弹出
	h.PushItem(&mockItem{id: "future", triggerAt: now.Add(time.Hour)})
	if item := h.PopIfDueWhere(now, blocked); item != nil {
		t.Errorf("堆顶未到期时不该弹出，实际 %v", item.GetID())
	}

	// 堆顶可投递：走常数级快路径，弹的是堆顶
	duePlain := &mockItem{id: "plain", triggerAt: now.Add(-time.Second)}
	h.PushItem(duePlain)
	if item := h.PopIfDueWhere(now, blocked); item == nil || item.GetID() != "plain" {
		t.Fatalf("堆顶可投递时应弹堆顶，实际 %v", item)
	}

	// 堆顶被挡住时，取到期区里"可投递且最早"的那一项：
	// 后到期的普通项要优先于更早到期但被挡住的项，而更晚的未到期项绝不参与。
	h.PushItem(&mockItem{id: "exec", triggerAt: now.Add(-3 * time.Second)})
	h.PushItem(&mockItem{id: "plain-late", triggerAt: now.Add(-time.Second)})
	h.PushItem(&mockItem{id: "plain-earliest", triggerAt: now.Add(-2 * time.Second)})
	item := h.PopIfDueWhere(now, blocked)
	if item == nil || item.GetID() != "plain-earliest" {
		t.Fatalf("应取可投递项里最早的那个，实际 %v", item)
	}
	if h.Len() != 3 {
		t.Errorf("只该弹出一条，剩余 %d", h.Len())
	}

	// 到期项全被挡住：返回 nil 而不是硬弹一个
	if item := h.PopIfDueWhere(now, blocked); item == nil || item.GetID() != "plain-late" {
		t.Fatalf("第二早的可投递项应接着被弹出，实际 %v", item)
	}
	if item := h.PopIfDueWhere(now, blocked); item != nil {
		t.Errorf("只剩被挡住的项时应返回 nil，实际 %v", item.GetID())
	}

	// allow 为 nil 与 PopIfDue 等价
	if item := h.PopIfDueWhere(now, nil); item == nil || item.GetID() != "exec" {
		t.Errorf("allow 为 nil 时应等价于 PopIfDue，实际 %v", item)
	}
}

// TestQuaternaryHeap_HasDue 覆盖调度循环用来区分"没事做"与"有事做不了"的那个查询。
func TestQuaternaryHeap_HasDue(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()

	if h.HasDue(now) {
		t.Error("空堆不该判定为有到期任务")
	}

	h.PushItem(&mockItem{id: "future", triggerAt: now.Add(time.Hour)})
	if h.HasDue(now) {
		t.Error("只有未到期项时不应判定为有到期任务")
	}

	h.PushItem(&mockItem{id: "due", triggerAt: now.Add(-time.Second)})
	if !h.HasDue(now) {
		t.Error("有到期项时必须判定为有到期任务，否则调度循环会按触发时间空等")
	}
	if h.Len() != 2 {
		t.Errorf("HasDue 是只读查询，不得弹出任何项，剩余 %d", h.Len())
	}
}

func TestQuaternaryHeap_LargeDataset(t *testing.T) {
	h := NewQuaternaryHeap()
	now := time.Now()
	n := 1000

	// Push items in reverse order
	for i := n; i > 0; i-- {
		h.PushItem(&mockItem{
			id:        string(rune(i)),
			triggerAt: now.Add(time.Duration(i) * time.Second),
		})
	}

	if h.Len() != n {
		t.Errorf("Expected length %d, got %d", n, h.Len())
	}

	// Pop all and verify sorted
	prev := popTop(h)
	count := 1
	for h.Len() > 0 {
		current := popTop(h)
		if current.GetTriggerTime().Before(prev.GetTriggerTime()) {
			t.Error("Items not in sorted order")
			break
		}
		prev = current
		count++
	}

	if count != n {
		t.Errorf("Expected to pop %d items, got %d", n, count)
	}
}
