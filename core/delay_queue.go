package core

import (
	"sync"
	"time"
)

type DelayQueue struct {
	heap     DelayQueue
	mu       sync.RWMutex
	cond     *sync.Cond
	closed   bool
	heapType HeapType
}

func NewDelayQueue(heapType HeapType) *DelayQueue {
	var strategy HeapStrategy

	switch heapType {
	case BinaryHeapType:
		strategy = &BinaryHeapStrategy{}
	case QuadHeapType:
		strategy = &QuadHeapStrategy{}
	default:
		strategy = &BinaryHeapStrategy{}
	}

	factory := NewHeapFactory(strategy)
	dq := &DelayQueue{
		heap:     factory.CreateQueue(),
		heapType: heapType,
		closed:   false,
	}
	dq.cond = sync.NewCond(&dq.mu)
	return dq
}

func (dq *DelayQueue) Offer(task *Task) bool {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	if dq.closed {
		return false
	}

	dq.heap.Push(task)
	if dq.heap.Peek() == task {
		dq.cond.Signal()
	}
	return true
}

func (dq *DelayQueue) Poll() *Task {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	for !dq.closed {
		first := dq.heap.Peek()
		if first == nil {
			dq.cond.Wait()
			continue
		}

		delay := first.GetPriority() - time.Now().UnixNano()
		if delay <= 0 {
			return dq.heap.Pop().(*Task)
		}

		dq.cond.Wait()
	}
	return nil
}

func (dq *DelayQueue) PollWithTimeout(timeout time.Duration) *Task {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for !dq.closed {
		first := dq.heap.Peek()
		if first == nil {
			select {
			case <-timer.C:
				return nil
			default:
				dq.cond.Wait()
			}
			continue
		}

		delay := time.Duration(first.GetPriority() - time.Now().UnixNano())
		if delay <= 0 {
			return dq.heap.Pop().(*Task)
		}

		if timeout < delay {
			delay = timeout
		}

		timer.Reset(delay)
		select {
		case <-timer.C:
			if dq.heap.Peek() == first {
				return dq.heap.Pop().(*Task)
			}
		}
	}
	return nil
}

func (dq *DelayQueue) Remove(task *Task) bool {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	if dq.closed {
		return false
	}

	return dq.heap.Remove(task) != nil
}

func (dq *DelayQueue) Size() int {
	dq.mu.RLock()
	defer dq.mu.RUnlock()
	return dq.heap.Len()
}

func (dq *DelayQueue) IsEmpty() bool {
	dq.mu.RLock()
	defer dq.mu.RUnlock()
	return dq.heap.IsEmpty()
}

func (dq *DelayQueue) Close() {
	dq.mu.Lock()
	defer dq.mu.Unlock()

	dq.closed = true
	dq.cond.Broadcast()
}
