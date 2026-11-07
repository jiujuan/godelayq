package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventBusSubscribeAndPublish(t *testing.T) {
	eb := NewEventBus(10)
	defer eb.Close()

	subID, ch := eb.Subscribe(EventJobScheduled, EventJobCompleted)
	assert.NotEmpty(t, subID)

	eb.Publish(Event{Type: EventJobScheduled, JobID: "1"})
	eb.Publish(Event{Type: EventJobStarted, JobID: "2"})
	eb.Publish(Event{Type: EventJobCompleted, JobID: "3"})

	assertReceiveEvent(t, ch, EventJobScheduled, "1")
	assertReceiveEvent(t, ch, EventJobCompleted, "3")
	assertNoEvent(t, ch)
}

func TestEventBusSubscribeAll(t *testing.T) {
	eb := NewEventBus(10)
	defer eb.Close()

	id, ch := eb.SubscribeAll()
	require.NotEmpty(t, id)

	eb.Publish(Event{Type: EventJobScheduled, JobID: "scheduled"})
	eb.Publish(Event{Type: EventJobFailed, JobID: "failed"})

	assertReceiveEvent(t, ch, EventJobScheduled, "scheduled")
	assertReceiveEvent(t, ch, EventJobFailed, "failed")
}

func TestEventBusUnsubscribeSpecificEventType(t *testing.T) {
	eb := NewEventBus(10)
	defer eb.Close()

	id, ch := eb.Subscribe(EventJobScheduled, EventJobCompleted)

	eb.Unsubscribe(id, EventJobScheduled)
	eb.Publish(Event{Type: EventJobScheduled, JobID: "skip"})
	eb.Publish(Event{Type: EventJobCompleted, JobID: "keep"})

	assertReceiveEvent(t, ch, EventJobCompleted, "keep")
	assertNoEvent(t, ch)
}

func TestEventBusUnsubscribeAllClosesChannel(t *testing.T) {
	eb := NewEventBus(10)

	id, ch := eb.Subscribe(EventJobScheduled, EventJobCompleted)
	eb.Unsubscribe(id)

	assertChannelClosed(t, ch)

	eb.Publish(Event{Type: EventJobScheduled, JobID: "ignored"})
	assertNoEvent(t, ch)
}

func TestEventBusUnsubscribeAllSubscription(t *testing.T) {
	eb := NewEventBus(10)

	id, ch := eb.SubscribeAll()
	eb.Unsubscribe(id)

	assertChannelClosed(t, ch)

	eb.Publish(Event{Type: EventJobCompleted, JobID: "ignored"})
	assertNoEvent(t, ch)
}

func TestEventBusUnsubscribeUnknownIDDoesNothing(t *testing.T) {
	eb := NewEventBus(10)
	defer eb.Close()

	_, ch := eb.Subscribe(EventJobScheduled)
	eb.Unsubscribe("missing-id")
	eb.Publish(Event{Type: EventJobScheduled, JobID: "still-received"})

	assertReceiveEvent(t, ch, EventJobScheduled, "still-received")
}

func TestEventBusUnsubscribeDoesNotAffectOtherSubscribers(t *testing.T) {
	eb := NewEventBus(10)
	defer eb.Close()

	id1, ch1 := eb.Subscribe(EventJobScheduled)
	_, ch2 := eb.Subscribe(EventJobScheduled)

	eb.Unsubscribe(id1)
	assertChannelClosed(t, ch1)

	eb.Publish(Event{Type: EventJobScheduled, JobID: "other-subscriber"})
	assertReceiveEvent(t, ch2, EventJobScheduled, "other-subscriber")
}

func TestEventBusBufferFull(t *testing.T) {
	eb := NewEventBus(1)
	defer eb.Close()

	_, ch := eb.Subscribe(EventJobScheduled)

	for i := 0; i < 100; i++ {
		eb.Publish(Event{Type: EventJobScheduled, JobID: string(rune(i))})
	}

	received := 0
	done := time.After(500 * time.Millisecond)

	for {
		select {
		case <-ch:
			received++
		case <-done:
			assert.True(t, received > 0, "should receive at least some events")
			return
		}
	}
}

func TestEventBusCloseClosesAllChannelsOnce(t *testing.T) {
	eb := NewEventBus(10)

	id1, ch1 := eb.Subscribe(EventJobScheduled)
	id2, ch2 := eb.SubscribeAll()

	eb.Unsubscribe(id1, EventJobScheduled)
	assertChannelClosed(t, ch1)

	eb.Close()

	assertChannelClosed(t, ch2)
	eb.Unsubscribe(id2)
}

func assertReceiveEvent(t *testing.T, ch <-chan Event, eventType EventType, jobID string) {
	t.Helper()

	select {
	case ev, ok := <-ch:
		require.True(t, ok, "expected event channel to stay open")
		assert.Equal(t, eventType, ev.Type)
		assert.Equal(t, jobID, ev.JobID)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func assertNoEvent(t *testing.T, ch <-chan Event) {
	t.Helper()

	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected event received: %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func assertChannelClosed(t *testing.T, ch <-chan Event) {
	t.Helper()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for closed channel")
	}
}
