package backup

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Точка зафиксирована вовремя: сторож молчит, уровень остаётся заявленным.
func TestFreezeWindowThawInTime(t *testing.T) {
	var thaws atomic.Int32
	w := NewFreezeWindow(time.Now(), time.Hour, func() error { thaws.Add(1); return nil }, nil)
	if err := w.Thaw(); err != nil {
		t.Fatal(err)
	}
	if w.Expired() {
		t.Fatal("окно не истекало")
	}
	level, note, err := w.Settle(model.ConsistencyApplication, model.ConsistencyApplication, true)
	if err != nil || level != model.ConsistencyApplication || note != "" {
		t.Fatalf("уровень изменён: %s %q %v", level, note, err)
	}
	if thaws.Load() != 1 {
		t.Fatalf("разморозок: %d", thaws.Load())
	}
}

// Гипервизор завис с точкой: гость размораживается сам, не дожидаясь его,
// а копия честно понижается до crash.
func TestFreezeWindowExpiresAndThawsGuest(t *testing.T) {
	thawed := make(chan struct{}, 1)
	expired := make(chan error, 1)
	w := NewFreezeWindow(time.Now(), 20*time.Millisecond, func() error {
		select {
		case thawed <- struct{}{}:
		default:
		}
		return nil
	}, func(err error) { expired <- err })

	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("сторож не разморозил гостя")
	}
	<-thawed
	if !w.Expired() {
		t.Fatal("истечение не отмечено")
	}

	level, note, err := w.Settle(model.ConsistencyApplication, model.ConsistencyApplication, false)
	if err != nil || level != model.ConsistencyCrash || note == "" {
		t.Fatalf("ожидалось понижение до crash: %s %q %v", level, note, err)
	}
	if _, _, err := w.Settle(model.ConsistencyApplication, model.ConsistencyApplication, true); err == nil {
		t.Fatal("строгое задание должно прерваться")
	}
}

// Заморозка давно началась: сторож срабатывает сразу, а не через полный срок.
func TestFreezeWindowCountsFromFreeze(t *testing.T) {
	done := make(chan struct{})
	NewFreezeWindow(time.Now().Add(-time.Minute), time.Second, func() error { return nil },
		func(error) { close(done) })
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("отсчёт идёт не от момента заморозки")
	}
}

// Гость не заморожен — отсчитывать нечего.
func TestFreezeWindowIdleWithoutFreeze(t *testing.T) {
	w := NewFreezeWindow(time.Time{}, time.Millisecond, func() error { return nil },
		func(error) { t.Error("сторож сработал без заморозки") })
	time.Sleep(20 * time.Millisecond)
	if w.Expired() {
		t.Fatal("истечение без заморозки")
	}
}

func TestFreezeLimitFallsBack(t *testing.T) {
	if got := (RunRequest{MaxFreeze: 10 * time.Second}).FreezeLimit(time.Minute); got != 10*time.Second {
		t.Fatalf("предел задания потерян: %s", got)
	}
	if got := (RunRequest{}).FreezeLimit(30 * time.Second); got != 30*time.Second {
		t.Fatalf("предел службы потерян: %s", got)
	}
	if got := (RunRequest{}).FreezeLimit(0); got != DefaultMaxFreeze {
		t.Fatalf("предел по умолчанию: %s", got)
	}
}
