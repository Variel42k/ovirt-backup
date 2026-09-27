package libvirtx

import (
	"time"

	"golang.org/x/crypto/ssh"
)

// Keepalive соединений пула.
//
// NAT и межсетевые экраны закрывают TCP-соединения, по которым долго ничего
// не идёт, — часто молча. Соединение пула простаивает между опросами
// мониторинга и бэкапами, и пул узнавал бы о закрытии только при следующей
// операции. Запрос keepalive@openssh.com раз в KeepaliveInterval держит
// соединение живым. Он идёт по уже открытому соединению и при обычном уровне
// журнала sshd не оставляет в журнале хоста ни строки.
//
// Keepalive соединение не обрывает: под нагрузкой ответ может запоздать, а
// обрыв уронил бы идущий бэкап. Мёртвое соединение по-прежнему находит
// проверка пула перед выдачей; keepalive только сообщает, что хост молчит.

// KeepaliveInterval — как часто пул напоминает о себе хосту.
const KeepaliveInterval = 30 * time.Second

// keepaliveSilentAfter — после скольких интервалов без ответа сообщать, что
// хост молчит.
const keepaliveSilentAfter = 3

// StartKeepalive запускает keepalive до закрытия соединения. onSilent
// вызывается один раз за период молчания, если хост не ответил
// keepaliveSilentAfter интервалов подряд. Повторный вызов ничего не делает.
func (c *Conn) StartKeepalive(interval time.Duration, onSilent func()) {
	c.mu.Lock()
	if c.closed || c.ssh == nil || c.stop != nil {
		c.mu.Unlock()
		return
	}
	c.stop = make(chan struct{})
	stop, client := c.stop, c.ssh
	c.mu.Unlock()
	go keepalive(stop, client, interval, onSilent)
}

// stopKeepaliveLocked останавливает keepalive; вызывается из Close под c.mu.
func (c *Conn) stopKeepaliveLocked() {
	if c.stop != nil {
		close(c.stop)
		c.stop = nil
	}
}

func keepalive(stop <-chan struct{}, client *ssh.Client, interval time.Duration, onSilent func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Новый запрос не уходит, пока не ответили на прошлый: на мёртвом
	// соединении иначе копились бы горутины, по одной на интервал.
	var inflight chan error
	missed, reported := 0, false
	for {
		select {
		case <-stop:
			return
		case err := <-inflight:
			inflight = nil
			if err != nil {
				// Соединение закрыто: ответа больше не будет, keepalive не нужен.
				return
			}
			missed, reported = 0, false
		case <-ticker.C:
			if inflight != nil {
				missed++
				if missed >= keepaliveSilentAfter && !reported && onSilent != nil {
					reported = true
					onSilent()
				}
				continue
			}
			ch := make(chan error, 1)
			inflight = ch
			go func() {
				// Отказ сервера (want-reply=false) — тоже ответ: хост жив.
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				ch <- err
			}()
		}
	}
}
