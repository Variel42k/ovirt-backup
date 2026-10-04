// Package gitclean находит в истории репозиториев GitLab большие файлы и
// каталоги-артефакты и убирает их, переписывая историю.
//
// Сама работа с репозиториями идёт на ВМ с GitLab, в хелпере
// jhvirt-gitlab-clean: служба вызывает его по SSH и разбирает ответы. Команды
// — только операции протокола с проверенными аргументами; хелпер проверяет их
// ещё раз на своей стороне.
package gitclean

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/sshpool"
	"github.com/Variel42k/ovirt-backup/internal/sshstats"
	"github.com/Variel42k/ovirt-backup/internal/sshtrust"
)

const (
	helper   = "jhvirt-gitlab-clean"
	protocol = "jhvirt-gitlab-clean/1"
	// topPaths — сколько самых тяжёлых путей репозитория попадает в отчёт.
	topPaths = 30
)

// RepoRef — репозиторий в списке хелпера.
type RepoRef struct {
	Path      string
	FullPath  string
	DiskBytes int64
}

// CleanResult — итог очистки одного репозитория.
type CleanResult struct {
	BeforeBytes int64
	AfterBytes  int64
	InPool      bool
}

// Transport — операции хелпера. Интерфейс нужен тестам: движок проверяется с
// поддельным хелпером, без SSH и без GitLab.
type Transport interface {
	Probe(ctx context.Context) (*model.GitlabProbe, error)
	List(ctx context.Context) ([]RepoRef, error)
	Analyze(ctx context.Context, repo string, rules model.GitCleanRules) (*model.GitCleanRepo, error)
	Clean(ctx context.Context, repo string, rules model.GitCleanRules) (*CleanResult, error)
}

// repoPath — путь репозитория, как его принимает хелпер. Проверяется и здесь:
// в команду попадает только то, что не может быть разобрано оболочкой иначе.
var repoPath = regexp.MustCompile(`^[A-Za-z0-9@._/+-]+\.git$`)

// ValidRepoPath reports whether a repository path may be sent to the helper.
func ValidRepoPath(path string) bool {
	return repoPath.MatchString(path) && !strings.HasPrefix(path, "/") && !strings.Contains(path, "..")
}

// ruleArgs превращает правила в аргументы хелпера. Имена уже проверены
// GitCleanRules.Normalize и состоят только из безопасных символов.
func ruleArgs(rules model.GitCleanRules) string {
	var parts []string
	if len(rules.Dirs) > 0 {
		parts = append(parts, "dirs="+strings.Join(rules.Dirs, ","))
	}
	if len(rules.Extensions) > 0 {
		parts = append(parts, "exts="+strings.Join(rules.Extensions, ","))
	}
	parts = append(parts, "big="+strconv.FormatInt(rules.BigFileBytes, 10))
	return strings.Join(parts, " ")
}

// ParseProbe разбирает ответ probe. Первая строка обязана быть версией
// протокола: чужая программа на месте хелпера не должна сойти за него.
func ParseProbe(out string) (*model.GitlabProbe, error) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(out), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != protocol {
		return nil, fmt.Errorf("на хосте не хелпер %s: первая строка ответа %q", protocol, firstLine(out))
	}
	probe := &model.GitlabProbe{}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "hostname":
			probe.Hostname = value
		case "user":
			probe.User = value
		case "repos_root":
			probe.ReposRoot = value
		case "gitlab":
			probe.GitLab = value
		case "git":
			probe.Git = value
		case "filter_repo":
			probe.FilterRepo = value
		case "clean_allowed":
			probe.CleanAllowed = value == "1"
		case "services_running":
			for _, name := range strings.Split(value, ",") {
				if name = strings.TrimSpace(name); name != "" {
					probe.ServicesRunning = append(probe.ServicesRunning, name)
				}
			}
		}
	}
	if probe.ReposRoot == "" {
		return nil, errors.New("хелпер не сообщил каталог репозиториев")
	}
	return probe, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 120 {
		line = line[:120] + "…"
	}
	return line
}

// ParseList разбирает ответ list: «repo<TAB>путь<TAB>КиБ<TAB>проект».
func ParseList(r io.Reader) ([]RepoRef, error) {
	var out []RepoRef
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 3 || fields[0] != "repo" {
			continue
		}
		if !ValidRepoPath(fields[1]) {
			return nil, fmt.Errorf("хелпер вернул недопустимый путь репозитория %q", fields[1])
		}
		ref := RepoRef{Path: fields[1], DiskBytes: atoi(fields[2]) << 10}
		if len(fields) > 3 {
			ref.FullPath = fields[3]
		}
		out = append(out, ref)
	}
	return out, scanner.Err()
}

// ParseAnalysis разбирает ответ analyze одного репозитория.
//
// Ответ без завершающей строки done считается оборванным: неполный отчёт
// выглядел бы как «мусора нет», и репозиторий остался бы невычищенным.
func ParseAnalysis(r io.Reader, repo string) (*model.GitCleanRepo, error) {
	report := &model.GitCleanRepo{Path: repo}
	done := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		switch fields[0] {
		case "repo":
			if len(fields) >= 3 {
				report.DiskBytes = atoi(fields[2]) << 10
			}
			if len(fields) >= 4 {
				report.FullPath = fields[3]
			}
		case "note":
			if len(fields) >= 2 && fields[1] == "pool" {
				report.InPool = true
			}
		case "total":
			if len(fields) >= 2 {
				report.BlobCount = atoi(fields[1])
			}
		case "rule":
			if len(fields) >= 5 {
				report.Findings = append(report.Findings, model.GitCleanFinding{
					Rule: fields[1], Count: atoi(fields[2]), Bytes: atoi(fields[3]), DiskBytes: atoi(fields[4]),
				})
			}
		case "path":
			// path<TAB>на диске<TAB>файлов<TAB>байт<TAB>правило<TAB>путь
			if len(fields) >= 6 {
				report.TopPaths = append(report.TopPaths, model.GitCleanPath{
					DiskBytes: atoi(fields[1]), Count: atoi(fields[2]), Bytes: atoi(fields[3]),
					Rule: fields[4], Path: strings.Join(fields[5:], "\t"),
				})
			}
		case "done":
			done = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !done {
		return nil, errors.New("ответ хелпера оборван: анализ репозитория не завершён")
	}
	sort.Slice(report.Findings, func(i, j int) bool { return report.Findings[i].DiskBytes > report.Findings[j].DiskBytes })
	sort.SliceStable(report.TopPaths, func(i, j int) bool { return report.TopPaths[i].DiskBytes > report.TopPaths[j].DiskBytes })
	for _, finding := range report.Findings {
		report.ReclaimBytes += finding.DiskBytes
	}
	return report, nil
}

// ParseClean разбирает ответ clean одного репозитория.
func ParseClean(r io.Reader) (*CleanResult, error) {
	result := &CleanResult{}
	cleaned, done := false, false
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		switch fields[0] {
		case "note":
			if len(fields) >= 2 && fields[1] == "pool" {
				result.InPool = true
			}
		case "cleaned":
			if len(fields) >= 4 {
				result.BeforeBytes, result.AfterBytes = atoi(fields[2])<<10, atoi(fields[3])<<10
				cleaned = true
			}
		case "done":
			done = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !cleaned || !done {
		return nil, errors.New("ответ хелпера оборван: неизвестно, завершилась ли очистка репозитория")
	}
	return result, nil
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}

// SSHTransport ходит к хелперу по SSH через общий пул соединений.
type SSHTransport struct {
	host    *model.GitlabHost
	timeout time.Duration
	hostKey ssh.HostKeyCallback
	poolKey string
}

// NewSSHTransport готовит канал к ВМ с GitLab. Без закреплённого ключа хоста и
// без явного отказа от проверки подключения не будет.
func NewSSHTransport(h *model.GitlabHost, timeout time.Duration) (*SSHTransport, error) {
	if h == nil {
		return nil, errors.New("не задано подключение к GitLab")
	}
	if strings.TrimSpace(h.PrivateKey) == "" {
		return nil, errors.New("у подключения нет SSH-ключа")
	}
	callback, err := sshtrust.Callback(h.HostKey, h.TrustAnyHostKey)
	if err != nil {
		return nil, fmt.Errorf("ключ хоста %s: %w", h.Name, err)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	key := sshpool.Key("gitlab-host", h.ID, strings.TrimSpace(h.Address), fmt.Sprint(h.Port), h.Username,
		h.PrivateKey, h.HostKey, fmt.Sprint(h.TrustAnyHostKey))
	return &SSHTransport{host: h, timeout: timeout, hostKey: callback, poolKey: key}, nil
}

func (t *SSHTransport) Probe(ctx context.Context) (*model.GitlabProbe, error) {
	out, err := t.run(ctx, helper+" probe")
	if err != nil {
		return nil, err
	}
	return ParseProbe(out.String())
}

func (t *SSHTransport) List(ctx context.Context) ([]RepoRef, error) {
	out, err := t.run(ctx, helper+" list")
	if err != nil {
		return nil, err
	}
	return ParseList(out)
}

func (t *SSHTransport) Analyze(ctx context.Context, repo string, rules model.GitCleanRules) (*model.GitCleanRepo, error) {
	if !ValidRepoPath(repo) {
		return nil, fmt.Errorf("недопустимый путь репозитория %q", repo)
	}
	out, err := t.run(ctx, fmt.Sprintf("%s analyze %s %s top=%d", helper, repo, ruleArgs(rules), topPaths))
	if err != nil {
		return nil, err
	}
	return ParseAnalysis(out, repo)
}

func (t *SSHTransport) Clean(ctx context.Context, repo string, rules model.GitCleanRules) (*CleanResult, error) {
	if !ValidRepoPath(repo) {
		return nil, fmt.Errorf("недопустимый путь репозитория %q", repo)
	}
	if rules.Empty() {
		return nil, errors.New("не задано ни одного правила очистки")
	}
	out, err := t.run(ctx, fmt.Sprintf("%s clean %s %s", helper, repo, ruleArgs(rules)))
	if err != nil {
		return nil, err
	}
	return ParseClean(out)
}

// run выполняет одну операцию хелпера и возвращает её вывод. Отмена закрывает
// сессию и помечает соединение негодным: sshd завершит процесс хелпера.
func (t *SSHTransport) run(ctx context.Context, command string) (*bytes.Buffer, error) {
	session, release, err := sshpool.Shared().Session(ctx, t.poolKey, t.connect)
	if err != nil {
		return nil, fmt.Errorf("SSH-сессия на %s: %w", t.host.Name, err)
	}
	broken := false
	defer func() { release(broken) }()

	stdout := &limitedBuffer{limit: 64 << 20}
	stderr := &limitedBuffer{limit: 64 << 10}
	session.Stdout, session.Stderr = stdout, stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err := <-done:
		if err != nil {
			return nil, t.commandError(err, stderr.String())
		}
		return &stdout.buf, nil
	case <-ctx.Done():
		broken = true
		_ = session.Close()
		<-done
		return nil, ctx.Err()
	}
}

func (t *SSHTransport) connect(ctx context.Context) (*ssh.Client, error) {
	signer, err := ssh.ParsePrivateKey([]byte(t.host.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("разбор SSH-ключа подключения: %w", err)
	}
	port := t.host.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(strings.TrimSpace(t.host.Address), fmt.Sprint(port))
	raw, err := (&net.Dialer{Timeout: t.timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("SSH к %s: %w", addr, err)
	}
	_ = raw.SetDeadline(time.Now().Add(t.timeout))
	// Только ключ: пароль не предлагается никогда.
	conn, channels, requests, err := ssh.NewClientConn(raw, addr, &ssh.ClientConfig{
		User: t.host.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: t.hostKey, Timeout: t.timeout,
	})
	sshstats.Record(addr, sshstats.GitlabClean, err)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("SSH-аутентификация на %s: %w", addr, err)
	}
	_ = raw.SetDeadline(time.Time{})
	return ssh.NewClient(conn, channels, requests), nil
}

func (t *SSHTransport) commandError(err error, stderr string) error {
	if detail := strings.TrimSpace(stderr); detail != "" {
		// Последняя строка — сообщение самого хелпера; выше идёт вывод git.
		lines := strings.Split(detail, "\n")
		return fmt.Errorf("хелпер на %s: %s", t.host.Name, strings.TrimSpace(lines[len(lines)-1]))
	}
	return fmt.Errorf("хелпер на %s: %w", t.host.Name, err)
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
