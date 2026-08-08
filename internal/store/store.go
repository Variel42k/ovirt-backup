package store

import (
	"context"
	"errors"

	"adveng/jh_virt/internal/secret"
)

// ErrNotFound is returned by every Get* method when the row is absent.
var ErrNotFound = errors.New("не найдено")

// ErrConflict is returned when a uniqueness constraint would be violated.
var ErrConflict = errors.New("объект с таким именем уже существует")

// Store is the repository facade over the database. Credentials pass through
// the cipher on the way in and out, so callers never see ciphertext.
type Store struct {
	db     *DB
	cipher *secret.Cipher
}

// New builds a Store over an open database handle.
func New(db *DB, c *secret.Cipher) *Store {
	return &Store{db: db, cipher: c}
}

// DB exposes the underlying handle for health checks and maintenance tasks.
func (s *Store) DB() *DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
