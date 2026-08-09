package api

import (
	"strings"
	"sync"
	"time"
)

// Пороги подбора пароля.
//
// Считаем по учётной записи, а не по адресу: clientIP берёт X-Forwarded-For,
// а этот заголовок подделывается одной строкой, и счётчик по адресу
// обходился бы сменой значения на каждом запросе. Имя учётной записи
// подменить нельзя — атакуют именно её.
const (
	// loginFailureThreshold — сколько неудач подряд допускается, прежде чем
	// включится пауза. Пять — чтобы обычная опечатка и повторный ввод не
	// приводили к блокировке.
	loginFailureThreshold = 5
	// loginBaseCooldown — пауза после превышения порога. Дальше удваивается.
	loginBaseCooldown = time.Minute
	// loginMaxCooldown — потолок паузы. Ограничение важно: без него забытый
	// пароль администратора превращался бы в блокировку на сутки, то есть
	// защита от подбора становилась бы способом отключить вход.
	loginMaxCooldown = 15 * time.Minute
	// loginFailureWindow — через сколько бездействия счётчик неудач
	// обнуляется сам.
	loginFailureWindow = 30 * time.Minute
	// loginLimiterMaxEntries ограничивает размер таблицы. Записи заводятся и
	// на несуществующие имена, поэтому без предела перебор случайных логинов
	// был бы способом занять всю память.
	loginLimiterMaxEntries = 4096
)

// loginLimiter притормаживает подбор пароля.
//
// Именно притормаживает, а не блокирует: учётная запись не запирается
// навсегда, пауза растёт до потолка и сама сходит на нет. Жёсткая блокировка
// после N попыток защищает от подбора, но дарит любому желающему возможность
// заблокировать администратора, зная только его логин.
type loginLimiter struct {
	mu    sync.Mutex
	users map[string]*loginAttempts
	now   func() time.Time // подменяется в тестах
}

type loginAttempts struct {
	failures  int
	blockedTo time.Time
	seen      time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		users: make(map[string]*loginAttempts),
		now:   time.Now,
	}
}

// key приводит имя к единому виду: иначе Admin и admin считались бы разными
// учётными записями и порог удваивался бы простой сменой регистра.
func loginKey(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// Allow сообщает, можно ли сейчас проверять пароль этой учётной записи.
// Второе значение — сколько ждать до следующей попытки.
func (l *loginLimiter) Allow(username string) (bool, time.Duration) {
	key := loginKey(username)
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	st, ok := l.users[key]
	if !ok {
		return true, 0
	}
	if now.Before(st.blockedTo) {
		return false, st.blockedTo.Sub(now).Round(time.Second)
	}
	// Пауза истекла: пробуем снова. Счётчик не сбрасываем — если подбор
	// продолжится, следующая пауза должна быть длиннее, а не такой же.
	return true, 0
}

// Fail отмечает неудачную попытку и при необходимости включает паузу.
func (l *loginLimiter) Fail(username string) {
	key := loginKey(username)
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	st, ok := l.users[key]
	if !ok {
		l.evictLocked(now)
		st = &loginAttempts{}
		l.users[key] = st
	}
	// Давняя неудача к текущей серии не относится.
	if !st.seen.IsZero() && now.Sub(st.seen) > loginFailureWindow {
		st.failures = 0
	}
	st.failures++
	st.seen = now

	if st.failures >= loginFailureThreshold {
		over := st.failures - loginFailureThreshold
		cooldown := loginBaseCooldown << min(over, 8)
		if cooldown > loginMaxCooldown || cooldown <= 0 {
			cooldown = loginMaxCooldown
		}
		st.blockedTo = now.Add(cooldown)
	}
}

// Reset забывает историю после успешного входа.
func (l *loginLimiter) Reset(username string) {
	key := loginKey(username)
	l.mu.Lock()
	delete(l.users, key)
	l.mu.Unlock()
}

// evictLocked освобождает место перед добавлением новой записи: сначала
// выбрасывает всё протухшее, а если таблица всё равно полна — самую старую.
// Вызывается с захваченным mu.
func (l *loginLimiter) evictLocked(now time.Time) {
	if len(l.users) < loginLimiterMaxEntries {
		return
	}
	for k, st := range l.users {
		if now.After(st.blockedTo) && now.Sub(st.seen) > loginFailureWindow {
			delete(l.users, k)
		}
	}
	if len(l.users) < loginLimiterMaxEntries {
		return
	}
	oldestKey, oldest := "", time.Time{}
	for k, st := range l.users {
		if oldest.IsZero() || st.seen.Before(oldest) {
			oldestKey, oldest = k, st.seen
		}
	}
	if oldestKey != "" {
		delete(l.users, oldestKey)
	}
}
