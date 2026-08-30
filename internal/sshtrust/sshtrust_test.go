package sshtrust

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The whole point of this package: no key and no explicit decision must not
// silently become "connect to anyone".
func TestCallbackRefusesWithoutKeyOrDecision(t *testing.T) {
	if _, err := Callback("", false); !errors.Is(err, ErrNoHostKey) {
		t.Fatalf("ожидалась ErrNoHostKey, получено %v", err)
	}
	if _, err := Callback("   \n", false); !errors.Is(err, ErrNoHostKey) {
		t.Fatalf("пробелы вместо ключа должны считаться отсутствием ключа, получено %v", err)
	}
}

func TestCallbackAllowsExplicitlyUnverified(t *testing.T) {
	cb, err := Callback("", true)
	if err != nil {
		t.Fatalf("явное разрешение должно приниматься: %v", err)
	}
	if cb == nil {
		t.Fatal("проверка не построена")
	}
}

// A pinned key must actually pin: the same key passes, a different one does not.
func TestCallbackPinsTheConfiguredKey(t *testing.T) {
	pinned, pinnedLine := generateHostKey(t)
	other, _ := generateHostKey(t)

	cb, err := Callback(pinnedLine, false)
	if err != nil {
		t.Fatalf("построение проверки: %v", err)
	}

	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 22}
	if err := cb("host:22", addr, pinned.PublicKey()); err != nil {
		t.Errorf("свой ключ должен приниматься: %v", err)
	}
	if err := cb("host:22", addr, other.PublicKey()); err == nil {
		t.Error("чужой ключ принят — подмена хоста осталась бы незамеченной")
	}
}

// A pinned key that does not parse must fail loudly at configuration time,
// not degrade into no verification at connection time.
func TestCallbackRejectsMalformedKey(t *testing.T) {
	if _, err := Callback("не ключ вовсе", false); err == nil {
		t.Fatal("нечитаемый ключ принят")
	}
	if _, err := Callback("не ключ вовсе", true); err == nil {
		t.Fatal("нечитаемый ключ принят при разрешённом подключении без проверки")
	}
}

// Scan has to return the key the host actually presented, and must not
// authenticate: it runs against addresses nobody has trusted yet.
func TestScanReturnsThePresentedKey(t *testing.T) {
	signer, line := generateHostKey(t)
	addr, stop := startSSHServer(t, signer)
	defer stop()

	key, err := Scan(context.Background(), addr, 5*time.Second)
	if err != nil {
		t.Fatalf("сбор ключа: %v", err)
	}
	if key.Line != line {
		t.Errorf("ключ разошёлся:\n получено %q\n ожидалось %q", key.Line, line)
	}
	if !strings.HasPrefix(key.Fingerprint, "SHA256:") {
		t.Errorf("отпечаток не в форме SHA256: %q", key.Fingerprint)
	}
	if key.Type != signer.PublicKey().Type() {
		t.Errorf("тип ключа %q, ожидался %q", key.Type, signer.PublicKey().Type())
	}
}

func TestScanReportsUnreachableHost(t *testing.T) {
	// Port 1 on the loopback: nothing listens there, and the attempt fails fast.
	if _, err := Scan(context.Background(), "127.0.0.1:1", time.Second); err == nil {
		t.Fatal("недоступный хост не должен возвращать ключ")
	}
}

func generateHostKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("генерация ключа: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("подписант: %v", err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	return signer, line
}

// startSSHServer runs a listener that completes just enough of the handshake
// for a client to see the host key.
func startSSHServer(t *testing.T, signer ssh.Signer) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("прослушивание: %v", err)
	}

	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// The client aborts once it has the key, so this handshake is expected
		// to fail; the error is not interesting.
		serverConn, _, _, _ := ssh.NewServerConn(conn, cfg)
		if serverConn != nil {
			_ = serverConn.Close()
		}
	}()

	return listener.Addr().String(), func() {
		_ = listener.Close()
		<-done
	}
}
