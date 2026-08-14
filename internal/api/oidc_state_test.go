package api

import (
	"strconv"
	"testing"
	"time"
)

// Начатый вход отдаётся ровно один раз. Повтор того же адреса возврата — это
// либо кнопка «назад», либо чужая попытка переиграть перехваченный код, и
// второй сессии не должно появиться ни в том, ни в другом случае.
func TestOIDCPendingIsOneShot(t *testing.T) {
	pending := newOIDCPending()
	pending.begin("ключ", oidcLogin{state: "state", nonce: "nonce", verifier: "verifier"})

	login, ok := pending.take("ключ")
	if !ok {
		t.Fatal("начатый вход не найден")
	}
	if login.state != "state" || login.nonce != "nonce" || login.verifier != "verifier" {
		t.Errorf("вход вернулся изменённым: %+v", login)
	}
	if _, ok := pending.take("ключ"); ok {
		t.Error("тот же вход выдан второй раз")
	}
}

// Брошенная вкладка не должна оставаться годной для входа неделю.
func TestOIDCPendingExpires(t *testing.T) {
	now := time.Now()
	pending := newOIDCPending()
	pending.now = func() time.Time { return now }

	pending.begin("ключ", oidcLogin{state: "state"})

	now = now.Add(oidcPendingTTL - time.Second)
	if _, ok := pending.take("ключ"); !ok {
		t.Fatal("вход отвергнут до истечения срока")
	}

	pending.begin("другой", oidcLogin{state: "state"})
	now = now.Add(oidcPendingTTL + time.Second)
	if _, ok := pending.take("другой"); ok {
		t.Error("принят вход с истёкшим сроком")
	}
}

// Начать вход может кто угодно без аутентификации. Таблица обязана иметь
// предел, иначе повторные обращения к /auth/oidc/start занимали бы память
// службы до отказа.
func TestOIDCPendingStaysBounded(t *testing.T) {
	pending := newOIDCPending()
	for i := range oidcPendingMax + 200 {
		pending.begin(strconv.Itoa(i), oidcLogin{state: "state"})
	}
	if len(pending.items) > oidcPendingMax {
		t.Errorf("в таблице %d записей при пределе %d", len(pending.items), oidcPendingMax)
	}
}
