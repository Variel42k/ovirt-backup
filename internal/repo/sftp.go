package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/sshtrust"
)

// sftpBackend stores backups on a remote host over SSH. It is the fallback for
// sites that have neither object storage nor a shared mount, and it is the one
// backend where the connection can drop mid-run, so it reconnects on demand.
type sftpBackend struct {
	name     string
	addr     string
	basePath string
	sshCfg   *ssh.ClientConfig

	mu     sync.Mutex
	ssh    *ssh.Client
	client *sftp.Client
}

func newSFTP(target *model.StorageTarget) (Backend, error) {
	if target.Host == "" {
		return nil, errors.New("для SFTP-хранилища не задан хост")
	}
	if target.Username == "" {
		return nil, errors.New("для SFTP-хранилища не задан пользователь")
	}
	port := target.Port
	if port == 0 {
		port = 22
	}

	// Ключ и пароль — альтернативы, а не набор. Предлагать оба значит отдать
	// пароль серверу, которому достаточно отклонить publickey, чтобы его
	// получить.
	var auths []ssh.AuthMethod
	if target.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(target.PrivateKey))
		if err != nil {
			// A passphrase-protected key cannot be used unattended; say so
			// instead of failing later with an opaque auth error.
			return nil, fmt.Errorf("разбор приватного ключа SFTP: %w "+
				"(ключ с парольной фразой не подходит для автоматических заданий)", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	} else if target.Password != "" {
		auths = append(auths, ssh.Password(target.Password))
	}
	if len(auths) == 0 {
		return nil, errors.New("для SFTP-хранилища не задан ни пароль, ни ключ")
	}

	hostKeyCallback, err := sshtrust.Callback(target.HostKey, target.TrustAnyHostKey)
	if err != nil {
		return nil, fmt.Errorf("хранилище %q: %w", target.Name, err)
	}

	base := target.BasePath
	if base == "" {
		base = "."
	}

	return &sftpBackend{
		name:     target.Name,
		addr:     fmt.Sprintf("%s:%d", target.Host, port),
		basePath: strings.TrimRight(base, "/"),
		sshCfg: &ssh.ClientConfig{
			User:            target.Username,
			Auth:            auths,
			HostKeyCallback: hostKeyCallback,
			Timeout:         20 * time.Second,
		},
	}, nil
}

func (s *sftpBackend) Kind() model.StorageKind { return model.StorageSFTP }
func (s *sftpBackend) Name() string            { return s.name }

// conn returns a live SFTP client, dialing or redialing as needed.
func (s *sftpBackend) conn() (*sftp.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		// Cheap liveness probe: a dead TCP connection fails here rather than
		// halfway through writing a disk image.
		if _, err := s.client.Getwd(); err == nil {
			return s.client, nil
		}
		s.closeLocked()
	}

	sshClient, err := ssh.Dial("tcp", s.addr, s.sshCfg)
	if err != nil {
		return nil, fmt.Errorf("подключение SSH к %s: %w", s.addr, err)
	}
	client, err := sftp.NewClient(sshClient, sftp.MaxPacket(1<<15))
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("открытие SFTP-сессии на %s: %w", s.addr, err)
	}
	s.ssh, s.client = sshClient, client
	return client, nil
}

func (s *sftpBackend) closeLocked() {
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
	if s.ssh != nil {
		_ = s.ssh.Close()
		s.ssh = nil
	}
}

func (s *sftpBackend) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	return nil
}

func (s *sftpBackend) remotePath(key string) string {
	return path.Join(s.basePath, strings.TrimLeft(path.Clean("/"+key), "/"))
}

func (s *sftpBackend) mkdirAll(client *sftp.Client, dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	if fi, err := client.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	if err := s.mkdirAll(client, path.Dir(dir)); err != nil {
		return err
	}
	if err := client.Mkdir(dir); err != nil {
		// Another disk of the same run may have created it concurrently.
		if fi, statErr := client.Stat(dir); statErr == nil && fi.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

func (s *sftpBackend) Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	client, err := s.conn()
	if err != nil {
		return 0, err
	}
	full := s.remotePath(key)
	if err := s.mkdirAll(client, path.Dir(full)); err != nil {
		return 0, fmt.Errorf("создание каталога для %s: %w", key, err)
	}

	tmp := full + ".jhv-tmp"
	f, err := client.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("создание %s: %w", key, err)
	}
	written, err := io.Copy(f, contextReader(ctx, r))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = client.Remove(tmp)
		return written, fmt.Errorf("запись %s: %w", key, err)
	}

	// Rename does not overwrite on most SFTP servers, so clear the target.
	_ = client.Remove(full)
	if err := client.Rename(tmp, full); err != nil {
		_ = client.Remove(tmp)
		return written, fmt.Errorf("публикация %s: %w", key, err)
	}
	return written, nil
}

func (s *sftpBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.GetRange(ctx, key, 0, -1)
}

func (s *sftpBackend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	client, err := s.conn()
	if err != nil {
		return nil, err
	}
	f, err := client.Open(s.remotePath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	if length < 0 {
		return f, nil
	}
	return &sectionCloser{Reader: io.LimitReader(f, length), closer: f}, nil
}

func (s *sftpBackend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	client, err := s.conn()
	if err != nil {
		return ObjectInfo{}, err
	}
	fi, err := client.Stat(s.remotePath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: fi.Size(), Modified: fi.ModTime().UTC()}, nil
}

func (s *sftpBackend) Delete(ctx context.Context, key string) error {
	client, err := s.conn()
	if err != nil {
		return err
	}
	if err := client.Remove(s.remotePath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *sftpBackend) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	objects, err := s.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	client, err := s.conn()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, o := range objects {
		if err := client.Remove(s.remotePath(o.Key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return count, err
		}
		count++
	}
	// Remove the now-empty directory tree, deepest first.
	dir := s.remotePath(prefix)
	if fi, err := client.Stat(dir); err == nil && fi.IsDir() {
		s.removeEmptyDirs(client, dir)
	}
	return count, nil
}

func (s *sftpBackend) removeEmptyDirs(client *sftp.Client, dir string) {
	entries, err := client.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			s.removeEmptyDirs(client, path.Join(dir, e.Name()))
		}
	}
	if entries, err := client.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = client.RemoveDirectory(dir)
	}
}

func (s *sftpBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	client, err := s.conn()
	if err != nil {
		return nil, err
	}
	root := s.remotePath(prefix)
	if fi, err := client.Stat(root); err != nil || !fi.IsDir() {
		root = path.Dir(root)
	}

	var out []ObjectInfo
	walker := client.Walk(root)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		fi := walker.Stat()
		if fi == nil || fi.IsDir() {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(walker.Path(), s.basePath), "/")
		if !strings.HasPrefix(rel, strings.TrimLeft(prefix, "/")) {
			continue
		}
		out = append(out, ObjectInfo{Key: rel, Size: fi.Size(), Modified: fi.ModTime().UTC()})
	}
	return out, nil
}

// Usage cannot be answered over plain SFTP; the protocol has no statvfs in the
// base specification and the extension is not universally available.
func (s *sftpBackend) Usage(ctx context.Context) (int64, int64, error) {
	client, err := s.conn()
	if err != nil {
		return 0, 0, err
	}
	st, err := client.StatVFS(s.basePath)
	if err != nil {
		return 0, 0, nil
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	total := int64(st.Blocks) * int64(st.Bsize)
	return free, total - free, nil
}

func (s *sftpBackend) Check(ctx context.Context) error {
	client, err := s.conn()
	if err != nil {
		return err
	}
	if err := s.mkdirAll(client, s.basePath); err != nil {
		return fmt.Errorf("создание базового каталога %s: %w", s.basePath, err)
	}
	return runCheck(ctx, s)
}
