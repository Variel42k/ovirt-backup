// Package sshtrust decides whether an SSH connection may proceed without a
// pinned host key, and collects the key the host offers so an operator has
// something to pin.
//
// The decision used to live in two places — the hypervisor connection and the
// SFTP repository — and both resolved an empty key the same silent way: connect
// anyway. That is the one failure mode nobody notices, because an intercepted
// connection looks exactly like a working one. Here the absence of a key is an
// error, and skipping verification is a separate, explicit, recorded choice.
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
)

// ErrNoHostKey means the connection has neither a pinned key nor a decision to
// go without one.
//
// The wording names the way out, because the operator meeting this message is
// usually adding a host and has no idea where a host key comes from.
var ErrNoHostKey = errors.New("не задан ключ хоста: получите отпечаток и сверьте его, " +
	"либо явно разрешите подключение без проверки")

// Callback builds the host key check for a connection.
//
// trustAny is deliberately a separate argument rather than "hostKey == \"\"":
// the caller has to have stored a decision somewhere for this to be reachable,
// and that decision is what the interface shows and the audit log records.
func Callback(hostKey string, trustAny bool) (ssh.HostKeyCallback, error) {
	return callback(hostKey, trustAny, false)
}

// AddressBoundCallback requires every pinned key to name the exact SSH
// endpoint it belongs to. It is used for clusters, where accepting one bare
// authorized_keys line for every node would allow a compromised member to
// impersonate another member.
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

// Fingerprint renders the SHA256 form an operator can compare against
// `ssh-keyscan` or `ssh-keygen -lf` output on the host itself.
func Fingerprint(key ssh.PublicKey) string { return ssh.FingerprintSHA256(key) }

// Key is a host key as offered by a server, in the two shapes the operator
// needs: one to store and one to read out loud while comparing.
type Key struct {
	// Line is the authorized_keys representation, which is what gets pinned.
	Line string `json:"line"`
	// Type is the key algorithm, e.g. ssh-ed25519.
	Type string `json:"type"`
	// Fingerprint is the SHA256 form shown for confirmation.
	Fingerprint string `json:"fingerprint"`
}

// errCollected aborts the handshake once the key is in hand.
var errCollected = errors.New("ключ получен")

// Scan returns the host key a server presents, without authenticating.
//
// No credentials are sent: the handshake is aborted from the host key callback,
// which runs before authentication. So this is safe to call against an address
// the operator has only typed and not yet trusted — which is precisely the
// moment it is needed.
//
// The result is not trustworthy on its own. Whoever could intercept the
// connection could also answer this scan. It exists so the operator has a
// fingerprint to compare with one obtained from the host by other means, and
// the interface has to say so.
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

	// The handshake is expected to fail with errCollected; any other outcome
	// means we never saw a key.
	client, _, _, err := ssh.NewClientConn(conn, addr, cfg)
	if client != nil {
		_ = client.Close()
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
