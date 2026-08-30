package store

import (
	"context"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/testdb"
)

// Обновление службы не должно ночью остановить резервное копирование.
//
// До этой миграции пустой ключ хоста означал «подключаться без проверки», и
// такие подключения работали. Требование ключа задним числом сломало бы их все
// разом — поэтому миграция переводит их в явное «без проверки»: поведение то
// же, но состояние теперь записано, видно оператору и снимается по одному.
//
// Проверка на пустой базе этого не покажет: там просто нет старых записей.
func TestHostKeyMigrationPreservesWorkingConnections(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.DatabaseFromDSN(testdb.Extra(t, "hostkey_migration"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const target = 36
	for version := 1; version < target; version++ {
		name := migrationNameForVersion(t, version)
		body, readErr := migrationFS.ReadFile("migrations/" + name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, execErr := db.ExecContext(ctx, string(body)); execErr != nil {
			t.Fatalf("apply %s: %v", name, execErr)
		}
	}

	now := time.Now().UTC()
	const pinned = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ8xVQ0nMPUNbwvUuPy4LtRsJvVKLxkPHtoT1P9QzUmB"

	// Три записи: гипервизор без ключа, гипервизор с ключом и подключение к
	// движку, у которого SSH нет вовсе.
	for _, row := range []struct{ id, kind, hostKey string }{
		{"kvm-bare", "kvm", ""},
		{"kvm-pinned", "kvm", pinned},
		{"engine", "ovirt", ""},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO servers
			(id,name,kind,engine_url,username,ssh_host_key,created_at,updated_at)
			VALUES ($1,$1,$2,'https://engine.example','user',$3,$4,$4)`,
			row.id, row.kind, row.hostKey, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ id, kind, hostKey string }{
		{"sftp-bare", "sftp", ""},
		{"sftp-pinned", "sftp", pinned},
		{"local", "local", ""},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO storage_targets
			(id,name,kind,enabled,host_key,created_at,updated_at)
			VALUES ($1,$1,$2,TRUE,$3,$4,$4)`,
			row.id, row.kind, row.hostKey, now); err != nil {
			t.Fatal(err)
		}
	}

	body, err := migrationFS.ReadFile("migrations/" + migrationNameForVersion(t, target))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply migration %d: %v", target, err)
	}

	for _, want := range []struct {
		table, column, id string
		trust             bool
		why               string
	}{
		{"servers", "ssh_trust_any_host_key", "kvm-bare", true,
			"работавшее подключение без ключа должно остаться рабочим, но помеченным"},
		{"servers", "ssh_trust_any_host_key", "kvm-pinned", false,
			"подключение с ключом должно остаться проверяемым"},
		{"servers", "ssh_trust_any_host_key", "engine", false,
			"у подключения к движку своя проверка подлинности, ключ SSH ему не нужен"},
		{"storage_targets", "trust_any_host_key", "sftp-bare", true,
			"работавшее хранилище без ключа должно остаться рабочим, но помеченным"},
		{"storage_targets", "trust_any_host_key", "sftp-pinned", false,
			"хранилище с ключом должно остаться проверяемым"},
		{"storage_targets", "trust_any_host_key", "local", false,
			"у локального хранилища нет SSH"},
	} {
		var got bool
		query := "SELECT " + want.column + " FROM " + want.table + " WHERE id=$1"
		if err := db.QueryRowContext(ctx, query, want.id).Scan(&got); err != nil {
			t.Fatalf("%s.%s: %v", want.table, want.id, err)
		}
		if got != want.trust {
			t.Errorf("%s.%s: %s = %v, ожидалось %v (%s)",
				want.table, want.id, want.column, got, want.trust, want.why)
		}
	}
}
