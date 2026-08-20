// Package events is an in-process fan-out of state changes, used to push live
// updates to connected browsers over server-sent events.
package events

import (
	"sync"
	"time"
)

// Kind names an event type so the UI can decide what to refresh.
type Kind string

const (
	KindServerState   Kind = "server_state"
	KindInventory     Kind = "inventory"
	KindAlert         Kind = "alert"
	KindRemediation   Kind = "remediation"
	KindBackupRun     Kind = "backup_run"
	KindVerifyRun     Kind = "verify_run"
	KindRestoreRun    Kind = "restore_run"
	KindStorageTarget Kind = "storage_target"
	KindJob           Kind = "job"
	KindReplication   Kind = "replication"
	KindSettings      Kind = "settings.changed"
)

// Event is one notification.
type Event struct {
	Kind     Kind      `json:"kind"`
	ServerID string    `json:"server_id,omitempty"`
	ObjectID string    `json:"object_id,omitempty"`
	Message  string    `json:"message,omitempty"`
	Payload  any       `json:"payload,omitempty"`
	At       time.Time `json:"at"`
}

// Bus fans events out to subscribers.
//
// Delivery is deliberately lossy: a browser that stopped reading must not be
// able to stall the monitor loop, so a full subscriber queue drops the event
// rather than blocking. The UI refetches on reconnect anyway.
type Bus struct {
	mu     sync.RWMutex
	nextID int
	subs   map[int]chan Event
	buffer int
}

// NewBus builds a bus. buffer is the per-subscriber queue depth.
func NewBus(buffer int) *Bus {
	if buffer <= 0 {
		buffer = 64
	}
	return &Bus{subs: map[int]chan Event{}, buffer: buffer}
}

// Subscribe registers a listener and returns its channel and a cancel func.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.nextID
	b.nextID++
	ch := make(chan Event, b.buffer)
	b.subs[id] = ch

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
	}
}

// Publish delivers an event to every current subscriber.
func (b *Bus) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// Subscriber is behind; skip it rather than slowing everyone down.
		}
	}
}

// Subscribers reports how many listeners are attached, for diagnostics.
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
