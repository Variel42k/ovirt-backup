package ovirt

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// ServerLoader извлекает определение соединения по идентификатору. Пул принимает его как
// функцию, чтобы этот пакет не должен был знать о магазине.
type ServerLoader func(ctx context.Context, serverID string) (*model.Server, error)

// Pool сохраняет один аутентифицированный клиент на каждый движок.
//
// Сохраняем клиент между частыми опросами. При изменении учётных данных создаём новый
// клиент, а изменения runtime-полей вроде last_checked_at не должны менять SSO-токен.
type Pool struct {
	load    ServerLoader
	timeout time.Duration
	log     zerolog.Logger

	mu      sync.Mutex
	entries map[string]*poolEntry
}

type poolEntry struct {
	client *Client
	key    connectionKey
}

// connectionKey намеренно исключает состояние, инвентаризацию и отображение метаданных.
// Он остается приватным, поскольку содержит уже имеющиеся в памяти учётные данные.
type connectionKey struct {
	kind        model.ServerKind
	engineURL   string
	username    string
	password    string
	caCert      string
	insecureTLS bool
}

func connectionKeyFor(srv *model.Server) connectionKey {
	return connectionKey{
		kind:        srv.Kind,
		engineURL:   srv.EngineURL,
		username:    srv.Username,
		password:    srv.Password,
		caCert:      srv.CACert,
		insecureTLS: srv.InsecureTLS,
	}
}

// NewPool строит пул. тайм-аут применяется к отдельным API вызовам.
func NewPool(load ServerLoader, timeout time.Duration, log zerolog.Logger) *Pool {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Pool{
		load:    load,
		timeout: timeout,
		log:     log,
		entries: map[string]*poolEntry{},
	}
}

// Get возвращает клиент для движка, создавая его, если это необходимо. Соединение,
// определение которого изменилось с момента создания клиента, перестраивается, поэтому
// немедленно вступает в силу редактирование учётных данных аутентификации в UI.
func (p *Pool) Get(ctx context.Context, serverID string) (*Client, error) {
	srv, err := p.load(ctx, serverID)
	if err != nil {
		return nil, err
	}
	return p.ForServer(srv)
}

// ForServer возвращает клиент для уже загруженного определения.
func (p *Pool) ForServer(srv *model.Server) (*Client, error) {
	if srv == nil {
		return nil, fmt.Errorf("сервер не задан")
	}
	if !srv.Kind.UsesOVirtAPI() {
		return nil, fmt.Errorf("подключение %q имеет тип %q, а не oVirt-совместимый", srv.Name, srv.Kind)
	}
	if !srv.Enabled {
		return nil, fmt.Errorf("сервер %q отключён", srv.Name)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	key := connectionKeyFor(srv)
	if entry, ok := p.entries[srv.ID]; ok && entry.key == key {
		return entry.client, nil
	}

	client, err := New(Config{
		EngineURL:   srv.EngineURL,
		Username:    srv.Username,
		Password:    srv.Password,
		CACert:      srv.CACert,
		InsecureTLS: srv.InsecureTLS,
		Timeout:     p.timeout,
		Logger:      p.log.With().Str("engine", srv.Name).Logger(),
	})
	if err != nil {
		return nil, err
	}

	if old, ok := p.entries[srv.ID]; ok {
		// Откажитесь от предыдущего токена, чтобы движок не накапливал сессии.
		go old.client.Logout(context.Background())
	}
	p.entries[srv.ID] = &poolEntry{client: client, key: key}
	return client, nil
}

// Invalidate сбрасывает кэшированный клиент на один движок.
func (p *Pool) Invalidate(serverID string) {
	p.mu.Lock()
	entry, ok := p.entries[serverID]
	delete(p.entries, serverID)
	p.mu.Unlock()

	if ok {
		go entry.client.Logout(context.Background())
	}
}

// Close все сеансы кэшируются.
func (p *Pool) Close() {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]*poolEntry{}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, entry := range entries {
		entry.client.Logout(ctx)
	}
}
