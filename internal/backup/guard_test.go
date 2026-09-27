package backup

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuardStopsCopyWithCause(t *testing.T) {
	low := errors.New("места мало")
	var calls atomic.Int32
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)

	g := StartGuard(ctx, time.Millisecond, func(context.Context) error {
		if calls.Add(1) >= 3 {
			return low
		}
		return nil
	}, stop)

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("сторож не отменил копирование")
	}
	if !errors.Is(context.Cause(ctx), low) {
		t.Fatalf("причина отмены = %v", context.Cause(ctx))
	}
	if err := g.Stop(); !errors.Is(err, low) {
		t.Fatalf("Stop = %v, ожидалась причина срабатывания", err)
	}
	if err := g.Stop(); !errors.Is(err, low) {
		t.Fatalf("повторный Stop = %v", err)
	}
}

func TestGuardQuietUntilStopped(t *testing.T) {
	var calls atomic.Int32
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)

	g := StartGuard(ctx, time.Millisecond, func(context.Context) error {
		calls.Add(1)
		return nil
	}, stop)
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := g.Stop(); err != nil || ctx.Err() != nil {
		t.Fatalf("сторож без нарушений не должен ничего отменять: %v", err)
	}
	after := calls.Load()
	time.Sleep(10 * time.Millisecond)
	if calls.Load() != after {
		t.Fatal("после Stop сторож продолжает проверять")
	}
}
