package backup

import (
	"context"
	"time"
)

// Разморозка после неудачной заморозки.
//
// Ошибка вызова заморозки не всегда значит, что гость не заморожен. Если вызов
// оборвался по тайм-ауту — libvirt, VDSM или движок не дождались агента, —
// агент мог закончить сценарии и заморозить файловые системы уже после
// ответа. Тогда разморозку никто не пришлёт, и гость останется стоять.
//
// Поэтому после любой ошибки заморозки служба размораживает гостя сама: для
// незамороженного гостя это ничего не меняет. Если заморозка могла ещё идти
// (pending), одна попытка может прийти, пока агент занят заморозкой, и не
// дойти до него: агент выполняет команды по очереди. Тогда разморозка
// повторяется, пока не пройдёт, но не дольше thawRetryWindow.

const (
	thawRetryWindow   = 3 * time.Minute
	thawRetryInterval = 10 * time.Second
	// thawAttemptTimeout — предел одной попытки разморозки.
	thawAttemptTimeout = 60 * time.Second
)

// ThawAfterFailedFreeze размораживает гостя после неудачной заморозки; см.
// комментарий выше. Контекст отвязан от отмены запуска: отменённый бэкап тоже
// обязан не оставить гостя замороженным.
func ThawAfterFailedFreeze(ctx context.Context, pending bool, thaw func(context.Context) error) error {
	return thawAfterFailedFreeze(ctx, pending, thaw, thawRetryWindow, thawRetryInterval)
}

func thawAfterFailedFreeze(ctx context.Context, pending bool, thaw func(context.Context) error,
	window, interval time.Duration) error {

	base := context.WithoutCancel(ctx)
	deadline := time.Now().Add(window)
	for {
		attemptCtx, cancel := context.WithTimeout(base, thawAttemptTimeout)
		err := thaw(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		if !pending || time.Now().Add(interval).After(deadline) {
			return err
		}
		time.Sleep(interval)
	}
}
