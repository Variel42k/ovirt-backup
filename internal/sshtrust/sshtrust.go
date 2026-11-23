// Пакет sshtrust решает, может ли соединение SSH протекать без прикреплённого ключа узла, и
// собирает ключ, предлагаемый узлом, таким образом, оператору есть что прикрепить.
//
// Раньше подключения к гипервизору и SFTP при пустом ключе молча продолжались.
// Перехваченное соединение при этом выглядело рабочим. Отсутствие ключа теперь вызывает
// ошибку. Отключение проверки требует явного решения, которое записывается.
package sshtrust

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/Variel42k/ovirt-backup/internal/sshstats"
)

// ErrNoHostKey означает, что соединение не имеет ни закрепленного ключа, ни решения
// обходиться без него.
//
// Формулировка называет выход, потому что оператор, встречающийся с этим сообщением, обычно
// добавляет хост и понятия не имеет, откуда берется ключ хоста.
var ErrNoHostKey = errors.New("не задан ключ хоста: получите отпечаток и сверьте его, " +
	"либо явно разрешите подключение без проверки")

// Callback создает проверку ключа хоста для соединения.
//
// trustAny передаётся отдельно от hostKey. Чтобы отключить проверку, вызывающий код должен
// сохранить явное решение, которое видно в интерфейсе и журнале аудита.
func Callback(hostKey string, trustAny bool) (ssh.HostKeyCallback, error) {
	return callback(hostKey, trustAny, false)
}

// AddressBoundCallback требует, чтобы каждый закреплённый SSH-ключ был привязан к точному
// адресу. Это не позволяет скомпрометированному узлу кластера выдавать себя за соседний по
// общему ключу без адреса.
func AddressBoundCallback(hostKey string, trustAny bool) (ssh.HostKeyCallback, error) {
	return callback(hostKey, trustAny, true)
}

func callback(hostKey string, trustAny, requireAddress bool) (ssh.HostKeyCallback, error) {
	pinned := strings.TrimSpace(hostKey)
	if pinned == "" {
		if trustAny {
			return ssh.InsecureIgnoreHostKey(), nil
		}
		return nil, ErrNoHostKey
	}

	type entry struct {
		hosts []string
		key   ssh.PublicKey
	}
	var entries []entry
	for lineNumber, line := range strings.Split(pinned, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("разбор ключа хоста, строка %d: ожидается authorized_keys или known_hosts", lineNumber+1)
		}
		if strings.HasPrefix(fields[0], "ssh-") || strings.HasPrefix(fields[0], "ecdsa-") ||
			strings.HasPrefix(fields[0], "sk-") {
			if requireAddress {
				return nil, fmt.Errorf("разбор ключа хоста, строка %d: для кластера нужна адресная строка known_hosts", lineNumber+1)
			}
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				return nil, fmt.Errorf("разбор ключа хоста, строка %d: %w", lineNumber+1, err)
			}
			entries = append(entries, entry{key: key})
			continue
		}
		if strings.HasPrefix(fields[0], "|") || strings.ContainsAny(fields[0], "*?!") {
			return nil, fmt.Errorf("разбор ключа хоста, строка %d: хешированные ключи и шаблоны не поддерживаются", lineNumber+1)
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[1:], " ")))
		if err != nil {
			return nil, fmt.Errorf("разбор ключа хоста, строка %d: %w", lineNumber+1, err)
		}
		entries = append(entries, entry{hosts: strings.Split(fields[0], ","), key: key})
	}
	if len(entries) == 0 {
		return nil, ErrNoHostKey
	}

	return func(hostname string, _ net.Addr, presented ssh.PublicKey) error {
		normalized := strings.ToLower(knownhosts.Normalize(hostname))
		var expected []ssh.PublicKey
		for _, candidate := range entries {
			if len(candidate.hosts) == 0 {
				expected = append(expected, candidate.key)
				continue
			}
			for _, host := range candidate.hosts {
				if strings.ToLower(knownhosts.Normalize(host)) == normalized {
					expected = append(expected, candidate.key)
					break
				}
			}
		}
		if len(expected) == 0 {
			return fmt.Errorf("для SSH-узла %s нет закреплённого ключа", hostname)
		}
		for _, key := range expected {
			if key.Type() == presented.Type() && bytes.Equal(key.Marshal(), presented.Marshal()) {
				return nil
			}
		}
		return fmt.Errorf("ключ SSH-узла %s не совпал: предъявлен %s", hostname, Fingerprint(presented))
	}, nil
}

// Fingerprint возвращает отпечаток SHA256 для сравнения с выводом ssh-keyscan или ssh-keygen
// -lf на самом хосте.
func Fingerprint(key ssh.PublicKey) string { return ssh.FingerprintSHA256(key) }

// Key — это хост-ключ, предлагаемый сервером, в двух формах, необходимых оператору: один
// для хранения и один для чтения вслух при сравнении.
type Key struct {
	// Линия — это authorized_keys представление, которое и закрепляется.
	Line string `json:"line"`
	// Тип является ключевым алгоритмом, например ssh-ed25519.
	Type string `json:"type"`
	// Отпечаток пальца — форма SHA256, показанная для подтверждения.
	Fingerprint string `json:"fingerprint"`
}

// errCollected прерывает рукопожатие, как только ключ находится в руке.
var errCollected = errors.New("ключ получен")

// Сканирование возвращает ключ хоста, представленный сервером, без аутентификации.
//
// Проверка ключа хоста прерывает согласование до аутентификации. Учётные данные не
// отправляются, поэтому можно запросить отпечаток по ещё не проверенному адресу.
//
// Ответ на запрос отпечатка может подменить тот, кто перехватывает соединение. Оператор
// должен сравнить его со значением, полученным от хоста другим способом. Интерфейс поясняет
// это.
func Scan(ctx context.Context, addr string, timeout time.Duration) (*Key, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	var found ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User:    "ovirt-backup-host-key-scan",
		Auth:    nil,
		Timeout: timeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			found = key
			return errCollected
		},
	}

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("подключение к %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	// Потерпит неудачу errCollected. Любой другой результат означает, что мы никогда не
	// видели ключа.
	client, _, _, err := ssh.NewClientConn(conn, addr, cfg)
	if client != nil {
		_ = client.Close()
	}
	// Рукопожатие обрывается нарочно, как только ключ получен: удачей считается
	// полученный ключ, а не ошибка рукопожатия.
	if found != nil {
		sshstats.Record(addr, sshstats.HostKey, nil)
	} else {
		sshstats.Record(addr, sshstats.HostKey, err)
	}
	if found == nil {
		if err != nil {
			return nil, fmt.Errorf("рукопожатие с %s: %w", addr, err)
		}
		return nil, fmt.Errorf("хост %s не предъявил ключ", addr)
	}

	return &Key{
		Line:        strings.TrimSpace(string(ssh.MarshalAuthorizedKey(found))),
		Type:        found.Type(),
		Fingerprint: Fingerprint(found),
	}, nil
}
