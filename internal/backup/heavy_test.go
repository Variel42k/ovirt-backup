package backup

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/config"
)

func newHeavyEngine(limit int) *Engine {
	return NewEngine(nil, nil, config.BackupConfig{HeavyWorkers: limit}, nil, zerolog.Nop())
}

// Больше heavy_workers одновременных операций начаться не должно: обе они
// читают цепочку целиком из хранилища, и десять параллельных чтений мешают не
// сервису, а хранилищу вместе с идущими бэкапами.
func TestHeavyLimitsConcurrency(t *testing.T) {
	const limit = 2
	e := newHeavyEngine(limit)
	ctx := context.Background()

	var (
		mu      sync.Mutex
		now     int
		highest int
	)
	release := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.acquireHeavy(ctx); err != nil {
				t.Error(err)
				return
			}
			defer e.releaseHeavy()

			mu.Lock()
			now++
			if now > highest {
				highest = now
			}
			mu.Unlock()

			<-release

			mu.Lock()
			now--
			mu.Unlock()
		}()
	}

	// Дать претендентам занять все места и упереться в предел.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		reached := now == limit
		mu.Unlock()
		if reached {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("за две секунды очередь не заполнилась до %d", limit)
		default:
			time.Sleep(time.Millisecond)
		}
	}

	close(release)
	wg.Wait()

	if highest > limit {
		t.Fatalf("одновременно выполнялось %d операций при пределе %d", highest, limit)
	}
}

// Операция, отменённая пока стояла в очереди, не должна начаться позже, когда
// до неё дойдёт черёд: за час ожидания причина запроса могла исчезнуть.
func TestHeavyRespectsCancellation(t *testing.T) {
	e := newHeavyEngine(1)

	if err := e.acquireHeavy(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer e.releaseHeavy()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.acquireHeavy(ctx) }()

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("место в очереди выдано отменённой операции")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ожидание не прервалось вместе с контекстом")
	}
}

// Нулевой или отрицательный предел не должен превращаться в вечную блокировку.
func TestHeavyZeroLimitStillRuns(t *testing.T) {
	for _, limit := range []int{0, -3} {
		e := newHeavyEngine(limit)
		if err := e.acquireHeavy(context.Background()); err != nil {
			t.Fatalf("предел %d: %v", limit, err)
		}
		e.releaseHeavy()
	}
}
