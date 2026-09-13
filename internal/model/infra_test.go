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

func TestServerKindFamiliesAreExplicit(t *testing.T) {
	for _, kind := range []ServerKind{KindOVirt, KindRedVirt, KindOLVM, KindRHV} {
		if !kind.Valid() || !kind.UsesOVirtAPI() || kind.UsesLibvirt() || kind.ManagedScope() != "engine" {
			t.Errorf("oVirt-compatible kind classified incorrectly: %q", kind)
		}
	}
	if !KindKVM.Valid() || KindKVM.UsesOVirtAPI() || !KindKVM.UsesLibvirt() || KindKVM.ManagedScope() != "host" {
		t.Fatalf("libvirt kind classified incorrectly: %q", KindKVM)
	}
	if !KindProxmox.Valid() || !KindProxmox.UsesProxmoxAPI() || KindProxmox.UsesOVirtAPI() ||
		KindProxmox.UsesLibvirt() || KindProxmox.ManagedScope() != "engine" {
		t.Fatalf("Proxmox kind classified incorrectly: %q", KindProxmox)
	}
	if !KindProxmox.SupportsBackup() || !KindProxmox.SupportsRestore() ||
		KindProxmox.SupportsEngineConfig() || !KindProxmox.SupportsVMManagement() ||
		KindProxmox.SupportsHostManagement() {
		t.Fatal("Proxmox capability flags do not match the implemented driver")
	}
	if ServerKind("future-driver").Valid() {
		t.Fatal("unknown connector silently accepted")
	}
}

func TestServerRejectsUnknownVirtualizationConnector(t *testing.T) {
	srv := &Server{
		Name: "unknown", Kind: ServerKind("future-driver"), Username: "user",
		EngineURL: "https://manager.example.org",
	}
	if err := srv.Validate(); err == nil || !strings.Contains(err.Error(), "неподдерживаемый") {
		t.Fatalf("unknown connector accepted: %v", err)
	}
}

func TestProxmoxAllowsManagementOnlyAndRequiresCompleteDataPlane(t *testing.T) {
	srv := &Server{Name: "pve", Kind: KindProxmox, EngineURL: "https://pve.example.org:8006",
		Username: "backup@pve!jhvirt", Password: "token-secret"}
	if err := srv.Validate(); err != nil {
		t.Fatalf("management-only подключение отклонено: %v", err)
	}
	if srv.HasProxmoxDataPlane() {
		t.Fatal("неполный канал данных помечен готовым")
	}
	srv.SSHUsername = "root"
	if err := srv.Validate(); err == nil {
		t.Fatal("частично заполненный канал данных принят")
	}
	srv.SSHPrivateKey = "private-key"
	srv.SSHHostKey = validHostKey
	if err := srv.Validate(); err != nil {
		t.Fatalf("полный канал данных отклонён: %v", err)
	}
	if !srv.HasProxmoxDataPlane() {
		t.Fatal("полный канал данных не распознан")
	}
}

func TestServerRejectsInvalidSSHPort(t *testing.T) {
	for _, port := range []int{-1, 65536} {
		srv := kvmServer()
		srv.SSHHostKey = validHostKey
		srv.SSHPort = port
		if err := srv.Validate(); err == nil {
			t.Errorf("invalid SSH port %d accepted", port)
		}
	}
}
