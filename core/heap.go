package core

type DelayedItem interface {
	GetPriority() int64
	SetIndex(int)
	GetIndex() int
}

type DelayQueue interface {
	Push(item DelayedItem)
	Pop() DelayedItem
	Peek() DelayedItem
	Remove(item DelayedItem) DelayedItem
	Len() int
	IsEmpty() bool
	Update(item DelayedItem, priority int64)
}
