package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStorageTargetJSONReturnsPresenceWithoutTrustMaterial(t *testing.T) {
	target := StorageTarget{
		Name: "sftp", Kind: StorageSFTP,
		SecretKey: "s3-secret", Password: "password", PrivateKey: "private-key",
		PasswordStored: true, PrivateKeyStored: true,
		HostKey: "ssh-ed25519 AAAA...", HostKeyStored: true,
	}
	body, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	value := string(body)
	for _, forbidden := range []string{"s3-secret", `"password":`, "private-key", "ssh-ed25519", `"host_key":`} {
		if strings.Contains(value, forbidden) {
			t.Fatalf("storage response exposed write-only material %q: %s", forbidden, value)
		}
	}
	if !strings.Contains(value, `"password_stored":true`) ||
		!strings.Contains(value, `"private_key_stored":true`) || !strings.Contains(value, `"host_key_stored":true`) {
		t.Fatalf("storage response omitted presence flags: %s", value)
	}
}
