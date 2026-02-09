package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEventBusSubscribeAndPublish(t *testing.T) {
	eb := NewEventBus(10)
	defer eb.Close()

	// 订阅特定事件
	subID, ch := eb.Subscribe(EventJobScheduled, EventJobCompleted)
	assert.NotEmpty(t, subID)

	// 发布被订阅的事件
	eb.Publish(Event{Type: EventJobScheduled, JobID: "1"})
	eb.Publish(Event{Type: EventJobStarted, JobID: "2"}) // 未订阅
	eb.Publish(Event{Type: EventJobCompleted, JobID: "3"})

	// 验证收到的事件
	select {
	case ev := <-ch:
		assert.Equal(t, EventJobScheduled, ev.Type)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for event")
	}

	select {
	case ev := <-ch:
		assert.Equal(t, EventJobCompleted, ev.Type)
		assert.Equal(t, "3", ev.JobID)
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for event")
	}

	// 验证未收到未订阅的事件
	select {
	case <-ch:
		t.Fatal("Should not receive unsubscribed event")
	case <-time.After(100 * time.Millisecond):
		// 正确，超时说明没收到
	}
}

func TestEventBusBufferFull(t *testing.T) {
	eb := NewEventBus(1) // 很小的缓冲区
	defer eb.Close()

	_, ch := eb.Subscribe(EventJobScheduled)

	// 快速发布多个事件（可能丢包但不应阻塞）
	for i := 0; i < 100; i++ {
		eb.Publish(Event{Type: EventJobScheduled, JobID: string(rune(i))})
	}

	// 验证至少收到一些（缓冲区里的）
	received := 0
	done := time.After(500 * time.Millisecond)

	for {
		select {
		case <-ch:
			received++
		case <-done:
			assert.True(t, received > 0, "Should receive at least some events")
			return
		}
	}
}
