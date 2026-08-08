package ovirt

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/model"
)

// ServerLoader fetches a connection definition by id. The pool takes it as a
// function so that this package does not have to know about the store.
type ServerLoader func(ctx context.Context, serverID string) (*model.Server, error)

// Pool keeps one authenticated client per engine.
//
// Re-authenticating on every call would be wasteful — the monitor polls every
// half minute — but a cached client must not outlive a credential change, so
// each entry remembers the server record it was built from.
type Pool struct {
	load    ServerLoader
	timeout time.Duration
	log     zerolog.Logger

	mu      sync.Mutex
	entries map[string]*poolEntry
}

type poolEntry struct {
	client    *Client
	updatedAt time.Time
}

// NewPool builds a pool. timeout applies to individual API calls.
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

// Get returns a client for the engine, building one if needed. A connection
// whose definition changed since the client was created is rebuilt, so editing
// credentials in the UI takes effect immediately.
func (p *Pool) Get(ctx context.Context, serverID string) (*Client, error) {
	srv, err := p.load(ctx, serverID)
	if err != nil {
		return nil, err
	}
	return p.ForServer(srv)
}

// ForServer returns a client for an already-loaded definition.
func (p *Pool) ForServer(srv *model.Server) (*Client, error) {
	if srv == nil {
		return nil, fmt.Errorf("сервер не задан")
	}
	if !srv.Enabled {
		return nil, fmt.Errorf("сервер %q отключён", srv.Name)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if entry, ok := p.entries[srv.ID]; ok && !entry.updatedAt.Before(srv.UpdatedAt) {
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
		// Drop the previous token so the engine does not accumulate sessions.
		go old.client.Logout(context.Background())
	}
	p.entries[srv.ID] = &poolEntry{client: client, updatedAt: srv.UpdatedAt}
	return client, nil
}

// Invalidate drops the cached client for one engine.
func (p *Pool) Invalidate(serverID string) {
	p.mu.Lock()
	entry, ok := p.entries[serverID]
	delete(p.entries, serverID)
	p.mu.Unlock()

	if ok {
		go entry.client.Logout(context.Background())
	}
}

// Close logs every cached session out.
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
