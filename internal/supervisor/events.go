package supervisor

import (
	"sync"
	"time"
)

// EventType identifies what happened to an app.
type EventType string

const (
	EventStatusChanged  EventType = "status_changed"
	EventHealthFailure  EventType = "health_failure"
	EventRestart        EventType = "restart"
	EventOOMKill        EventType = "oom_kill"
)

// Event is a single state transition for one app.
type Event struct {
	Type           EventType `json:"type"`
	AppID          string    `json:"app_id"`
	Status         string    `json:"status,omitempty"`
	PID            int       `json:"pid,omitempty"`
	RestartCount   int       `json:"restart_count,omitempty"`
	HealthFailures int64     `json:"health_failures,omitempty"`
	Timestamp      time.Time `json:"ts"`
}

// EventBus is a fan-out pub/sub for app events. Subscribers receive
// events on a channel; unsubscribe closes the channel. Safe for
// concurrent use.
type EventBus struct {
	mu   sync.RWMutex
	subs map[uint64]chan Event
	seq  uint64
}

// NewEventBus creates a ready-to-use bus.
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[uint64]chan Event)}
}

// Subscribe returns a channel that receives events for all apps.
// Buffer size controls backpressure — slow consumers drop events.
// Call Unsubscribe with the returned ID when done.
func (b *EventBus) Subscribe(bufSize int) (id uint64, ch <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	c := make(chan Event, bufSize)
	b.subs[b.seq] = c
	return b.seq, c
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *EventBus) Unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		close(ch)
		delete(b.subs, id)
	}
}

// Publish sends an event to all subscribers. Non-blocking: if a
// subscriber's buffer is full, the event is dropped for that
// subscriber (backpressure).
func (b *EventBus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
