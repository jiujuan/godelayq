package core

type HeapType int

const (
	BinaryHeapType HeapType = iota
	QuadHeapType
)

type HeapStrategy interface {
	Create() DelayQueue
}

type BinaryHeapStrategy struct{}

func (s *BinaryHeapStrategy) Create() DelayQueue {
	return NewBinaryHeap()
}

type QuadHeapStrategy struct{}

func (s *QuadHeapStrategy) Create() DelayQueue {
	return NewQuadHeap()
}

type HeapFactory struct {
	strategy HeapStrategy
}

func NewHeapFactory(strategy HeapStrategy) *HeapFactory {
	return &HeapFactory{strategy: strategy}
}

func (f *HeapFactory) CreateQueue() DelayQueue {
	return f.strategy.Create()
}
