package dbdump

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// helperServer — SSH-сервер в процессе теста, изображающий хелпер дампов.
// exec получает команду и пишет ответ в канал; accepted считает подключения —
// столько раз настоящий хост записал бы в журнал вход по ключу.
type helperServer struct {
	addr     string
	accepted atomic.Int32
	exec     func(cmd string, ch ssh.Channel)
}

func startHelperServer(t *testing.T, exec func(cmd string, ch ssh.Channel)) *helperServer {
	t.Helper()
	_, hostKey, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &helperServer{addr: ln.Addr().String(), exec: exec}
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			go s.serve(raw, cfg)
		}
	}()
	return s
}

func (s *helperServer) serve(raw net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
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
				var payload struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &payload)
				_ = req.Reply(true, nil)
				go s.exec(payload.Command, ch)
			}
		}()
	}
}

func exitWith(ch ssh.Channel, status uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	_ = ch.Close()
}

func testTransport(t *testing.T, addr string) *SSHTransport {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	host, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)
	transport, err := NewSSHTransport(&model.DBHost{
		ID: "db-" + t.Name(), Name: "db-01", Address: host, Port: port, Username: "jhvirt_dump",
		PrivateKey: string(pem.EncodeToMemory(block)), TrustAnyHostKey: true,
	}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return transport
}

// Поток: строки счётчиков, «СУБД не ответила» — замер с ошибкой, конец бэкапа
// закрывает сессию, но не соединение: следующий бэкап войдёт без нового входа.
func TestWatchStatsStreamsSamplesOverOneSession(t *testing.T) {
	var commands sync.Map
	srv := startHelperServer(t, func(cmd string, ch ssh.Channel) {
		commands.Store(cmd, true)
		for _, line := range []string{
			"jhvirt-db-stats/1 postgresql 10 1 3 1000",
			"jhvirt-db-stats/1 postgresql unavailable",
			"jhvirt-db-stats/1 postgresql 12 1 4 2000",
		} {
			if _, err := ch.Write([]byte(line + "\n")); err != nil {
				return
			}
		}
		// Как настоящий хелпер: ждёт до следующего замера, пока служба не закроет сессию.
		buf := make([]byte, 1)
		_, _ = ch.Read(buf)
		exitWith(ch, 0)
	})
	transport := testTransport(t, srv.addr)

	ctx, cancel := context.WithCancel(context.Background())
	var (
		mu      sync.Mutex
		samples []Stats
		errs    int
	)
	done := make(chan error, 1)
	go func() {
		done <- transport.WatchStats(ctx, model.DBEnginePostgreSQL, 10*time.Second, func(s Stats, err error) {
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				return
			}
			samples = append(samples, s)
			if len(samples) == 2 {
				cancel()
			}
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("конец бэкапа должен вернуть отмену, получено %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("поток статистики не завершился после отмены")
	}
	mu.Lock()
	if len(samples) != 2 || samples[1].Commits != 12 || errs != 1 {
		t.Fatalf("замеры %+v, ошибок %d; ожидалось два замера и одна «не ответила»", samples, errs)
	}
	mu.Unlock()
	if _, ok := commands.Load("/usr/local/sbin/jhvirt-db-dump stats-watch postgresql 10"); !ok {
		t.Error("хелперу ушла не та команда")
	}

	// Соединение осталось в пуле: следующая операция не входит на хост заново.
	// Что ответит сервер, не важно — важен только счётчик подключений.
	_ = transport.run(context.Background(), "/usr/local/sbin/jhvirt-db-dump probe", nil, &strings.Builder{})
	if got := srv.accepted.Load(); got != 1 {
		t.Fatalf("подключений к хосту = %d, ожидалось одно на поток и следующую операцию", got)
	}
}

// Старый хелпер без stats-watch отвечает ошибкой без единой строки — служба
// переходит на замеры по одному.
func TestWatchStatsReportsOldHelper(t *testing.T) {
	srv := startHelperServer(t, func(cmd string, ch ssh.Channel) {
		_, _ = ch.Stderr().Write([]byte("jhvirt-db-dump: unknown action\n"))
		exitWith(ch, 64)
	})
	transport := testTransport(t, srv.addr)

	calls := 0
	err := transport.WatchStats(context.Background(), model.DBEnginePostgreSQL, 10*time.Second, func(Stats, error) { calls++ })
	if err == nil || !strings.Contains(err.Error(), "unknown action") || calls != 0 {
		t.Fatalf("ожидалась ошибка старого хелпера без замеров: %v, замеров %d", err, calls)
	}
}
