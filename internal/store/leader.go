package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// LeaderLockKey — ключ консультативной блокировки, по которой экземпляры
// службы договариваются, кто из них ведущий.
//
// Число произвольное, но постоянное: сменить его — значит развести экземпляры
// по разным блокировкам, и ведущими станут оба.
const LeaderLockKey int64 = 7_240_119

// Leadership — удержанное ведущее место.
//
// Блокировка берётся на отдельном соединении и живёт, пока оно живо. Это её
// главное свойство: упавший экземпляр отпускает место сам, потому что сервер
// закрывает его соединение, — без сроков, сердцебиений и ожидания, пока
// истечёт аренда.
type Leadership struct {
	conn *sql.Conn

	mu   sync.Mutex
	lost bool
}

// TryBecomeLeader пытается занять место ведущего, не дожидаясь освобождения.
//
// Возвращает (nil, nil), если место занято другим экземпляром: это не ошибка,
// а обычное состояние ведомого, и обращаться с ним как с отказом нельзя.
func (s *Store) TryBecomeLeader(ctx context.Context) (*Leadership, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("соединение для выборов ведущего: %w", err)
	}

	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`,
		LeaderLockKey).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("попытка занять место ведущего: %w", err)
	}
	if !acquired {
		// Соединение отдаём обратно в пул: держать его без блокировки незачем.
		_ = conn.Close()
		return nil, nil
	}
	return &Leadership{conn: conn}, nil
}

// Alive проверяет, что место всё ещё за нами.
//
// Обрыв соединения PostgreSQL замечает не мгновенно, а наполовину закрытое
// TCP-соединение может выглядеть живым часами. Пока мы считаем себя ведущими
// ошибочно, второй экземпляр уже работает — и задания выполняются дважды.
// Поэтому соединение регулярно опрашивается, и потеря блокировки означает
// немедленный отказ от роли.
func (l *Leadership) Alive(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return false
	}

	var held bool
	err := l.conn.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE locktype='advisory' AND objid=$1 AND pid=pg_backend_pid() AND granted)`,
		LeaderLockKey).Scan(&held)
	if err != nil || !held {
		l.lost = true
		return false
	}
	return true
}

// Release отдаёт место добровольно — при штатной остановке службы.
func (l *Leadership) Release(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return
	}
	l.lost = true

	// Разблокировать пытаемся, но не настаиваем: закрытие соединения снимет
	// блокировку в любом случае.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = l.conn.ExecContext(releaseCtx, `SELECT pg_advisory_unlock($1)`, LeaderLockKey)
	_ = l.conn.Close()
}
