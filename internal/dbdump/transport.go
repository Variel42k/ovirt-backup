// Package dbdump снимает логические дампы СУБД через хелпер jhvirt-db-dump и
// хранит их в том же чанкованном формате, что и копии дисков.
package dbdump

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/sshtrust"
)

const (
	// Protocol — версия протокола хелпера, которую понимает служба.
	Protocol = "jhvirt-db-dump/1"
	helper   = "/usr/local/sbin/jhvirt-db-dump"
)

// Transport — операции хелпера. Интерфейс нужен тестам: движок проверяется с
// поддельным хелпером, без SSH и без СУБД.
type Transport interface {
	Probe(ctx context.Context) (*ProbeResult, error)
	List(ctx context.Context, engine model.DBEngine) ([]string, error)
	Dump(ctx context.Context, engine model.DBEngine, database string, consume func(io.Reader) error) error
	Globals(ctx context.Context, consume func(io.Reader) error) error
	Restore(ctx context.Context, engine model.DBEngine, database string, dump io.Reader) error
}

// ProbeResult — ответ хелпера на probe.
type ProbeResult struct {
	Engines        []model.DBEngineInfo
	Errors         map[model.DBEngine]string
	RestoreEnabled bool
}

// ParseProbe разбирает ответ probe. Первая строка обязана быть версией
// протокола: чужая программа на месте хелпера не должна сойти за него.
func ParseProbe(out string) (*ProbeResult, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != Protocol {
		return nil, fmt.Errorf("хост вернул несовместимый протокол хелпера дампов, ожидается %s", Protocol)
	}
	res := &ProbeResult{Errors: map[model.DBEngine]string{}}
	for sc.Scan() {
		fields := strings.SplitN(strings.TrimSpace(sc.Text()), " ", 3)
		switch {
		case len(fields) == 3 && fields[0] == "engine" && model.DBEngine(fields[1]).Valid():
			res.Engines = append(res.Engines, model.DBEngineInfo{Engine: model.DBEngine(fields[1]), Version: fields[2]})
		case len(fields) >= 2 && fields[0] == "error" && model.DBEngine(fields[1]).Valid():
			detail := ""
			if len(fields) == 3 {
				detail = fields[2]
			}
			res.Errors[model.DBEngine(fields[1])] = detail
		case len(fields) == 2 && fields[0] == "restore":
			res.RestoreEnabled = fields[1] == "enabled"
		}
	}
	return res, sc.Err()
}

// SSHTransport ходит к хелперу по SSH. Команды — только операции протокола
// с проверенными аргументами; хелпер проверяет их ещё раз на своей стороне.
type SSHTransport struct {
	host    *model.DBHost
	timeout time.Duration
	hostKey ssh.HostKeyCallback
}

// NewSSHTransport готовит канал к хосту СУБД. Без закреплённого ключа хоста
// и без явного отказа от проверки подключения не будет.
func NewSSHTransport(h *model.DBHost, timeout time.Duration) (*SSHTransport, error) {
	if h == nil {
		return nil, errors.New("не задан хост СУБД")
	}
	if strings.TrimSpace(h.PrivateKey) == "" {
		return nil, errors.New("у хоста СУБД нет SSH-ключа")
	}
	callback, err := sshtrust.Callback(h.HostKey, h.TrustAnyHostKey)
	if err != nil {
		return nil, fmt.Errorf("ключ хоста СУБД %s: %w", h.Name, err)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &SSHTransport{host: h, timeout: timeout, hostKey: callback}, nil
}

func (t *SSHTransport) Probe(ctx context.Context) (*ProbeResult, error) {
	var out bytes.Buffer
	if err := t.run(ctx, helper+" probe", nil, &out); err != nil {
		return nil, err
	}
	return ParseProbe(out.String())
}

func (t *SSHTransport) List(ctx context.Context, engine model.DBEngine) ([]string, error) {
	if !engine.Valid() {
		return nil, fmt.Errorf("неизвестная СУБД %q", engine)
	}
	var out bytes.Buffer
	if err := t.run(ctx, fmt.Sprintf("%s list %s", helper, engine), nil, &out); err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out.String(), "\n") {
		if name := strings.TrimSpace(line); name != "" && model.ValidDBName(name) {
			names = append(names, name)
		}
	}
	return names, nil
}

func (t *SSHTransport) Dump(ctx context.Context, engine model.DBEngine, database string,
	consume func(io.Reader) error) error {
	if !engine.Valid() || !model.ValidDBName(database) {
		return fmt.Errorf("недопустимый запрос дампа %s/%q", engine, database)
	}
	return t.stream(ctx, fmt.Sprintf("%s dump %s %s", helper, engine, database), consume)
}

func (t *SSHTransport) Globals(ctx context.Context, consume func(io.Reader) error) error {
	return t.stream(ctx, helper+" globals postgresql", consume)
}

func (t *SSHTransport) Restore(ctx context.Context, engine model.DBEngine, database string, dump io.Reader) error {
	if !engine.Valid() || !model.ValidDBName(database) {
		return fmt.Errorf("недопустимый запрос восстановления %s/%q", engine, database)
	}
	return t.run(ctx, fmt.Sprintf("%s restore %s %s", helper, engine, database), dump, io.Discard)
}

func (t *SSHTransport) stream(ctx context.Context, command string, consume func(io.Reader) error) error {
	client, err := t.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH-сессия на %s: %w", t.host.Name, err)
	}
	defer session.Close()
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &limitedBuffer{limit: 64 << 10}
	session.Stderr = stderr
	if err := session.Start(command); err != nil {
		return fmt.Errorf("запуск хелпера на %s: %w", t.host.Name, err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-stop:
		}
	}()
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	if err := consume(stdout); err != nil {
		_ = client.Close()
		<-done
		return err
	}
	if err := <-done; err != nil {
		return t.commandError(err, stderr.String())
	}
	return ctx.Err()
}

func (t *SSHTransport) run(ctx context.Context, command string, stdin io.Reader, stdout io.Writer) error {
	client, err := t.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("SSH-сессия на %s: %w", t.host.Name, err)
	}
	defer session.Close()
	stderr := &limitedBuffer{limit: 64 << 10}
	session.Stdin, session.Stdout, session.Stderr = stdin, stdout, stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err := <-done:
		if err != nil {
			return t.commandError(err, stderr.String())
		}
		return nil
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return ctx.Err()
	}
}

func (t *SSHTransport) connect(ctx context.Context) (*ssh.Client, error) {
	signer, err := ssh.ParsePrivateKey([]byte(t.host.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("разбор SSH-ключа хоста СУБД: %w", err)
	}
	port := t.host.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(strings.TrimSpace(t.host.Address), fmt.Sprint(port))
	raw, err := (&net.Dialer{Timeout: t.timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("SSH к хосту СУБД %s: %w", addr, err)
	}
	_ = raw.SetDeadline(time.Now().Add(t.timeout))
	// Только ключ: при наличии ключа пароль не предлагается никогда.
	conn, channels, requests, err := ssh.NewClientConn(raw, addr, &ssh.ClientConfig{
		User: t.host.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: t.hostKey, Timeout: t.timeout,
	})
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("SSH-аутентификация на %s: %w", addr, err)
	}
	_ = raw.SetDeadline(time.Time{})
	return ssh.NewClient(conn, channels, requests), nil
}

func (t *SSHTransport) commandError(err error, stderr string) error {
	if detail := strings.TrimSpace(stderr); detail != "" {
		return fmt.Errorf("хелпер дампов на %s: %w: %s", t.host.Name, err, detail)
	}
	return fmt.Errorf("хелпер дампов на %s: %w", t.host.Name, err)
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return original, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
