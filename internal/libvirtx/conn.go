// Package libvirtx connects to a libvirt host over SSH and exposes what the
// backup driver needs: the domain inventory, checkpoints, the pull-mode backup
// API, and a tunnel to the NBD socket the hypervisor opens for a backup.
//
// SSH rather than libvirt's TLS transport: it needs nothing installed on the
// hypervisor and no port beyond 22, and the same channel carries both the
// control plane (libvirt RPC over the unix socket) and the data plane (the
// backup NBD socket). A site that has already built libvirt PKI can point the
// dialer elsewhere; everything above this file is transport-agnostic.
package libvirtx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"
	"golang.org/x/crypto/ssh"
)

// DefaultSocket is where libvirtd listens on a stock installation.
const DefaultSocket = "/var/run/libvirt/libvirt-sock"

// Config describes one hypervisor connection.
type Config struct {
	Host string
	Port int
	User string

	// Ровно один из способов аутентификации. Ключ без парольной фразы —
	// автоматические задания не смогут её ввести.
	Password   string
	PrivateKey string

	// HostKey в формате authorized_keys. Пусто — ключ хоста не проверяется;
	// это осознанный компромисс для лабораторий, но не для боевой установки.
	HostKey string

	// Socket — путь к сокету libvirtd на удалённом хосте.
	Socket string

	// ConnectTimeout ограничивает установку SSH-соединения; 0 — 30 секунд.
	ConnectTimeout time.Duration
}

func (c Config) addr() string {
	port := c.Port
	if port == 0 {
		port = 22
	}
	return fmt.Sprintf("%s:%d", c.Host, port)
}

func (c Config) socket() string {
	if c.Socket == "" {
		return DefaultSocket
	}
	return c.Socket
}

// Conn is a live connection to one hypervisor.
type Conn struct {
	cfg Config

	mu     sync.Mutex
	ssh    *ssh.Client
	lv     *libvirt.Libvirt
	rpc    net.Conn
	closed bool
}

// Connect establishes the SSH session and the libvirt RPC channel on top of it.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.Host == "" {
		return nil, errors.New("не указан адрес гипервизора")
	}
	if cfg.User == "" {
		return nil, errors.New("не указан пользователь SSH")
	}

	clientCfg, err := sshConfig(cfg)
	if err != nil {
		return nil, err
	}

	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", cfg.addr())
	if err != nil {
		return nil, fmt.Errorf("подключение к %s: %w", cfg.addr(), err)
	}

	// The SSH handshake has no context support of its own, so the deadline is
	// pushed onto the connection and cleared once it completes.
	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	} else {
		_ = rawConn.SetDeadline(time.Now().Add(timeout))
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, cfg.addr(), clientCfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("SSH-подключение к %s: %w", cfg.addr(), err)
	}
	_ = rawConn.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConn, chans, reqs)

	rpc, err := client.Dial("unix", cfg.socket())
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("сокет libvirt %s на %s недоступен: %w "+
			"(проверьте, что libvirtd запущен и пользователь %s состоит в группе libvirt)",
			cfg.socket(), cfg.Host, err, cfg.User)
	}

	lv := libvirt.New(rpc)
	if err := lv.Connect(); err != nil {
		_ = rpc.Close()
		_ = client.Close()
		return nil, fmt.Errorf("рукопожатие libvirt на %s: %w", cfg.Host, err)
	}

	return &Conn{cfg: cfg, ssh: client, lv: lv, rpc: rpc}, nil
}

func sshConfig(cfg Config) (*ssh.ClientConfig, error) {
	var auths []ssh.AuthMethod

	if cfg.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(cfg.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("разбор приватного ключа: %w "+
				"(ключ с парольной фразой не подходит для автоматических заданий)", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		auths = append(auths, ssh.Password(cfg.Password))
	}
	if len(auths) == 0 {
		return nil, errors.New("не задан ни пароль, ни приватный ключ для SSH")
	}

	hostKeyCallback, err := hostKeyCallback(cfg.HostKey)
	if err != nil {
		return nil, err
	}

	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}, nil
}

func hostKeyCallback(hostKey string) (ssh.HostKeyCallback, error) {
	if strings.TrimSpace(hostKey) == "" {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostKey))
	if err != nil {
		return nil, fmt.Errorf("разбор ключа хоста: %w (ожидается формат authorized_keys)", err)
	}
	return ssh.FixedHostKey(parsed), nil
}

// Libvirt exposes the RPC client for calls this package does not wrap.
func (c *Conn) Libvirt() *libvirt.Libvirt { return c.lv }

// Host returns the configured hypervisor address, for log messages.
func (c *Conn) Host() string { return c.cfg.Host }

// DialRemoteUnix opens a tunnel to a unix socket on the hypervisor. This is
// how the NBD data stream of a pull-mode backup is reached without opening a
// single extra port.
func (c *Conn) DialRemoteUnix(path string) (net.Conn, error) {
	c.mu.Lock()
	client := c.ssh
	closed := c.closed
	c.mu.Unlock()

	if closed || client == nil {
		return nil, errors.New("соединение с гипервизором закрыто")
	}
	conn, err := client.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("туннель к сокету %s на %s: %w", path, c.cfg.Host, err)
	}
	return conn, nil
}

// Run executes a command on the hypervisor and returns its standard output.
//
// It is used sparingly: creating and removing the scratch files a pull-mode
// backup needs, and running qemu-img there when an image has to be inspected
// where it lives. Everything else goes through libvirt.
func (c *Conn) Run(ctx context.Context, command string) (string, error) {
	c.mu.Lock()
	client := c.ssh
	closed := c.closed
	c.mu.Unlock()

	if closed || client == nil {
		return "", errors.New("соединение с гипервизором закрыто")
	}

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("открытие SSH-сессии: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case <-ctx.Done():
		// Signal is best effort; closing the session is what actually stops it.
		_ = session.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	case err := <-done:
		if err != nil {
			return stdout.String(), fmt.Errorf("команда %q на %s: %w: %s",
				command, c.cfg.Host, err, strings.TrimSpace(stderr.String()))
		}
		return stdout.String(), nil
	}
}

// RunWithStdin executes a command on the hypervisor, feeding it from r.
//
// This is how a reconstructed disk image reaches the host for a boot test: the
// bytes go straight into a file over the existing SSH channel, without a
// staging copy on the backup server.
func (c *Conn) RunWithStdin(ctx context.Context, command string, r io.Reader) error {
	c.mu.Lock()
	client := c.ssh
	closed := c.closed
	c.mu.Unlock()

	if closed || client == nil {
		return errors.New("соединение с гипервизором закрыто")
	}

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("открытие SSH-сессии: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	session.Stderr = &stderr

	if err := session.Start(command); err != nil {
		return fmt.Errorf("запуск %q: %w", command, err)
	}

	copyErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdin, r)
		// Closing stdin is what tells the remote command it has everything.
		if closeErr := stdin.Close(); err == nil {
			err = closeErr
		}
		copyErr <- err
	}()

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return ctx.Err()
	case err := <-copyErr:
		if err != nil {
			_ = session.Signal(ssh.SIGKILL)
			return fmt.Errorf("передача данных в %q: %w", command, err)
		}
	}

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return fmt.Errorf("команда %q на %s: %w: %s",
				command, c.cfg.Host, err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
}

// WriteFile creates a file on the hypervisor with the given content, used for
// nothing larger than a helper script.
func (c *Conn) WriteFile(ctx context.Context, path string, content []byte, mode os.FileMode) error {
	// Base64 through the shell avoids quoting problems with arbitrary content
	// and keeps this dependency-free on the remote side.
	encoded := encodeBase64(content)
	cmd := fmt.Sprintf("umask 077 && printf '%%s' '%s' | base64 -d > %s && chmod %o %s",
		encoded, shellQuote(path), mode.Perm(), shellQuote(path))
	_, err := c.Run(ctx, cmd)
	return err
}

// Close tears down the libvirt session and the SSH connection.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true

	var firstErr error
	if c.lv != nil {
		if err := c.lv.Disconnect(); err != nil {
			firstErr = err
		}
	}
	if c.rpc != nil {
		_ = c.rpc.Close()
	}
	if c.ssh != nil {
		if err := c.ssh.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Alive reports whether the connection still answers, used by the poller
// before deciding a host is down.
func (c *Conn) Alive(ctx context.Context) error {
	c.mu.Lock()
	lv := c.lv
	closed := c.closed
	c.mu.Unlock()

	if closed || lv == nil {
		return errors.New("соединение закрыто")
	}
	done := make(chan error, 1)
	go func() {
		_, err := lv.ConnectGetLibVersion()
		done <- err
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// Version returns the libvirt daemon version as major.minor.release.
func (c *Conn) Version(ctx context.Context) (string, error) {
	raw, err := c.lv.ConnectGetLibVersion()
	if err != nil {
		return "", err
	}
	// libvirt encodes the version as major*1000000 + minor*1000 + release.
	major := raw / 1000000
	minor := (raw % 1000000) / 1000
	release := raw % 1000
	return fmt.Sprintf("%d.%d.%d", major, minor, release), nil
}

// SupportsIncrementalBackup reports whether the daemon is new enough for the
// pull-mode backup API with checkpoints, which landed in libvirt 6.0.
func (c *Conn) SupportsIncrementalBackup(ctx context.Context) (bool, string, error) {
	version, err := c.Version(ctx)
	if err != nil {
		return false, "", err
	}
	var major, minor, release int
	if _, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &release); err != nil {
		return false, version, nil
	}
	return major > 6 || (major == 6 && minor >= 0), version, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
