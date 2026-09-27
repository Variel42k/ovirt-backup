package backup

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlaceLimiterDisabledByDefault(t *testing.T) {
	for _, l := range []*PlaceLimiter{nil, NewPlaceLimiter(0)} {
		for i := 0; i < 10; i++ {
			release, err := l.Acquire(context.Background(), []string{"dom"}, func() { t.Fatal("без предела ждать нечего") })
			if err != nil {
				t.Fatal(err)
			}
			defer release()
		}
	}
}

func TestPlaceLimiterWaitsForFreeSlot(t *testing.T) {
	l := NewPlaceLimiter(1)
	first, err := l.Acquire(context.Background(), []string{"dom"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var waited atomic.Bool
	acquired := make(chan func(), 1)
	go func() {
		release, err := l.Acquire(context.Background(), []string{"dom"}, func() { waited.Store(true) })
		if err != nil {
			t.Error(err)
			return
		}
		acquired <- release
	}()

	select {
	case <-acquired:
		t.Fatal("второй бэкап того же места не должен начаться, пока идёт первый")
	case <-time.After(50 * time.Millisecond):
	}
	first()
	first() // повторный вызов безопасен и не освобождает чужое место
	select {
	case release := <-acquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("место освободилось, а второй бэкап всё ждёт")
	}
	if !waited.Load() {
		t.Fatal("ожидание должно быть видно запуску")
	}

	// Другое место очередь не делит.
	a, _ := l.Acquire(context.Background(), []string{"a"}, nil)
	b, err := l.Acquire(context.Background(), []string{"b"}, func() { t.Fatal("разные места ждать друг друга не должны") })
	if err != nil {
		t.Fatal(err)
	}
	a()
	b()
}

func TestPlaceLimiterCancelReleasesTakenSlots(t *testing.T) {
	l := NewPlaceLimiter(1)
	holdB, _ := l.Acquire(context.Background(), []string{"b"}, nil)

	// Запуск на «a» и «b»: «a» займёт, на «b» будет ждать и получит отмену.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, []string{"b", "a", "a"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline", err)
	}
	holdB()

	// «a» освобождено после отмены: его можно занять сразу.
	release, err := l.Acquire(context.Background(), []string{"a"}, func() { t.Fatal("место «a» осталось занятым после отмены") })
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestPlaceLimiterNoCircularWait(t *testing.T) {
	// Два запуска с одними и теми же местами в разном порядке: без общего
	// порядка каждый занял бы своё первое место и ждал бы второе вечно.
	l := NewPlaceLimiter(1)
	done := make(chan struct{})
	for _, keys := range [][]string{{"x", "y"}, {"y", "x"}} {
		go func(keys []string) {
			for i := 0; i < 200; i++ {
				release, err := l.Acquire(context.Background(), keys, nil)
				if err != nil {
					t.Error(err)
					return
				}
				release()
			}
			done <- struct{}{}
		}(keys)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("запуски ждут друг друга по кругу")
		}
	}
}
