package api

import (
	"sync"
	"time"
)

const (
	// oidcPendingTTL — сколько живёт начатый вход. Столько времени человеку
	// хватает на ввод пароля и второй фактор у провайдера; всё, что дольше,
	// почти наверняка брошенная вкладка.
	oidcPendingTTL = 10 * time.Minute
	// oidcPendingMax ограничивает таблицу. Начать вход может кто угодно без
	// всякой аутентификации, поэтому без предела повторные обращения к
	// /auth/oidc/start были бы способом занять память службы.
	oidcPendingMax = 1024
)

// oidcLogin — начатый вход, ожидающий возврата от провайдера.
type oidcLogin struct {
	// state уходит провайдеру и возвращается в адресе; сверяется с тем, что
	// лежит в куке браузера.
	state string
	// nonce связывает токен с этим самым входом: провайдер кладёт его внутрь
	// подписанного токена, и подставить чужой токен становится нельзя.
	nonce string
	// verifier — секрет PKCE. Провайдеру уходит только его хеш, поэтому
	// перехваченный код обменять без этого значения невозможно.
	verifier string
	// redirect — куда вернуть человека после входа.
	redirect string
	expires  time.Time
}

// oidcPending хранит начатые входы.
//
// В памяти, а не в базе и не в куке: nonce и секрет PKCE не должны попадать в
// браузер вовсе, а пережить перезапуск им не нужно — потеряется лишь вход,
// который прямо сейчас идёт, и он повторяется нажатием кнопки. Служба
// запускается в одном экземпляре, делить эту таблицу не с кем.
type oidcPending struct {
	mu    sync.Mutex
	items map[string]oidcLogin
	now   func() time.Time // подменяется в тестах
}

func newOIDCPending() *oidcPending {
	return &oidcPending{items: make(map[string]oidcLogin), now: time.Now}
}

// begin запоминает начатый вход. Ключ — идентификатор из куки браузера.
func (p *oidcPending) begin(key string, login oidcLogin) {
	now := p.now()
	login.expires = now.Add(oidcPendingTTL)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictLocked(now)
	p.items[key] = login
}

// take возвращает начатый вход и сразу забывает его.
//
// Одноразовость здесь — часть защиты: код от провайдера обменивается ровно
// один раз, и повтор того же адреса не должен заводить вторую сессию.
func (p *oidcPending) take(key string) (oidcLogin, bool) {
	now := p.now()

	p.mu.Lock()
	defer p.mu.Unlock()

	login, ok := p.items[key]
	if !ok {
		return oidcLogin{}, false
	}
	delete(p.items, key)
	if now.After(login.expires) {
		return oidcLogin{}, false
	}
	return login, true
}

// evictLocked освобождает место: сначала протухшее, а если таблица всё равно
// полна — самый старый вход. Вызывается с захваченным mu.
func (p *oidcPending) evictLocked(now time.Time) {
	if len(p.items) < oidcPendingMax {
		return
	}
	for key, login := range p.items {
		if now.After(login.expires) {
			delete(p.items, key)
		}
	}
	if len(p.items) < oidcPendingMax {
		return
	}
	oldestKey, oldest := "", time.Time{}
	for key, login := range p.items {
		if oldest.IsZero() || login.expires.Before(oldest) {
			oldestKey, oldest = key, login.expires
		}
	}
	if oldestKey != "" {
		delete(p.items, oldestKey)
	}
}
