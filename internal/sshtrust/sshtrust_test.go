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
	"golang.org/x/crypto/ssh/knownhosts"
)

// Без ключа или явного решения пропустить проверку нельзя автоматически доверять любому
// хосту.
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

// Прикрепленный ключ должен фактически прикрепляться: тот же ключ проходит, другой нет.
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

func TestCallbackPinsEachKnownHostSeparately(t *testing.T) {
	first, _ := generateHostKey(t)
	second, _ := generateHostKey(t)
	bundle := strings.Join([]string{
		knownhosts.Line([]string{"[10.0.0.11]:22"}, first.PublicKey()),
		knownhosts.Line([]string{"[10.0.0.12]:22"}, second.PublicKey()),
	}, "\n")
	cb, err := Callback(bundle, false)
	if err != nil {
		t.Fatalf("построение проверки кластера: %v", err)
	}
	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 11), Port: 22}
	if err := cb("10.0.0.11:22", addr, first.PublicKey()); err != nil {
		t.Fatalf("ключ первого узла не принят: %v", err)
	}
	if err := cb("10.0.0.12:22", addr, second.PublicKey()); err != nil {
		t.Fatalf("ключ второго узла не принят: %v", err)
	}
	if err := cb("10.0.0.11:22", addr, second.PublicKey()); err == nil {
		t.Fatal("ключ второго узла принят от имени первого")
	}
	if err := cb("10.0.0.13:22", addr, first.PublicKey()); err == nil {
		t.Fatal("узел без отдельной привязки принят")
	}
}

func TestAddressBoundCallbackRejectsClusterWideBareKey(t *testing.T) {
	key, line := generateHostKey(t)
	if _, err := AddressBoundCallback(line, false); err == nil {
		t.Fatal("общий authorized_keys ключ принят для всех узлов кластера")
	}
	bound := knownhosts.Line([]string{"[10.0.0.11]:22"}, key.PublicKey())
	cb, err := AddressBoundCallback(bound, false)
	if err != nil {
		t.Fatalf("адресный ключ кластера отклонён: %v", err)
	}
	if err := cb("10.0.0.11:22", &net.TCPAddr{}, key.PublicKey()); err != nil {
		t.Fatalf("адресный ключ не прошёл проверку: %v", err)
	}
}

func TestCallbackRejectsKnownHostsPatterns(t *testing.T) {
	_, line := generateHostKey(t)
	for _, host := range []string{"*.example.org", "|1|hash|hash"} {
		if _, err := Callback(host+" "+line, false); err == nil {
			t.Errorf("неоднозначный шаблон known_hosts принят: %s", host)
		}
	}
}

// Прикрепленный ключ, который не разбирается, должен громко выходить из строя во время
// настройки, а не деградировать в отсутствие проверки во время подключения.
func TestCallbackRejectsMalformedKey(t *testing.T) {
	if _, err := Callback("не ключ вовсе", false); err == nil {
		t.Fatal("нечитаемый ключ принят")
	}
	if _, err := Callback("не ключ вовсе", true); err == nil {
		t.Fatal("нечитаемый ключ принят при разрешённом подключении без проверки")
	}
}

// Сканирование должно возвращать ключ, фактически представленный хостом, и не должно
// аутентифицироваться: он работает по адресам, которым никто еще не доверял.
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
	// Порт 1 на loopback: там ничего не слушается, и попытка быстро проваливается.
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

// startSSHServer запускает слушатель, который завершает достаточное количество рукопожатия,
// чтобы клиент мог увидеть ключ хоста.
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
		// Клиент прерывает работу, как только у него есть ключ, поэтому ожидается, что это
		// рукопожатие потерпит неудачу; Ошибка не интересна.
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
