package api

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestServerPayloadKeepsHiddenTrustMaterialUntilExplicitlyCleared(t *testing.T) {
	existing := &model.Server{
		Password: "password", SSHPrivateKey: "private-key",
		CACert: "ca-certificate", SSHHostKey: "ssh-ed25519 host-key",
	}

	payload := serverPayload{}
	payload.fillHiddenFrom(existing)
	if payload.Password != existing.Password || payload.SSHPrivateKey != existing.SSHPrivateKey ||
		payload.CACert != existing.CACert || payload.SSHHostKey != existing.SSHHostKey {
		t.Fatalf("empty edit did not preserve hidden connection material: %#v", payload)
	}

	payload = serverPayload{ClearCACert: true, ClearSSHHostKey: true}
	payload.fillHiddenFrom(existing)
	if payload.CACert != "" || payload.SSHHostKey != "" {
		t.Fatalf("explicit clear restored trust material: %#v", payload)
	}
}

func TestCertificateFingerprintDoesNotExposePEM(t *testing.T) {
	der, err := base64.StdEncoding.DecodeString("MIIB")
	if err != nil {
		t.Fatal(err)
	}
	bundle := "-----BEGIN CERTIFICATE-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END CERTIFICATE-----\n"
	fingerprint, err := certificateFingerprint(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fingerprint, "CERTIFICATE") || len(strings.Split(fingerprint, ":")) != 32 {
		t.Fatalf("unexpected SHA-256 fingerprint: %q", fingerprint)
	}
}

func TestValidateServerRejectsRemotePlaintextEngine(t *testing.T) {
	srv := &model.Server{
		Name: "engine", Kind: model.KindOVirt, EngineURL: "http://engine.example.org",
		Username: "service@internal", Password: "secret", Enabled: true,
	}
	if err := validateServer(srv, true); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("remote plaintext engine accepted: %v", err)
	}
}

func TestVirtualizationKindOptionsCoverEveryBuiltInDriver(t *testing.T) {
	options := virtualizationKindOptions()
	seen := make(map[string]virtualizationKindDescriptor, len(options))
	for _, option := range options {
		seen[option.Value] = option
	}
	for _, kind := range model.AllServerKinds() {
		option, ok := seen[string(kind)]
		if !ok {
			t.Errorf("тип виртуализации %q отсутствует в /meta", kind)
			continue
		}
		if option.ManagedScope != kind.ManagedScope() || option.SupportsBackup != kind.SupportsBackup() ||
			option.SupportsRestore != kind.SupportsRestore() {
			t.Errorf("неверные возможности %q: %+v", kind, option)
		}
		if option.SafeProvision != kind.UsesOVirtAPI() {
			t.Errorf("неверный признак безопасного мастера %q: %+v", kind, option)
		}
		if option.SupportsVMManagement != kind.SupportsVMManagement() ||
			option.SupportsHostManagement != kind.SupportsHostManagement() {
			t.Errorf("неверные возможности управления %q: %+v", kind, option)
		}
	}
}

func TestValidateServerRejectsUnknownVirtualizationConnector(t *testing.T) {
	srv := &model.Server{
		Name: "unknown", Kind: model.ServerKind("future-driver"), EngineURL: "https://manager.example.org",
		Username: "service", Password: "secret", Enabled: true,
	}
	if err := validateServer(srv, true); err == nil || !strings.Contains(err.Error(), "неподдерживаемый") {
		t.Fatalf("unknown connector accepted by API: %v", err)
	}
}

func TestValidateServerAcceptsProxmoxTokenAndRejectsUserPasswordShape(t *testing.T) {
	srv := &model.Server{Name: "pve", Kind: model.KindProxmox, EngineURL: "https://pve.example.org:8006",
		Username: "backup@pve!jhvirt", Password: "token-secret", Enabled: true}
	if err := validateServer(srv, true); err != nil {
		t.Fatalf("valid Proxmox token rejected: %v", err)
	}
	srv.Username = "root@pam"
	if err := validateServer(srv, true); err == nil || !strings.Contains(err.Error(), "token ID") {
		t.Fatalf("user/password-shaped Proxmox credentials accepted: %v", err)
	}
}

func TestProbeHintDNSMentionsLocalRouteDomain(t *testing.T) {
	hint := probeHint(errors.New("dial tcp: lookup engine.example.local on 127.0.0.11:53: server misbehaving"))
	for _, want := range []string{"сервере приложения", ".local", "route-domain", "TLS"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("probeHint() = %q, want substring %q", hint, want)
		}
	}
}

func TestLibvirtHintDNSMentionsContainer(t *testing.T) {
	hint := libvirtHint(errors.New("dial tcp: lookup kvm.example.local: no such host"))
	for _, want := range []string{"KVM-хоста", "внутри контейнера", ".local", "route-domain"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("libvirtHint() = %q, want substring %q", hint, want)
		}
	}
}
