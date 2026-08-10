package api

import (
	"errors"
	"strings"
	"testing"
)

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
