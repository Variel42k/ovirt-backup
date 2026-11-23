// Пакет libvirtx подключается к хосту libvirt через SSH и предоставляет драйверу инвентарь
// доменов, checkpoint, API pull-бэкапа и туннель до сокета NBD.
//
// SSH не требует дополнительных компонентов на гипервизоре или портов кроме 22. Один канал
// переносит управляющий RPC libvirt и данные NBD через Unix-сокеты. При наличии PKI libvirt
// способ подключения можно сменить без правок вышележащего кода.
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

	"github.com/Variel42k/ovirt-backup/internal/sshstats"
	"github.com/Variel42k/ovirt-backup/internal/sshtrust"
)

// DefaultSocket — где libvirtd слушает на инсталляции запаса.
const DefaultSocket = "/var/run/libvirt/libvirt-sock"

// Config описывает одно гипервизорное соединение.
type Config struct {
	Host string
	Port int
	User string

	// Ровно один из способов аутентификации. Ключ без парольной фразы —
	// автоматические задания не смогут её ввести.
	Password   string
	PrivateKey string

	// HostKey в формате authorized_keys.
	//
	// Пусто — подключения не будет: пустой ключ означает «ключ ещё не задан», а
	// не «проверять не надо». Отказаться от проверки можно только явно, через
	// TrustAnyHostKey.
	HostKey string

	// TrustAnyHostKey — осознанный отказ от проверки подлинности гипервизора.
	//
	// Годится для лаборатории и не годится для боевой установки: тот, кто
	// встанет посередине такого подключения, получает доступ к дискам всех
	// виртуальных машин этого хоста.
	TrustAnyHostKey bool

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

// Conn — живое соединение с одним гипервизором.
type Conn struct {
	cfg Config

	mu     sync.Mutex
	ssh    *ssh.Client
	lv     *libvirt.Libvirt
	rpc    net.Conn
	closed bool
	// stop останавливает keepalive (StartKeepalive); nil — не запущен.
	stop chan struct{}
}

// Connect устанавливаетъ SSH сеансъ и libvirt RPC каналъ надъ нимъ.
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

	// Рукопожатие SSH не имеет собственной контекстной поддержки, поэтому крайний срок
	// переносится на соединение и очищается после его завершения.
	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	} else {
		_ = rawConn.SetDeadline(time.Now().Add(timeout))
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, cfg.addr(), clientCfg)
	sshstats.Record(cfg.addr(), sshstats.Libvirt, err)
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

	// Ключ и пароль — альтернативы, а не набор способов. Раньше серверу
	// предлагались оба, и это отдавало пароль тому, кто им не должен владеть:
	// достаточно отклонить publickey, и клиент сам предъявит пароль. Хосту,
	// притворившемуся гипервизором, ничего другого и не нужно.
	if cfg.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(cfg.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("разбор приватного ключа: %w "+
				"(ключ с парольной фразой не подходит для автоматических заданий)", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	} else if cfg.Password != "" {
		auths = append(auths, ssh.Password(cfg.Password))
	}
	if len(auths) == 0 {
		return nil, errors.New("не задан ни пароль, ни приватный ключ для SSH")
	}

	hostKeyCallback, err := sshtrust.Callback(cfg.HostKey, cfg.TrustAnyHostKey)
	if err != nil {
		return nil, fmt.Errorf("гипервизор %s: %w", cfg.Host, err)
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

// Libvirt выставляет клиент RPC для вызовов, которые этот пакет не обёртывает.
func (c *Conn) Libvirt() *libvirt.Libvirt { return c.lv }

// Host возвращает сконфигурированный адрес гипервизора для сообщений журнала.
func (c *Conn) Host() string { return c.cfg.Host }

// DialRemoteUnix открывает туннель к гнезду Unix на гипервизоре. Так достигается поток
// данных NBD резервного копирования в pull-режиме без открытия ни одного дополнительного
// порта.
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

// Run выполняет команду на гипервизоре и возвращает его стандартный вывод.
//
// Он используется экономно: для создания и удаления временных файлов данных требуется
// резервное копирование в режиме извлечения и запуск qemu-img там, где образ должно быть
// проверено, где оно живет. Все остальное проходит через libvirt.
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
		// Попытка отправки сигнала может завершиться ошибкой. Операцию останавливает
		// закрытие сеанса.
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

// RunWithStdin Выполняет команду на гипервизоре, питая его от р.
//
// Восстановленный образ для проверки загрузки передаётся прямо в файл на хосте через
// существующий канал SSH. На сервере копий промежуточный файл не создаётся.
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
		// Закрытие stdin — то, что говорит удаленная команда, у которой есть все.
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

// WriteFile создает на гипервизоре файл с заданным содержимым, используемый ни для чего
// большего, чем вспомогательный скрипт.
func (c *Conn) WriteFile(ctx context.Context, path string, content []byte, mode os.FileMode) error {
	// Base64 через оболочку избегает цитирования проблем с произвольным содержанием и
	// сохраняет эту зависимость свободной на удаленной стороне.
	encoded := encodeBase64(content)
	cmd := fmt.Sprintf("umask 077 && printf '%%s' '%s' | base64 -d > %s && chmod %o %s",
		encoded, shellQuote(path), mode.Perm(), shellQuote(path))
	_, err := c.Run(ctx, cmd)
	return err
}

// Close ср. libvirt сессія и SSH связь.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.stopKeepaliveLocked()

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

// Alive сообщает, отвечает ли соединение, используемое процессом мониторинга, прежде чем
// решить, что хост не работает.
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

// Version возвращает демоническую версию libvirt как major.minor.release.
func (c *Conn) Version(ctx context.Context) (string, error) {
	raw, err := c.lv.ConnectGetLibVersion()
	if err != nil {
		return "", err
	}
	// libvirt кодирует версию как мажор*1000000 + минор*1000 + релиз.
	major := raw / 1000000
	minor := (raw % 1000000) / 1000
	release := raw % 1000
	return fmt.Sprintf("%d.%d.%d", major, minor, release), nil
}

// SupportsIncrementalBackup сообщает, является ли демон достаточно новым для резервного
// копирования в режиме тяги API с checkpoints, который приземлился в libvirt 6.0.
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
