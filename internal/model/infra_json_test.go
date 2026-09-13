package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestServerJSONOnlyReportsTrustMaterialPresence(t *testing.T) {
	raw, err := json.Marshal(Server{
		Password: "api-secret", PasswordStored: true,
		CACert:       "-----BEGIN CERTIFICATE-----\nsecret-ish trust material",
		CACertStored: true, SSHHostKey: "ssh-ed25519 AAAA...", SSHHostKeyStored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, "api-secret") || strings.Contains(body, "BEGIN CERTIFICATE") || strings.Contains(body, "ssh-ed25519") ||
		strings.Contains(body, `"ca_cert"`) || strings.Contains(body, `"ssh_host_key"`) {
		t.Fatalf("trust material leaked in JSON: %s", body)
	}
	if !strings.Contains(body, `"password_stored":true`) || !strings.Contains(body, `"ca_cert_stored":true`) || !strings.Contains(body, `"ssh_host_key_stored":true`) {
		t.Fatalf("presence flags missing from JSON: %s", body)
	}
}
