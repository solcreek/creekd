package supervisor

import (
	"testing"
	"time"
)

func TestEventBusPublishSubscribe(t *testing.T) {
	bus := NewEventBus()
	id, ch := bus.Subscribe(10)
	defer bus.Unsubscribe(id)

	e := Event{Type: EventStatusChanged, AppID: "test", Status: "running", Timestamp: time.Now()}
	bus.Publish(e)

	select {
	case got := <-ch:
		if got.AppID != "test" {
			t.Errorf("AppID = %q, want %q", got.AppID, "test")
		}
		if got.Type != EventStatusChanged {
			t.Errorf("Type = %q, want %q", got.Type, EventStatusChanged)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestEventBusMultipleSubscribers(t *testing.T) {
	bus := NewEventBus()
	id1, ch1 := bus.Subscribe(10)
	id2, ch2 := bus.Subscribe(10)
	defer bus.Unsubscribe(id1)
	defer bus.Unsubscribe(id2)

	bus.Publish(Event{Type: EventRestart, AppID: "app1", Timestamp: time.Now()})

	for _, ch := range []<-chan Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.AppID != "app1" {
				t.Errorf("AppID = %q, want %q", got.AppID, "app1")
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive event")
		}
	}
}

func TestEventBusUnsubscribe(t *testing.T) {
	bus := NewEventBus()
	id, ch := bus.Subscribe(10)
	bus.Unsubscribe(id)

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Error("channel should be closed after Unsubscribe")
	}
}

func TestEventBusBackpressure(t *testing.T) {
	bus := NewEventBus()
	id, ch := bus.Subscribe(1) // buffer of 1
	defer bus.Unsubscribe(id)

	// Publish 3 events — first fills buffer, rest are dropped.
	for i := 0; i < 3; i++ {
		bus.Publish(Event{Type: EventStatusChanged, AppID: "app", Timestamp: time.Now()})
	}

	// Should get exactly 1.
	select {
	case <-ch:
	default:
		t.Fatal("expected at least 1 event")
	}

	select {
	case <-ch:
		t.Fatal("expected buffer to be drained after 1 read")
	default:
	}
}

func TestEventBusDoubleUnsubscribe(t *testing.T) {
	bus := NewEventBus()
	id, _ := bus.Subscribe(1)
	bus.Unsubscribe(id)
	bus.Unsubscribe(id) // should not panic
}
