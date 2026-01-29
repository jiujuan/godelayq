package core

import (
	"sync/atomic"
	"time"
)

type Task struct {
	ID        string
	Data      interface{}
	ExecuteAt int64 // 任务的执行时间戳（UnixNano）
	index     int   // 在堆中的索引
}

var taskIDCounter uint64

func NewTask(data interface{}, delay time.Duration) *Task {
	id := atomic.AddUint64(&taskIDCounter, 1)
	return &Task{
		ID:        string(id),
		Data:      data,
		ExecuteAt: time.Now().Add(delay).UnixNano(),
		index:     -1,
	}
}

func (t *Task) GetPriority() int64 {
	return t.ExecuteAt
}

func (t *Task) SetIndex(i int) {
	t.index = i
}

func (t *Task) GetIndex() int {
	return t.index
}
