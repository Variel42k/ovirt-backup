package main

import (
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Адрес хранилища для CLI набирают руками, в аварии. Разбор должен быть
// предсказуемым, а отказ — называть ожидаемый формат.
func TestParseSMBSpec(t *testing.T) {
	cases := []struct {
		spec string
		want model.StorageTarget
	}{
		{"smb://svc:pass@nas.example.org/backups",
			model.StorageTarget{Host: "nas.example.org", Share: "backups", Username: "svc", Password: "pass"}},
		{"smb://svc:pass@nas.example.org:4450/backups/jhvirt/копии",
			model.StorageTarget{Host: "nas.example.org", Port: 4450, Share: "backups",
				Username: "svc", Password: "pass", BasePath: "jhvirt/копии"}},
		{"smb://EXAMPLE;svc:pass@10.0.0.5/backups",
			model.StorageTarget{Host: "10.0.0.5", Share: "backups", Domain: "EXAMPLE",
				Username: "svc", Password: "pass"}},
		// Пароль с собакой действует только закодированным — иначе адрес
		// разобрался бы по другой границе.
		{"smb://svc:p%40ss@nas/backups",
			model.StorageTarget{Host: "nas", Share: "backups", Username: "svc", Password: "p@ss"}},
	}

	for _, c := range cases {
		got, err := parseSMB(c.spec)
		if err != nil {
			t.Errorf("%s: %v", c.spec, err)
			continue
		}
		if got.Kind != model.StorageSMB {
			t.Errorf("%s: тип %q", c.spec, got.Kind)
		}
		if got.Host != c.want.Host || got.Port != c.want.Port || got.Share != c.want.Share ||
			got.Domain != c.want.Domain || got.Username != c.want.Username ||
			got.Password != c.want.Password || got.BasePath != c.want.BasePath {
			t.Errorf("%s разобран как host=%q port=%d share=%q domain=%q user=%q pass=%q path=%q",
				c.spec, got.Host, got.Port, got.Share, got.Domain, got.Username, got.Password, got.BasePath)
		}
	}

	for _, bad := range []string{"smb://nas.example.org/backups", "smb://svc:pass@nas.example.org", "smb://"} {
		if _, err := parseSMB(bad); err == nil {
			t.Errorf("неполный адрес %q приняли", bad)
		}
	}
}

func TestParseWebDAVSpec(t *testing.T) {
	got, err := parseWebDAV("webdavs://backup:pass@cloud.example.org/remote.php/dav/files/backup")
	if err != nil {
		t.Fatalf("разбор адреса: %v", err)
	}
	if got.Kind != model.StorageWebDAV {
		t.Errorf("тип %q", got.Kind)
	}
	if got.Endpoint != "https://cloud.example.org/remote.php/dav/files/backup" {
		t.Errorf("адрес коллекции %q", got.Endpoint)
	}
	if got.Username != "backup" || got.Password != "pass" {
		t.Errorf("учётные данные разобраны неверно: %q / %q", got.Username, got.Password)
	}

	// Схема без TLS должна быть явной: молча понижать защиту нельзя.
	plain, err := parseWebDAV("webdav://backup:pass@nas.local/dav")
	if err != nil {
		t.Fatalf("разбор адреса без TLS: %v", err)
	}
	if !strings.HasPrefix(plain.Endpoint, "http://") {
		t.Errorf("webdav:// дал %q", plain.Endpoint)
	}

	if _, err := parseWebDAV("webdavs://"); err == nil {
		t.Error("адрес без сервера приняли")
	}
}
