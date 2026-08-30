package model

import (
	"strings"
	"testing"
)

const validHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ8xVQ0nMPUNbwvUuPy4LtRsJvVKLxkPHtoT1P9QzUmB"

func kvmServer() *Server {
	return &Server{
		Name: "гипервизор", Kind: KindKVM, Username: "svc",
		SSHHost: "kvm1.example.org", SSHPrivateKey: "ключ",
	}
}

// Ключ хоста проверяется при сохранении, а не при подключении: подключение
// случается ночью в фоновом задании, и отказ там увидит не тот, кто заводил
// хост, и не тогда, когда может это исправить.
func TestLibvirtServerNeedsHostKeyDecision(t *testing.T) {
	srv := kvmServer()

	err := srv.Validate()
	if err == nil {
		t.Fatal("подключение без ключа хоста принято")
	}
	// Сообщение обязано называть выход: оператор, впервые заводящий хост, не
	// знает, откуда берётся ключ хоста.
	if !strings.Contains(err.Error(), "отпечаток") {
		t.Errorf("в сообщении нет подсказки, что делать: %v", err)
	}
}

func TestLibvirtServerAcceptsPinnedKey(t *testing.T) {
	srv := kvmServer()
	srv.SSHHostKey = validHostKey

	if err := srv.Validate(); err != nil {
		t.Fatalf("подключение с ключом хоста отвергнуто: %v", err)
	}
}

func TestLibvirtServerAcceptsExplicitTrust(t *testing.T) {
	srv := kvmServer()
	srv.SSHTrustAnyHostKey = true

	if err := srv.Validate(); err != nil {
		t.Fatalf("явное разрешение отвергнуто: %v", err)
	}
}

// Пробелы вместо ключа — это отсутствие ключа, а не ключ. Иначе правило
// обходится случайно, одним лишним переводом строки в форме.
func TestLibvirtServerTreatsBlankKeyAsMissing(t *testing.T) {
	srv := kvmServer()
	srv.SSHHostKey = "  \n\t "

	if err := srv.Validate(); err == nil {
		t.Fatal("пробелы приняты за ключ хоста")
	}
}

// Требование относится только к SSH: у движка своя проверка подлинности —
// сертификат TLS, и требовать от него ключ SSH означало бы сделать
// невозможным заведение обычного подключения к oVirt.
func TestEngineServerDoesNotNeedHostKey(t *testing.T) {
	srv := &Server{
		Name: "движок", Kind: KindOVirt, Username: "admin@internal",
		EngineURL: "https://engine.example.org",
	}

	if err := srv.Validate(); err != nil {
		t.Fatalf("подключение к движку потребовало ключ SSH: %v", err)
	}
}
