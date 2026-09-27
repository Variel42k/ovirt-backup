// Package sshpool держит SSH-соединения к хостам открытыми между операциями.
//
// Каждое новое SSH-соединение оставляет в журнале хоста несколько строк: вход
// по ключу, открытие и закрытие PAM-сессии, сессию systemd-logind. Служба
// ходит к хостам часто — статистика СУБД во время бэкапа, проверка канала
// данных перед каждым бэкапом Proxmox, проверка хранилищ по расписанию, — и
// соединение на каждую операцию засоряло бы журнал хоста сотнями входов в
// час, в которых теряются настоящие.
//
// Пул держит соединение открытым и открывает на нём сессии для следующих
// операций. Новое соединение появляется, только если прежнее умерло,
// простаивало дольше Idle или на нём уже MaxSessions сессий. Каналы сессий
// sshd при обычном уровне журнала (INFO) не записывает.
package sshpool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// DefaultIdle — сколько соединение без сессий ждёт следующей операции.
	// Покрывает паузы между ВМ одного задания и между замерами статистики,
	// но не держит соединение с ночи до ночи.
	DefaultIdle = 5 * time.Minute
	// DefaultMaxSessions — сессий на одно соединение. У sshd по умолчанию
	// MaxSessions 10; запас на случай, если на хосте его уменьшили не сильно.
	DefaultMaxSessions = 8
	// checkAfter — соединение, простоявшее дольше, перед выдачей проверяется
	// keepalive-запросом: NAT или межсетевой экран могли тихо его закрыть.
	checkAfter = 30 * time.Second
	// checkTimeout — сколько ждать ответа на проверку.
	checkTimeout = 10 * time.Second
)

// Dialer открывает новое соединение, когда в пуле нет годного.
type Dialer func(ctx context.Context) (*ssh.Client, error)

// Key собирает ключ пула из всего, что определяет подключение. Секреты в
// ключе не хранятся — только их отпечаток.
func Key(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Pool — соединения по ключам. Ключ должен меняться вместе со всем, что
// влияет на подключение: адресом, пользователем, ключом, закреплённым ключом
// хоста. Тогда правка настроек сама уводит операции на новое соединение.
type Pool struct {
	idle        time.Duration
	maxSessions int

	mu      sync.Mutex
	entries map[string][]*entry
	closed  bool
}

type entry struct {
	key      string
	client   *ssh.Client
	active   int
	lastUsed time.Time
	dead     bool
	timer    *time.Timer
}

// New создаёт пул. Нулевые значения — DefaultIdle и DefaultMaxSessions.
func New(idle time.Duration, maxSessions int) *Pool {
	if idle <= 0 {
		idle = DefaultIdle
	}
	if maxSessions <= 0 {
		maxSessions = DefaultMaxSessions
	}
	return &Pool{idle: idle, maxSessions: maxSessions, entries: map[string][]*entry{}}
}

var (
	sharedOnce sync.Once
	shared     *Pool
)

// Shared — общий пул службы. Транспорты создаются на каждую операцию, а
// соединения должны переживать их, поэтому пул один на процесс.
func Shared() *Pool {
	sharedOnce.Do(func() { shared = New(DefaultIdle, DefaultMaxSessions) })
	return shared
}

// Lease — соединение, выданное одной операции. Release или Discard вызываются
// ровно один раз, когда операция закончила пользоваться соединением.
type Lease struct {
	Client *ssh.Client
	pool   *Pool
	e      *entry
	once   sync.Once
}

// Release возвращает соединение в пул.
func (l *Lease) Release() { l.once.Do(func() { l.pool.release(l.e, false) }) }

// Discard сообщает, что соединение негодно: оно закрывается, как только им
// перестанут пользоваться другие операции, и больше не выдаётся.
func (l *Lease) Discard() { l.once.Do(func() { l.pool.release(l.e, true) }) }

// Acquire выдаёт соединение по ключу: живое из пула или новое через dial.
func (p *Pool) Acquire(ctx context.Context, key string, dial Dialer) (*Lease, error) {
	for {
		e, needCheck, err := p.pick(key)
		if err != nil {
			return nil, err
		}
		if e == nil {
			break
		}
		if !needCheck || alive(ctx, e.client) {
			return &Lease{Client: e.client, pool: p, e: e}, nil
		}
		p.release(e, true)
	}

	client, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	e := &entry{key: key, client: client, active: 1, lastUsed: time.Now()}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = client.Close()
		return nil, errors.New("пул SSH-соединений закрыт")
	}
	p.entries[key] = append(p.entries[key], e)
	p.mu.Unlock()
	return &Lease{Client: client, pool: p, e: e}, nil
}

// pick занимает место на подходящем соединении. needCheck — оно простаивало
// и перед выдачей его надо проверить.
func (p *Pool) pick(key string) (*entry, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, errors.New("пул SSH-соединений закрыт")
	}
	for _, e := range p.entries[key] {
		if e.dead || e.active >= p.maxSessions {
			continue
		}
		if e.timer != nil {
			e.timer.Stop()
			e.timer = nil
		}
		e.active++
		return e, e.active == 1 && time.Since(e.lastUsed) > checkAfter, nil
	}
	return nil, false, nil
}

func (p *Pool) release(e *entry, broken bool) {
	p.mu.Lock()
	e.active--
	e.lastUsed = time.Now()
	if broken && !e.dead {
		e.dead = true
		p.removeLocked(e)
	}
	closeNow := e.active <= 0 && (e.dead || p.closed)
	if !closeNow && e.active <= 0 {
		e.timer = time.AfterFunc(p.idle, func() { p.expire(e) })
	}
	p.mu.Unlock()
	if closeNow {
		_ = e.client.Close()
	}
}

// expire закрывает соединение, простоявшее Idle без сессий.
func (p *Pool) expire(e *entry) {
	p.mu.Lock()
	if e.active > 0 || e.dead || time.Since(e.lastUsed) < p.idle {
		p.mu.Unlock()
		return
	}
	e.dead = true
	p.removeLocked(e)
	p.mu.Unlock()
	_ = e.client.Close()
}

func (p *Pool) removeLocked(e *entry) {
	list := p.entries[e.key]
	for i, candidate := range list {
		if candidate == e {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(p.entries, e.key)
	} else {
		p.entries[e.key] = list
	}
}

// Session открывает сессию на соединении из пула. Если соединение из пула
// умерло, повторяет один раз на новом. done закрывает сессию и возвращает
// соединение; broken=true — соединение оборвалось посреди операции.
func (p *Pool) Session(ctx context.Context, key string, dial Dialer) (session *ssh.Session,
	done func(broken bool), err error) {

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		lease, err := p.Acquire(ctx, key, dial)
		if err != nil {
			return nil, nil, err
		}
		s, err := lease.Client.NewSession()
		if err == nil {
			return s, func(broken bool) {
				_ = s.Close()
				if broken {
					lease.Discard()
				} else {
					lease.Release()
				}
			}, nil
		}
		lease.Discard()
		lastErr = err
	}
	return nil, nil, fmt.Errorf("открытие SSH-сессии: %w", lastErr)
}

// Close закрывает все соединения; занятые — как только их освободят.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	var idle []*ssh.Client
	for key, list := range p.entries {
		for _, e := range list {
			if e.timer != nil {
				e.timer.Stop()
			}
			if e.active <= 0 {
				idle = append(idle, e.client)
			}
		}
		delete(p.entries, key)
	}
	p.mu.Unlock()
	for _, c := range idle {
		_ = c.Close()
	}
}

// Size — сколько соединений держит пул; для тестов и диагностики.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, list := range p.entries {
		n += len(list)
	}
	return n
}

// alive проверяет соединение keepalive-запросом, как это делает OpenSSH.
// Ответ «не поддерживается» тоже ответ: соединение живо.
func alive(ctx context.Context, client *ssh.Client) bool {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil
	case <-ctx.Done():
		return false
	}
}
