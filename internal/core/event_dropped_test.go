package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// EventBus.Dropped（G4/Q05）：满即丢是既有契约，本组用例钉的是"丢弃有计数"。
// 缓冲 2、发 5 条，订阅者不消费：通道里 2 条、计数 3。
func TestEventBusDroppedCountsWhenSubscriberBufferFull(t *testing.T) {
	eb := NewEventBus(2)
	defer eb.Close()

	_, ch := eb.Subscribe(EventJobStarted)
	assert.Equal(t, int64(0), eb.Dropped())

	for i := 0; i < 5; i++ {
		eb.Publish(Event{Type: EventJobStarted, JobID: "j"})
	}
	assert.Equal(t, 2, len(ch), "缓冲里的两条该在")
	assert.Equal(t, int64(3), eb.Dropped(), "超出的三条要计入丢弃")

	// 继续发布计数继续增长：单调，不因订阅者开始消费而清零。
	for i := 0; i < 2; i++ {
		eb.Publish(Event{Type: EventJobStarted, JobID: "j"})
	}
	assert.Equal(t, int64(5), eb.Dropped())
}

// SubscribeAll 的订阅者缓冲满同样计入：一条事件对多个订阅者丢弃时逐个计数，
// 读数说的是"丢了多少条投递"，不是"丢了多少条事件"。
func TestEventBusDroppedCountsAllSubscriberSeparately(t *testing.T) {
	eb := NewEventBus(1)
	defer eb.Close()

	_, typed := eb.Subscribe(EventJobFailed)
	_, all := eb.SubscribeAll()

	eb.Publish(Event{Type: EventJobFailed, JobID: "j"}) // 两个通道各收 1，缓冲满
	eb.Publish(Event{Type: EventJobFailed, JobID: "j"}) // 两个通道各丢 1

	assert.Equal(t, 1, len(typed))
	assert.Equal(t, 1, len(all))
	assert.Equal(t, int64(2), eb.Dropped())
}
