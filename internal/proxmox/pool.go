package proxmox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

type ServerLoader func(context.Context, string) (*model.Server, error)

// Pool caches immutable HTTP clients until connection settings change.
type Pool struct {
	load    ServerLoader
	timeout time.Duration
	mu      sync.Mutex
	entries map[string]poolEntry
}

type poolEntry struct {
	client    *Client
	updatedAt time.Time
}

func NewPool(load ServerLoader, timeout time.Duration) *Pool {
	return &Pool{load: load, timeout: timeout, entries: make(map[string]poolEntry)}
}

func (p *Pool) Get(ctx context.Context, serverID string) (*Client, error) {
	srv, err := p.load(ctx, serverID)
	if err != nil {
		return nil, err
	}
	return p.ForServer(srv)
}

func (p *Pool) ForServer(srv *model.Server) (*Client, error) {
	if srv == nil {
		return nil, fmt.Errorf("подключение Proxmox не задано")
	}
	if !srv.Kind.UsesProxmoxAPI() {
		return nil, fmt.Errorf("подключение %q имеет тип %q, а не Proxmox", srv.Name, srv.Kind)
	}
	if !srv.Enabled {
		return nil, fmt.Errorf("подключение %q отключено", srv.Name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.entries[srv.ID]; ok && !entry.updatedAt.Before(srv.UpdatedAt) {
		return entry.client, nil
	}
	client, err := New(Config{
		BaseURL: srv.EngineURL, TokenID: srv.Username, TokenSecret: srv.Password,
		CACert: srv.CACert, InsecureTLS: srv.InsecureTLS, Timeout: p.timeout,
	})
	if err != nil {
		return nil, err
	}
	p.entries[srv.ID] = poolEntry{client: client, updatedAt: srv.UpdatedAt}
	return client, nil
}

func (p *Pool) Invalidate(serverID string) {
	p.mu.Lock()
	delete(p.entries, serverID)
	p.mu.Unlock()
}
