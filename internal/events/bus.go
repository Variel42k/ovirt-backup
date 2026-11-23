// Пакет events представляет собой внутрипроцессный вентилятор изменений состояния,
// используемый для отправки обновлений подключенным браузерам через сервер events.
package events

import (
	"sync"
	"time"
)

// Kind называет тип события, чтобы UI мог решить, что обновлять.
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

// Event уведомление.
type Event struct {
	Kind     Kind      `json:"kind"`
	ServerID string    `json:"server_id,omitempty"`
	ObjectID string    `json:"object_id,omitempty"`
	Message  string    `json:"message,omitempty"`
	Payload  any       `json:"payload,omitempty"`
	At       time.Time `json:"at"`
}

// Поклонники автобусов вышли к подписчикам.
//
// Переполненная очередь подписчика пропускает событие, чтобы отключившийся браузер не
// задерживал монитор. При переподключении интерфейс снова запрашивает данные.
type Bus struct {
	mu     sync.RWMutex
	nextID int
	subs   map[int]chan Event
	buffer int
}

// NewBus строит шину. буфер — глубина очереди для подписчика.
func NewBus(buffer int) *Bus {
	if buffer <= 0 {
		buffer = 64
	}
	return &Bus{subs: map[int]chan Event{}, buffer: buffer}
}

// Subscribe регистрирует слушателя и возвращает его канал и отменяет функт.
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

// Publish доставляет событие всем текущим подписчикам.
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
			// Подписчик остался позади; Пропустить его, а не замедлять всех вниз.
		}
	}
}

// Subscribers отчита́тель м. —ница ж. кто отчитывается, для диагностики.
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
