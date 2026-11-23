package libvirtx

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// ServerLoader извлекает определение соединения по идентификатору. Принимая его в качестве
// функции, этот пакет остается независимым от магазина.
type ServerLoader func(ctx context.Context, serverID string) (*model.Server, error)

// Пул поддерживает один живой SSH+libvirt сеанс на гипервизор.
//
// Состояние SSH-соединения может пропасть без уведомления при перезагрузке хоста, сбросе
// NAT или перезапуске libvirtd. Перед выдачей соединения проверяем его и переподключаемся
// при сбое.
type Pool struct {
	load    ServerLoader
	timeout time.Duration
	log     zerolog.Logger

	mu      sync.Mutex
	entries map[string]*poolEntry
}

type poolEntry struct {
	conn      *Conn
	updatedAt time.Time
}

// NewPool строит пул. тайм-аут ограничивает настройку соединения и liveness зондов.
func NewPool(load ServerLoader, timeout time.Duration, log zerolog.Logger) *Pool {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Pool{
		load:    load,
		timeout: timeout,
		log:     log,
		entries: map[string]*poolEntry{},
	}
}

// Get возвращает рабочее соединение с гипервизором и при необходимости подключается заново.
func (p *Pool) Get(ctx context.Context, serverID string) (*Conn, error) {
	srv, err := p.load(ctx, serverID)
	if err != nil {
		return nil, err
	}
	return p.ForServer(ctx, srv)
}

// ForServer возвращает соединение для уже загруженного определения.
func (p *Pool) ForServer(ctx context.Context, srv *model.Server) (*Conn, error) {
	if srv == nil {
		return nil, fmt.Errorf("сервер не задан")
	}
	if !srv.Kind.UsesLibvirt() {
		return nil, fmt.Errorf("подключение %q имеет тип %q, а не libvirt", srv.Name, srv.Kind)
	}
	if !srv.Enabled {
		return nil, fmt.Errorf("сервер %q отключён", srv.Name)
	}

	p.mu.Lock()
	entry, cached := p.entries[srv.ID]
	// Если после подключения изменились учётные данные или адрес хоста, кэшированный сеанс
	// относится к старым настройкам.
	if cached && entry.updatedAt.Before(srv.UpdatedAt) {
		delete(p.entries, srv.ID)
		p.mu.Unlock()
		go entry.conn.Close()
		p.mu.Lock()
		cached = false
	}
	p.mu.Unlock()

	if cached {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := entry.conn.Alive(probeCtx)
		cancel()
		if err == nil {
			return entry.conn, nil
		}
		p.log.Debug().Err(err).Str("гипервизор", srv.Name).
			Msg("соединение с гипервизором не отвечает — переподключаюсь")
		p.Invalidate(srv.ID)
	}

	conn, err := Connect(ctx, ConfigFromServer(srv))
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	// Другой goroutine можетъ подключаться, пока мы набирали; Оставь себе.
	if existing, ok := p.entries[srv.ID]; ok {
		p.mu.Unlock()
		go conn.Close()
		return existing.conn, nil
	}
	p.entries[srv.ID] = &poolEntry{conn: conn, updatedAt: srv.UpdatedAt}
	p.mu.Unlock()

	// Соединение пула живёт долго и простаивает между операциями: keepalive
	// не даёт NAT и межсетевому экрану закрыть его молча.
	name := srv.Name
	conn.StartKeepalive(KeepaliveInterval, func() {
		p.log.Warn().Str("гипервизор", name).
			Msg("гипервизор не отвечает на keepalive — соединение, вероятно, оборвано; пул переподключится при следующей операции")
	})

	return conn, nil
}

// ConfigFromServer отображает сохраненное соединение на транспортную настройку.
func ConfigFromServer(srv *model.Server) Config {
	return Config{
		Host:            srv.SSHHost,
		Port:            srv.SSHPort,
		User:            srv.Username,
		Password:        srv.Password,
		PrivateKey:      srv.SSHPrivateKey,
		HostKey:         srv.SSHHostKey,
		TrustAnyHostKey: srv.SSHTrustAnyHostKey,
	}
}

// Invalidate отбрасывает кэшированное соединение для одного гипервизора.
func (p *Pool) Invalidate(serverID string) {
	p.mu.Lock()
	entry, ok := p.entries[serverID]
	delete(p.entries, serverID)
	p.mu.Unlock()

	if ok {
		go entry.conn.Close()
	}
}

// Close срыва́ть каждую кэшированную сессию.
func (p *Pool) Close() {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]*poolEntry{}
	p.mu.Unlock()

	for _, entry := range entries {
		_ = entry.conn.Close()
	}
}
