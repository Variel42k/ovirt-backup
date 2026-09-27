package backup

import (
	"context"
	"sync"
	"time"
)

// Guard следит за условием, пока копируются диски.
//
// Горячий бэкап держит открытой точку, и пока она открыта, записи гостя
// где-то копятся: в scratch-файле на хосте, в scratch-дисках или слое
// снапшота на домене хранения. Сторож раз в интервал вызывает проверку и,
// если та вернула причину, отменяет копирование с этой причиной — бэкап
// закрывается раньше, чем пострадает работающая ВМ.
type Guard struct {
	quit chan struct{}
	done chan struct{}
	once sync.Once

	mu    sync.Mutex
	fired error
}

// StartGuard вызывает check раз в interval, пока не остановлен или пока не
// отменён ctx. Непустая ошибка из check отменяет копирование через stop с
// этой причиной и завершает сторожа.
func StartGuard(ctx context.Context, interval time.Duration, check func(context.Context) error,
	stop context.CancelCauseFunc) *Guard {

	g := &Guard{quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(g.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-g.quit:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if err := check(ctx); err != nil {
				g.mu.Lock()
				g.fired = err
				g.mu.Unlock()
				stop(err)
				return
			}
		}
	}()
	return g
}

// Stop останавливает сторожа и возвращает причину, если он сработал.
// Вызывать можно повторно и после того, как сторож вышел сам.
func (g *Guard) Stop() error {
	g.once.Do(func() { close(g.quit) })
	<-g.done
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fired
}
