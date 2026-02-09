package core

import (
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
		item := h.PopItem()
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
	item := h.PopItem()
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
	prev := h.PopItem()
	for h.Len() > 0 {
		current := h.PopItem()
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
	h.Update(updatedItem)
	
	// item2 should now be at top
	top := h.Peek()
	if top.GetID() != "item2" {
		t.Errorf("Expected item2 at top after update, got %s", top.GetID())
	}
	
	// Update non-existent item (should not panic)
	h.Update(&mockItem{id: "nonexistent", triggerAt: now})
	
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
		current := h.PopItem()
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
					id:        string(rune('a'+offset*itemsPerGoroutine+j)),
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
				if item := h.PopItem(); item != nil {
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
		item := h.PopItem()
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
	prev := h.PopItem()
	count := 1
	for h.Len() > 0 {
		current := h.PopItem()
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
