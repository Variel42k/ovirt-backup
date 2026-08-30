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
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
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
	pinned := strings.TrimSpace(hostKey)
	if pinned == "" {
		if trustAny {
			return ssh.InsecureIgnoreHostKey(), nil
		}
		return nil, ErrNoHostKey
	}

	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pinned))
	if err != nil {
		return nil, fmt.Errorf("разбор ключа хоста: %w (ожидается формат authorized_keys)", err)
	}
	return ssh.FixedHostKey(parsed), nil
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
