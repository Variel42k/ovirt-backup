package backup

import (
	"fmt"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// DefaultMaxFreeze — предел окна заморозки, если его не задали ни задание, ни
// конфигурация.
const DefaultMaxFreeze = 60 * time.Second

// FreezeLimit возвращает предел окна заморозки для запуска: из задания, иначе
// значение службы по умолчанию.
func (r RunRequest) FreezeLimit(def time.Duration) time.Duration {
	if r.MaxFreeze > 0 {
		return r.MaxFreeze
	}
	if def > 0 {
		return def
	}
	return DefaultMaxFreeze
}

// FreezeWindow ограничивает, сколько гость стоит замороженным.
//
// Пока файловые системы заморожены, запись в госте стоит целиком. Обычная ВМ
// это переживает, а узел Kubernetes — нет: etcd не может сделать fsync и
// теряет лидерство, kube-controller-manager и kube-scheduler не продлевают
// аренду и перезапускаются, kubelet перестаёт отмечаться, и через 40 с узел
// становится NotReady. Точку же движок фиксирует не мгновенно: oVirt готовит
// scratch-диски, и фаза initializing может длиться минутами.
//
// Когда окно истекает раньше, чем точка зафиксирована, сторож размораживает
// гостя сам. Копия от этого не портится — она просто перестаёт быть
// согласованной и становится такой, как после сбоя питания; вызывающий узнаёт
// об этом из Expired и понижает уровень или прерывает запуск.
type FreezeWindow struct {
	mu      sync.Mutex
	thaw    func() error
	timer   *time.Timer
	expired bool
	limit   time.Duration
}

// NewFreezeWindow запускает отсчёт от момента заморозки. thaw вызывается под
// замком сторожа, поэтому может менять состояние вызывающего без своего;
// повторный вызов должен быть безопасен. onExpire получает результат
// разморозки по истечении окна и вызывается вне замка.
//
// Нулевой момент заморозки означает, что гость не заморожен: сторож тогда
// ничего не отсчитывает, а Thaw просто зовёт thaw.
func NewFreezeWindow(frozenAt time.Time, limit time.Duration, thaw func() error,
	onExpire func(error)) *FreezeWindow {

	w := &FreezeWindow{thaw: thaw, limit: limit}
	if frozenAt.IsZero() || limit <= 0 {
		return w
	}
	wait := limit - time.Since(frozenAt)
	if wait < 0 {
		wait = 0
	}
	w.timer = time.AfterFunc(wait, func() {
		w.mu.Lock()
		w.expired = true
		err := w.thaw()
		w.mu.Unlock()
		if onExpire != nil {
			onExpire(err)
		}
	})
	return w
}

// Thaw размораживает гостя и останавливает отсчёт. Безопасен при повторном
// вызове и одновременно со сторожем.
func (w *FreezeWindow) Thaw() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	return w.thaw()
}

// Expired сообщает, что гостя разморозил сторож, а не вызывающий. Проверять
// надо после Thaw: так гонка между сторожем и фиксацией точки решается в
// пользу осторожного ответа.
func (w *FreezeWindow) Expired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.expired
}

// Limit — предел окна, с которым сторож запущен.
func (w *FreezeWindow) Limit() time.Duration { return w.limit }

// ExpiredNote — причина понижения уровня для запуска.
func (w *FreezeWindow) ExpiredNote() string {
	return fmt.Sprintf("гипервизор не зафиксировал точку за %s заморозки; гость разморожен досрочно, "+
		"чтобы не остановить приложения", w.limit.Round(time.Second))
}

// Settle решает, что делать с запуском, окно которого истекло: при строгом
// требовании — ошибка, иначе понижение до crash с причиной. Если окно не
// истекало, возвращает достигнутый уровень без изменений.
func (w *FreezeWindow) Settle(reached model.Consistency, target model.Consistency, require bool) (model.Consistency, string, error) {
	if !w.Expired() {
		return reached, "", nil
	}
	note := w.ExpiredNote()
	if require {
		return model.ConsistencyCrash, note, fmt.Errorf("задание требует согласованности уровня «%s», но %s; копия не сохраняется",
			target.Title(), note)
	}
	return model.ConsistencyCrash, note + "; копия снята как после сбоя питания", nil
}
