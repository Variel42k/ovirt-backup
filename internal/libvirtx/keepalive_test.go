package libvirtx

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// keepaliveHost считает keepalive-запросы; silent — хост, который на них не
// отвечает (оборванное соединение, которое TCP ещё не заметил).
type keepaliveHost struct {
	silent   bool
	requests atomic.Int32
}

func startKeepaliveHost(t *testing.T, silent bool) (*Conn, *keepaliveHost) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	host := &keepaliveHost{silent: silent}
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		_, chans, reqs, err := ssh.NewServerConn(raw, cfg)
		if err != nil {
			return
		}
		go func() {
			for ch := range chans {
				_ = ch.Reject(ssh.Prohibited, "не нужно")
			}
		}()
		for req := range reqs {
			if req.Type == "keepalive@openssh.com" {
				host.requests.Add(1)
			}
			if !host.silent && req.WantReply {
				// Как OpenSSH: неизвестный глобальный запрос — отказ, но ответ.
				_ = req.Reply(false, nil)
			}
		}
	}()

	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User: "root", HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // тестовый сервер
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &Conn{cfg: Config{Host: "test"}, ssh: client}, host
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestKeepaliveRepeatsAndStopsOnClose(t *testing.T) {
	conn, host := startKeepaliveHost(t, false)
	silent := atomic.Bool{}
	conn.StartKeepalive(10*time.Millisecond, func() { silent.Store(true) })
	conn.StartKeepalive(10*time.Millisecond, nil) // повторный запуск ничего не добавляет

	waitFor(t, "трёх keepalive", func() bool { return host.requests.Load() >= 3 })
	if silent.Load() {
		t.Fatal("хост отвечает — сообщать о молчании нельзя")
	}

	conn.mu.Lock()
	conn.closed = true
	conn.stopKeepaliveLocked()
	conn.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	after := host.requests.Load()
	time.Sleep(60 * time.Millisecond)
	if got := host.requests.Load(); got != after {
		t.Fatalf("после закрытия keepalive продолжается: %d → %d", after, got)
	}
}

func TestKeepaliveSilentHostGetsOneRequestAndOneWarning(t *testing.T) {
	conn, host := startKeepaliveHost(t, true)
	var warnings atomic.Int32
	conn.StartKeepalive(10*time.Millisecond, func() { warnings.Add(1) })

	waitFor(t, "сообщения о молчании", func() bool { return warnings.Load() == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := host.requests.Load(); got != 1 {
		t.Fatalf("keepalive-запросов %d: пока нет ответа на прошлый, новый не отправляется", got)
	}
	if got := warnings.Load(); got != 1 {
		t.Fatalf("сообщений о молчании %d, want 1", got)
	}

	// Keepalive соединение не обрывает: это решает проверка пула.
	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	if closed {
		t.Fatal("keepalive не должен закрывать соединение")
	}
	_ = conn.ssh.Close()
}
