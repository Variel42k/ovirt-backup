package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"time"
)

// A running worker keeps its session lock until all cancellation cleanup has
// returned. A timeout never gives a second worker permission to race it.
type VMTransferLock struct {
	conn *sql.Conn
	key  string
}

func (s *Store) TryVMTransferLock(ctx context.Context, id string) (*VMTransferLock, error) {
	return s.tryRecoveryLock(ctx, "jhvirt-vm-transfer:"+id)
}

func (s *Store) TryVMTransferQueueLock(ctx context.Context, batchID string) (*VMTransferLock, error) {
	return s.tryRecoveryLock(ctx, "jhvirt-vm-transfer-batch:"+batchID)
}

func (s *Store) TryVMRecoveryLock(ctx context.Context, serverID, vmID string) (*VMTransferLock, error) {
	return s.tryRecoveryLock(ctx, serverID+":"+vmID)
}

func (s *Store) tryRecoveryLock(ctx context.Context, key string) (*VMTransferLock, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, key).Scan(&ok)
	if err != nil || !ok {
		_ = conn.Close()
		return nil, err
	}
	return &VMTransferLock{conn: conn, key: key}, nil
}

func (l *VMTransferLock) Alive(ctx context.Context) bool { return l.conn.PingContext(ctx) == nil }

func (l *VMTransferLock) Release() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var ok bool
	if err := l.conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, l.key).Scan(&ok); err != nil || !ok {
		_ = l.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = l.conn.Close()
}
