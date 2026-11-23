package store

import (
	"context"
	"errors"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

// ErrNotFound возвращается каждым методом Get*, когда строка отсутствует.
var ErrNotFound = errors.New("не найдено")

// ErrConflict возвращается, когда будет нарушено ограничение уникальности.
var ErrConflict = errors.New("объект с таким именем уже существует")

// Store — фасад репозитория над базой данных. учётные данные аутентификации проходят через
// шифр при входе и выходе, поэтому клиенты этого интерфейса никогда не видят шифротекст.
type Store struct {
	db     *DB
	cipher *secret.Cipher
	// alertRaised вызывается, когда оповещение загорелось впервые или после
	// того, как его погасили. Устанавливается через OnAlertRaised.
	alertRaised func(model.Alert)
}

// New строит Магазин поверх открытой базы данных.
func New(db *DB, c *secret.Cipher) *Store {
	return &Store{db: db, cipher: c}
}

// DB предоставляет базовую ручку для проверок работоспособности и задач режима
// обслуживания.
func (s *Store) DB() *DB { return s.db }

// Close высвобождает обработчик базы данных.
func (s *Store) Close() error { return s.db.Close() }

// Ping проверяет, что база данных доступна.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
