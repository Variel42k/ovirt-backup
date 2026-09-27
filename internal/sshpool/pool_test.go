package sshpool

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testServer — SSH-сервер в процессе теста. accepted считает TCP-подключения:
// ровно столько раз настоящий хост записал бы в журнал вход по ключу.
type testServer struct {
	addr     string
	accepted atomic.Int32
	mu       sync.Mutex
	conns    []net.Conn
	ln       net.Listener
}

func startServer(t *testing.T) *testServer {
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
	s := &testServer{addr: ln.Addr().String(), ln: ln}
	t.Cleanup(func() { _ = ln.Close(); s.dropAll() })

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			s.mu.Lock()
			s.conns = append(s.conns, raw)
			s.mu.Unlock()
			go s.serve(raw, cfg)
		}
	}()
	return s
}

func (s *testServer) serve(raw net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	go func() {
		for req := range reqs {
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "только session")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range requests {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				_, _ = ch.Write([]byte("ok\n"))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = ch.Close()
			}
		}()
	}
}

// dropAll обрывает все соединения со стороны сервера: так выглядит
// перезагруженный хост или соединение, закрытое межсетевым экраном.
func (s *testServer) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

func (s *testServer) dial(ctx context.Context) (*ssh.Client, error) {
	return ssh.Dial("tcp", s.addr, &ssh.ClientConfig{
		User: "jhvirt", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
}

func runOnce(t *testing.T, p *Pool, key string, dial Dialer) {
	t.Helper()
	session, done, err := p.Session(context.Background(), key, dial)
	if err != nil {
		t.Fatalf("сессия: %v", err)
	}
	out, err := session.Output("probe")
	done(err != nil)
	if err != nil || string(out) != "ok\n" {
		t.Fatalf("команда: %q, %v", out, err)
	}
}

// Главное свойство пула: десять операций — один вход на хост, а не десять.
func TestPoolReusesOneConnectionForManyOperations(t *testing.T) {
	srv := startServer(t)
	p := New(time.Minute, 8)
	defer p.Close()

	for i := 0; i < 10; i++ {
		runOnce(t, p, "host", srv.dial)
	}
	if got := srv.accepted.Load(); got != 1 {
		t.Fatalf("подключений к хосту = %d, ожидалось 1", got)
	}
}

// Хост оборвал соединение — следующая операция молча подключается заново.
func TestPoolRedialsAfterConnectionDrops(t *testing.T) {
	srv := startServer(t)
	p := New(time.Minute, 8)
	defer p.Close()

	runOnce(t, p, "host", srv.dial)
	srv.dropAll()
	time.Sleep(20 * time.Millisecond)
	runOnce(t, p, "host", srv.dial)
	if got := srv.accepted.Load(); got != 2 {
		t.Fatalf("подключений = %d, ожидалось 2: одно исходное и одно после обрыва", got)
	}
}

// Без операций соединение не висит вечно.
func TestPoolClosesIdleConnection(t *testing.T) {
	srv := startServer(t)
	p := New(30*time.Millisecond, 8)
	defer p.Close()

	runOnce(t, p, "host", srv.dial)
	if p.Size() != 1 {
		t.Fatalf("после операции пул должен держать соединение, держит %d", p.Size())
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.Size() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Size() != 0 {
		t.Fatal("простаивающее соединение не закрыто")
	}
}

// Больше MaxSessions одновременных сессий — второе соединение, а не отказ
// sshd «administratively prohibited».
func TestPoolOpensAnotherConnectionWhenSessionsExhausted(t *testing.T) {
	srv := startServer(t)
	p := New(time.Minute, 2)
	defer p.Close()

	var leases []*Lease
	for i := 0; i < 3; i++ {
		lease, err := p.Acquire(context.Background(), "host", srv.dial)
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	if got := srv.accepted.Load(); got != 2 {
		t.Fatalf("подключений = %d, ожидалось 2 при пределе 2 сессии на соединение", got)
	}
	for _, l := range leases {
		l.Release()
	}
}

// Разные ключи — разные соединения: правка настроек хоста уводит операции на
// новое соединение с новыми учётными данными.
func TestPoolSeparatesKeys(t *testing.T) {
	srv := startServer(t)
	p := New(time.Minute, 8)
	defer p.Close()

	runOnce(t, p, "host-old-key", srv.dial)
	runOnce(t, p, "host-new-key", srv.dial)
	if got := srv.accepted.Load(); got != 2 {
		t.Fatalf("подключений = %d, ожидалось 2", got)
	}
}
