package api

import (
	"fmt"
	"testing"
	"time"
)

// clock даёт тестам управляемое время: ждать настоящую минуту паузы в тесте
// значит либо не проверять её вовсе, либо сделать тест самым медленным в наборе.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter() (*loginLimiter, *clock) {
	c := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	l := newLoginLimiter()
	l.now = c.now
	return l, c
}

func TestLimiterAllowsUntilThreshold(t *testing.T) {
	l, _ := newTestLimiter()

	for i := 0; i < loginFailureThreshold-1; i++ {
		if ok, _ := l.Allow("admin"); !ok {
			t.Fatalf("вход закрыт после %d неудач, а порог %d", i, loginFailureThreshold)
		}
		l.Fail("admin")
	}
	if ok, _ := l.Allow("admin"); !ok {
		t.Fatal("вход закрыт до достижения порога")
	}

	l.Fail("admin") // порог достигнут
	ok, retry := l.Allow("admin")
	if ok {
		t.Fatal("после порога вход должен быть приостановлен")
	}
	if retry <= 0 || retry > loginBaseCooldown {
		t.Fatalf("неожиданная пауза: %s", retry)
	}
}

func TestLimiterReleasesAfterCooldown(t *testing.T) {
	l, c := newTestLimiter()
	for i := 0; i < loginFailureThreshold; i++ {
		l.Fail("admin")
	}
	if ok, _ := l.Allow("admin"); ok {
		t.Fatal("пауза не включилась")
	}

	c.add(loginBaseCooldown + time.Second)
	if ok, _ := l.Allow("admin"); !ok {
		t.Fatal("пауза истекла, но вход всё ещё закрыт")
	}
}

// Пауза должна расти, иначе после первой паузы подбор продолжится с той же
// скоростью, только с минутными перерывами.
func TestLimiterCooldownGrowsAndIsCapped(t *testing.T) {
	l, c := newTestLimiter()

	var prev time.Duration
	for i := 0; i < 20; i++ {
		l.Fail("admin")
		_, retry := l.Allow("admin")
		if retry > loginMaxCooldown {
			t.Fatalf("пауза %s превысила потолок %s", retry, loginMaxCooldown)
		}
		if retry < prev {
			t.Fatalf("пауза уменьшилась: было %s, стало %s", prev, retry)
		}
		prev = retry
		c.add(retry)
	}
	if prev != loginMaxCooldown {
		t.Fatalf("пауза должна была дойти до потолка %s, дошла до %s", loginMaxCooldown, prev)
	}
}

func TestLimiterResetOnSuccess(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < loginFailureThreshold; i++ {
		l.Fail("admin")
	}
	l.Reset("admin")
	if ok, _ := l.Allow("admin"); !ok {
		t.Fatal("успешный вход должен снимать паузу")
	}
}

// Смена регистра не должна давать новый запас попыток.
func TestLimiterIgnoresCase(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < loginFailureThreshold; i++ {
		l.Fail("Admin")
	}
	if ok, _ := l.Allow("  aDmIn "); ok {
		t.Fatal("другой регистр обошёл ограничение")
	}
}

// Счётчик забывается сам, иначе пять опечаток за год закрыли бы вход.
func TestLimiterForgetsOldFailures(t *testing.T) {
	l, c := newTestLimiter()
	for i := 0; i < loginFailureThreshold-1; i++ {
		l.Fail("admin")
	}
	c.add(loginFailureWindow + time.Minute)
	l.Fail("admin")
	if ok, _ := l.Allow("admin"); !ok {
		t.Fatal("давние неудачи не должны складываться со свежими")
	}
}

// Перебор несуществующих имён не должен раздувать таблицу без предела.
func TestLimiterBoundedMemory(t *testing.T) {
	l, c := newTestLimiter()
	for i := 0; i < loginLimiterMaxEntries*2; i++ {
		l.Fail(fmt.Sprintf("кто-то-%d", i))
		if i%64 == 0 {
			c.add(time.Second)
		}
	}
	if len(l.users) > loginLimiterMaxEntries {
		t.Fatalf("таблица разрослась до %d записей при пределе %d", len(l.users), loginLimiterMaxEntries)
	}
}
