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
	"github.com/Variel42k/ovirt-backup/internal/sshpool"
	"github.com/Variel42k/ovirt-backup/internal/sshstats"
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

type Stats struct{ Commits, Rollbacks, Active, LogBytes int64 }

type StatsTransport interface {
	Stats(context.Context, model.DBEngine) (Stats, error)
}

// StatsWatcher отдаёт статистику потоком — одна сессия на весь бэкап. onSample
// вызывается на каждую строку: с ошибкой, если СУБД не ответила. Возврат без
// отмены ctx значит, что поток недоступен (старый хелпер) или оборвался.
type StatsWatcher interface {
	WatchStats(ctx context.Context, engine model.DBEngine, interval time.Duration, onSample func(Stats, error)) error
}

// errStatsUnavailable — хелпер жив, но СУБД не ответила на этот замер.
var errStatsUnavailable = errors.New("СУБД не ответила на запрос статистики")

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
//
// Соединение берётся из общего пула (sshpool): статистика СУБД во время
// бэкапа снимается раз в несколько секунд, и соединение на каждый замер
// засоряло бы журнал хоста входами по ключу.
type SSHTransport struct {
	host    *model.DBHost
	timeout time.Duration
	hostKey ssh.HostKeyCallback
	poolKey string
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
	key := sshpool.Key("db-host", h.ID, strings.TrimSpace(h.Address), fmt.Sprint(h.Port), h.Username,
		h.PrivateKey, h.HostKey, fmt.Sprint(h.TrustAnyHostKey))
	return &SSHTransport{host: h, timeout: timeout, hostKey: callback, poolKey: key}, nil
}

// session открывает сессию на соединении из пула.
func (t *SSHTransport) session(ctx context.Context) (*ssh.Session, func(broken bool), error) {
	session, done, err := sshpool.Shared().Session(ctx, t.poolKey, t.connect)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH-сессия на %s: %w", t.host.Name, err)
	}
	return session, done, nil
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

func (t *SSHTransport) Stats(ctx context.Context, engine model.DBEngine) (Stats, error) {
	if !engine.Valid() {
		return Stats{}, fmt.Errorf("неизвестная СУБД %q", engine)
	}
	var body bytes.Buffer
	if err := t.run(ctx, fmt.Sprintf("%s stats %s", helper, engine), nil, &body); err != nil {
		return Stats{}, err
	}
	return ParseStats(body.String(), engine)
}

// WatchStats читает статистику из одной долгой сессии stats-watch.
//
// Конец бэкапа — отмена ctx — закрывает сессию, но не соединение: оно цело и
// пригодится следующему бэкапу. Хелпер на хосте завершается на следующей
// записи в закрытый поток.
func (t *SSHTransport) WatchStats(ctx context.Context, engine model.DBEngine, interval time.Duration,
	onSample func(Stats, error)) error {

	if !engine.Valid() {
		return fmt.Errorf("неизвестная СУБД %q", engine)
	}
	seconds := int(interval / time.Second)
	if seconds < 2 {
		seconds = 2
	}
	if seconds > 300 {
		seconds = 300
	}
	session, release, err := t.session(ctx)
	if err != nil {
		return err
	}
	defer release(false)
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &limitedBuffer{limit: 16 << 10}
	session.Stderr = stderr
	if err := session.Start(fmt.Sprintf("%s stats-watch %s %d", helper, engine, seconds)); err != nil {
		return fmt.Errorf("запуск хелпера на %s: %w", t.host.Name, err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close()
		case <-stop:
		}
	}()

	unavailable := fmt.Sprintf("jhvirt-db-stats/1 %s unavailable", engine)
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == unavailable {
			onSample(Stats{}, errStatsUnavailable)
			continue
		}
		onSample(ParseStats(line, engine))
	}
	waitErr := session.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return t.commandError(waitErr, stderr.String())
	}
	return errors.New("поток статистики закончился")
}

// ParseStats validates the deliberately small helper response. Rejecting an
// extra token keeps diagnostics or an unexpected program on the remote side
// from being silently accepted as monitoring data.
func ParseStats(body string, engine model.DBEngine) (Stats, error) {
	var out Stats
	var protocol, returned string
	reader := strings.NewReader(body)
	if _, err := fmt.Fscan(reader, &protocol, &returned, &out.Commits, &out.Rollbacks, &out.Active, &out.LogBytes); err != nil || protocol != "jhvirt-db-stats/1" || returned != string(engine) {
		return Stats{}, errors.New("хост вернул некорректную статистику СУБД")
	}
	var extra string
	if _, err := fmt.Fscan(reader, &extra); !errors.Is(err, io.EOF) {
		return Stats{}, errors.New("хост вернул лишние данные после статистики СУБД")
	}
	if out.Commits < 0 || out.Rollbacks < 0 || out.Active < 0 || out.LogBytes < 0 {
		return Stats{}, errors.New("хост вернул отрицательные счётчики СУБД")
	}
	return out, nil
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

// Прерывание — отмена или ошибка потребителя — закрывает сессию и помечает
// соединение негодным: пул закроет его, как только им перестанут пользоваться
// другие операции, и sshd завершит процесс хелпера.
func (t *SSHTransport) stream(ctx context.Context, command string, consume func(io.Reader) error) error {
	session, release, err := t.session(ctx)
	if err != nil {
		return err
	}
	broken := false
	defer func() { release(broken) }()
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
			_ = session.Close()
		case <-stop:
		}
	}()
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	if err := consume(stdout); err != nil {
		broken = true
		_ = session.Close()
		<-done
		return err
	}
	if err := <-done; err != nil {
		if ctx.Err() != nil {
			broken = true
			return ctx.Err()
		}
		return t.commandError(err, stderr.String())
	}
	return ctx.Err()
}

func (t *SSHTransport) run(ctx context.Context, command string, stdin io.Reader, stdout io.Writer) error {
	session, release, err := t.session(ctx)
	if err != nil {
		return err
	}
	broken := false
	defer func() { release(broken) }()
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
		broken = true
		_ = session.Close()
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
	sshstats.Record(addr, sshstats.DBDump, err)
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
