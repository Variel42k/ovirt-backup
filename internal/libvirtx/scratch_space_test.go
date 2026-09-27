package libvirtx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Режимы SFTP на поддельном хосте.
const (
	sftpOff       = "off"       // подсистема sftp выключена в sshd
	sftpNoStatVFS = "nostatvfs" // SFTP есть, расширения statvfs@openssh.com нет
	sftpStatVFS   = "statvfs"   // обычный OpenSSH
)

// fakeHost — SSH-сервер, который отвечает на stat -f и, в зависимости от
// режима, обслуживает SFTP.
type fakeHost struct {
	mode       string
	subsystems atomic.Int32
	execs      atomic.Int32

	mu      sync.Mutex
	lastCmd string
}

// statVFSCmder добавляет обработчику SFTP ответ на statvfs.
type statVFSCmder struct{ sftp.FileCmder }

func (statVFSCmder) StatVFS(*sftp.Request) (*sftp.StatVFS, error) {
	return &sftp.StatVFS{Bsize: 4096, Frsize: 1 << 20, Blocks: 100, Bfree: 60, Bavail: 50}, nil
}

func startFakeHost(t *testing.T, mode string) (*Conn, *fakeHost) {
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
	host := &fakeHost{mode: mode}
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go host.serve(raw, cfg)
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

func (h *fakeHost) serve(raw net.Conn, cfg *ssh.ServerConfig) {
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
		go h.session(ch, requests)
	}
}

func (h *fakeHost) session(ch ssh.Channel, requests <-chan *ssh.Request) {
	for req := range requests {
		switch req.Type {
		case "subsystem":
			var sub struct{ Name string }
			_ = ssh.Unmarshal(req.Payload, &sub)
			h.subsystems.Add(1)
			if sub.Name != "sftp" || h.mode == sftpOff {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			handlers := sftp.InMemHandler()
			if h.mode == sftpStatVFS {
				handlers.FileCmd = statVFSCmder{handlers.FileCmd}
			}
			go func() {
				_ = sftp.NewRequestServer(ch, handlers).Serve()
				_ = ch.Close()
			}()
		case "exec":
			var cmd struct{ Command string }
			_ = ssh.Unmarshal(req.Payload, &cmd)
			h.execs.Add(1)
			h.mu.Lock()
			h.lastCmd = cmd.Command
			h.mu.Unlock()
			_ = req.Reply(true, nil)
			_, _ = ch.Write([]byte("100 4096\n"))
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			_ = ch.Close()
		default:
			_ = req.Reply(false, nil)
		}
	}
}

func TestScratchSpaceFallsBackToStatCommand(t *testing.T) {
	for _, mode := range []string{sftpOff, sftpNoStatVFS} {
		t.Run(mode, func(t *testing.T) {
			conn, host := startFakeHost(t, mode)
			space := conn.NewScratchSpace("/var/lib/libvirt/qemu")
			defer space.Close()

			for i := 1; i <= 2; i++ {
				free, err := space.Free(context.Background())
				if err != nil {
					t.Fatalf("замер %d: %v", i, err)
				}
				if free != 100*4096 {
					t.Fatalf("замер %d: free = %d, want %d — по ответу stat", i, free, 100*4096)
				}
				if got := host.execs.Load(); got != int32(i) {
					t.Fatalf("замер %d: команд stat %d, want %d", i, got, i)
				}
			}
			// После первого отказа SFTP больше не пробуется: каждая попытка —
			// лишняя сессия в журнале хоста.
			if got := host.subsystems.Load(); got != 1 {
				t.Fatalf("запросов подсистемы sftp: %d, want 1", got)
			}
			host.mu.Lock()
			cmd := host.lastCmd
			host.mu.Unlock()
			if !strings.Contains(cmd, "stat -f") || !strings.Contains(cmd, "'/var/lib/libvirt/qemu'") {
				t.Fatalf("команда = %q", cmd)
			}
		})
	}
}

func TestScratchSpaceUsesStatVFSOverOneSession(t *testing.T) {
	conn, host := startFakeHost(t, sftpStatVFS)
	space := conn.NewScratchSpace("/var/lib/libvirt/qemu")
	defer space.Close()

	for i := 0; i < 3; i++ {
		free, err := space.Free(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if free != 50<<20 {
			t.Fatalf("free = %d, want %d — Bavail × Frsize", free, 50<<20)
		}
	}
	if host.execs.Load() != 0 || host.subsystems.Load() != 1 {
		t.Fatalf("команд %d, сессий sftp %d: нужна одна сессия SFTP и ни одной команды",
			host.execs.Load(), host.subsystems.Load())
	}
}
