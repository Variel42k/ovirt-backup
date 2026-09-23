package backup

import (
	"context"
	"sync"
	"time"
)

// readPacer ограничивает скорость чтения с хранилища ВМ.
//
// Один на запуск: диски ВМ читаются параллельно, а нагрузка ложится на одно и
// то же хранилище, поэтому предел общий. Каждое чтение заранее резервирует
// своё место в очереди, так что средняя скорость не превышает предела, а
// всплеск ограничен одним блоком.
type readPacer struct {
	mu   sync.Mutex
	rate float64 // байт в секунду
	next time.Time
}

// newReadPacer возвращает nil для «без ограничения»: nil-ограничитель ничего
// не ждёт.
func newReadPacer(limitMBps int) *readPacer {
	if limitMBps <= 0 {
		return nil
	}
	return &readPacer{rate: float64(limitMBps) * (1 << 20)}
}

// Wait резервирует n байт и ждёт своей очереди.
func (p *readPacer) Wait(ctx context.Context, n int64) error {
	if p == nil || n <= 0 {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	start := p.next
	p.next = p.next.Add(time.Duration(float64(n) / p.rate * float64(time.Second)))
	p.mu.Unlock()

	wait := time.Until(start)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ReadLimit — ограничение чтения для запуска в МиБ/с: из задания, иначе
// значение службы; 0 — без ограничения.
func (r RunRequest) ReadLimit(def int) int {
	if r.MaxReadMBps > 0 {
		return r.MaxReadMBps
	}
	if def > 0 {
		return def
	}
	return 0
}
