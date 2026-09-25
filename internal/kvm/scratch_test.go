package kvm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const gib = int64(1) << 30

func TestScratchReserve(t *testing.T) {
	for _, tc := range []struct {
		name string
		free int64
		want int64
	}{
		{"место неизвестно — только наблюдать", 0, 0},
		{"большой том — 5 %", 100 * gib, 5 * gib},
		{"средний том — не меньше 1 ГиБ", 10 * gib, gib},
		{"почти полный том — не больше половины свободного", 1536 << 20, 768 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scratchReserve(tc.free); got != tc.want {
				t.Errorf("scratchReserve(%d) = %d, ожидалось %d", tc.free, got, tc.want)
			}
		})
	}
}

// fakeScratch отдаёт замеры по очереди, последний повторяет.
type fakeScratch struct {
	mu      sync.Mutex
	samples []scratchSample
	calls   int
}

func (f *fakeScratch) sample(context.Context) scratchSample {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	if i >= len(f.samples) {
		i = len(f.samples) - 1
	}
	f.calls++
	return f.samples[i]
}

func (f *fakeScratch) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Гость пишет, scratch растёт, место на томе тает. Сторож обязан закрыть
// бэкап, как только свободного станет меньше запаса, — раньше, чем запись
// гостя начнёт получать ошибки.
func TestScratchGuardStopsBackupBeforeSpaceRunsOut(t *testing.T) {
	fake := &fakeScratch{samples: []scratchSample{
		{free: 20 * gib, freeKnown: true, used: gib, usedKnown: true},
		{free: 8 * gib, freeKnown: true, used: 13 * gib, usedKnown: true},
		{free: gib, freeKnown: true, used: 20 * gib, usedKnown: true},
	}}
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)

	guard := startScratchGuard(ctx, time.Millisecond, 2*gib, fake.sample, stop, zerolog.Nop())
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("сторож не закрыл бэкап, хотя место опустилось ниже запаса")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errScratchLow) {
		t.Fatalf("копирование отменено не сторожем: %v", cause)
	}

	peak, fired := guard.Stop()
	if !errors.Is(fired, errScratchLow) {
		t.Errorf("Stop должен вернуть причину срабатывания, получено %v", fired)
	}
	if peak != 20*gib {
		t.Errorf("пик scratch = %d, ожидалось %d", peak, 20*gib)
	}
}

// Без известного запаса (свободное место на старте неизвестно) сторож только
// наблюдает: закрыть бэкап без точки отсчёта значило бы ронять его наугад.
func TestScratchGuardWithoutReserveOnlyWatches(t *testing.T) {
	fake := &fakeScratch{samples: []scratchSample{
		{free: 100 << 20, freeKnown: true, used: 3 * gib, usedKnown: true},
	}}
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)

	guard := startScratchGuard(ctx, time.Millisecond, 0, fake.sample, stop, zerolog.Nop())
	waitForSamples(t, fake, 5)

	peak, fired := guard.Stop()
	if fired != nil || ctx.Err() != nil {
		t.Fatalf("без запаса сторож не должен закрывать бэкап: %v", fired)
	}
	if peak != 3*gib {
		t.Errorf("пик scratch = %d, ожидалось %d", peak, 3*gib)
	}
}

// Неудачный замер свободного места — не повод закрывать бэкап.
func TestScratchGuardIgnoresUnknownFreeSpace(t *testing.T) {
	fake := &fakeScratch{samples: []scratchSample{{freeKnown: false}}}
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)

	guard := startScratchGuard(ctx, time.Millisecond, 2*gib, fake.sample, stop, zerolog.Nop())
	waitForSamples(t, fake, 5)

	if _, fired := guard.Stop(); fired != nil || ctx.Err() != nil {
		t.Fatalf("сторож сработал без замера: %v", fired)
	}
}

// Stop можно вызвать и после того, как сторож уже сработал и вышел сам.
func TestScratchGuardStopIsSafeAfterFiring(t *testing.T) {
	fake := &fakeScratch{samples: []scratchSample{{free: 1, freeKnown: true}}}
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)

	guard := startScratchGuard(ctx, time.Millisecond, gib, fake.sample, stop, zerolog.Nop())
	<-ctx.Done()
	if _, fired := guard.Stop(); fired == nil {
		t.Fatal("ожидалась причина срабатывания")
	}
	if _, fired := guard.Stop(); fired == nil {
		t.Fatal("повторный Stop должен вернуть ту же причину")
	}
}

func waitForSamples(t *testing.T, fake *fakeScratch, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for fake.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("сторож сделал %d замеров из %d", fake.count(), n)
		}
		time.Sleep(time.Millisecond)
	}
}
