package backup

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Заморозка могла ещё идти: разморозка повторяется, пока агент не освободится.
func TestThawAfterFailedFreezeRetriesWhilePending(t *testing.T) {
	calls := 0
	err := thawAfterFailedFreeze(context.Background(), true, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("агент занят")
		}
		return nil
	}, time.Second, time.Millisecond)
	if err != nil || calls != 3 {
		t.Fatalf("разморозка: %v после %d попыток, ожидался успех с третьей", err, calls)
	}
}

// Агент прямо отказал в заморозке — он её не выполнял, хватает одной попытки.
func TestThawAfterFailedFreezeSingleAttemptWhenNotPending(t *testing.T) {
	calls := 0
	err := thawAfterFailedFreeze(context.Background(), false, func(context.Context) error {
		calls++
		return errors.New("агент недоступен")
	}, time.Second, time.Millisecond)
	if err == nil || calls != 1 {
		t.Fatalf("ожидалась одна неудачная попытка, было %d: %v", calls, err)
	}
}

// Повторы ограничены окном: вечно ждать агента нельзя.
func TestThawAfterFailedFreezeGivesUpAfterWindow(t *testing.T) {
	calls := 0
	started := time.Now()
	err := thawAfterFailedFreeze(context.Background(), true, func(context.Context) error {
		calls++
		return errors.New("агент не отвечает")
	}, 20*time.Millisecond, 5*time.Millisecond)
	if err == nil {
		t.Fatal("ожидалась ошибка после исчерпания окна")
	}
	if calls < 2 || time.Since(started) > time.Second {
		t.Fatalf("попыток %d за %s", calls, time.Since(started))
	}
}

// Отменённый запуск не отменяет разморозку.
func TestThawAfterFailedFreezeIgnoresCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := thawAfterFailedFreeze(ctx, false, func(attempt context.Context) error {
		return attempt.Err()
	}, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("разморозка получила отменённый контекст: %v", err)
	}
}
