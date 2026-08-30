package libvirtx

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// ServerLoader fetches a connection definition by id. Taking it as a function
// keeps this package independent of the store.
type ServerLoader func(ctx context.Context, serverID string) (*model.Server, error)

// Pool keeps one live SSH+libvirt session per hypervisor.
//
// It differs from the oVirt pool in one important way: an SSH connection is
// stateful and can die silently — a rebooted hypervisor, a dropped NAT
// mapping, a restarted libvirtd. Handing out a dead connection would turn one
// network blip into a failed backup, so every handout is preceded by a cheap
// liveness probe and a reconnect if it fails.
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

// NewPool builds a pool. timeout bounds connection setup and liveness probes.
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

// Get returns a live connection for the hypervisor, dialing or redialing as
// needed.
func (p *Pool) Get(ctx context.Context, serverID string) (*Conn, error) {
	srv, err := p.load(ctx, serverID)
	if err != nil {
		return nil, err
	}
	return p.ForServer(ctx, srv)
}

// ForServer returns a connection for an already-loaded definition.
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
	// A definition edited since the connection was made carries different
	// credentials or a different host; the cached session is stale by
	// definition.
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
	// Another goroutine may have connected while we were dialing; keep one.
	if existing, ok := p.entries[srv.ID]; ok {
		p.mu.Unlock()
		go conn.Close()
		return existing.conn, nil
	}
	p.entries[srv.ID] = &poolEntry{conn: conn, updatedAt: srv.UpdatedAt}
	p.mu.Unlock()

	return conn, nil
}

// ConfigFromServer maps a stored connection onto the transport config.
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

// Invalidate drops the cached connection for one hypervisor.
func (p *Pool) Invalidate(serverID string) {
	p.mu.Lock()
	entry, ok := p.entries[serverID]
	delete(p.entries, serverID)
	p.mu.Unlock()

	if ok {
		go entry.conn.Close()
	}
}

// Close tears down every cached session.
func (p *Pool) Close() {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]*poolEntry{}
	p.mu.Unlock()

	for _, entry := range entries {
		_ = entry.conn.Close()
	}
}
