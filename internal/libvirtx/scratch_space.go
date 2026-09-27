package libvirtx

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// scratchSpaceTimeout — предел одного замера: сторож места не должен
// зависнуть вместе с сессией.
const scratchSpaceTimeout = 10 * time.Second

// ScratchSpace меряет свободное место на томе scratch, пока идёт бэкап.
//
// Замер нужен каждые несколько секунд, и команда stat на каждый оставляла бы
// строку в журнале хоста (у sshd с LogLevel VERBOSE — «Starting session» на
// каждую команду). Поэтому место берётся запросом statvfs через одну
// SFTP-сессию на весь бэкап. Если SFTP на хосте выключен или не умеет statvfs
// (это расширение OpenSSH), замер идёт командой, как раньше.
type ScratchSpace struct {
	conn *Conn
	dir  string

	mu     sync.Mutex
	client *sftp.Client
	noSFTP bool
}

// NewScratchSpace готовит замеры для каталога scratch. SFTP-сессия
// открывается при первом замере; Close обязателен.
func (c *Conn) NewScratchSpace(dir string) *ScratchSpace {
	return &ScratchSpace{conn: c, dir: dir}
}

// Free — свободное место на томе каталога, байт.
func (s *ScratchSpace) Free(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, scratchSpaceTimeout)
	defer cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	if client := s.sftpLocked(); client != nil {
		type result struct {
			free int64
			err  error
		}
		done := make(chan result, 1)
		go func() {
			st, err := client.StatVFS(s.dir)
			if err != nil {
				done <- result{err: err}
				return
			}
			done <- result{free: int64(st.Bavail * st.Frsize)}
		}()
		select {
		case r := <-done:
			if r.err == nil {
				return r.free, nil
			}
			// statvfs не поддержан или сессия умерла — дальше командой.
			s.dropLocked(true)
		case <-ctx.Done():
			// Закрытие сессии разблокирует зависший запрос; следующий замер
			// откроет её заново.
			s.dropLocked(false)
			return 0, ctx.Err()
		}
	}
	return s.conn.ScratchFree(ctx, s.dir)
}

// sftpLocked открывает SFTP-сессию, если её ещё нет и SFTP не отказал.
func (s *ScratchSpace) sftpLocked() *sftp.Client {
	if s.client != nil || s.noSFTP {
		return s.client
	}
	s.conn.mu.Lock()
	sshClient, closed := s.conn.ssh, s.conn.closed
	s.conn.mu.Unlock()
	if closed || sshClient == nil {
		return nil
	}
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		s.noSFTP = true
		return nil
	}
	s.client = client
	return client
}

func (s *ScratchSpace) dropLocked(disableSFTP bool) {
	if s.client != nil {
		client := s.client
		s.client = nil
		// Close ждёт, пока сервер закроет сессию; зависшему серверу это не
		// должно мешать сторожу.
		go func() { _ = client.Close() }()
	}
	if disableSFTP {
		s.noSFTP = true
	}
}

// Close закрывает SFTP-сессию.
func (s *ScratchSpace) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked(false)
	return nil
}
