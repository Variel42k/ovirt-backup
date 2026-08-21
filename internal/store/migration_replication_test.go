package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/testdb"
)

func TestReplicationMigrationBackfillsLegacyRuns(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.DatabaseFromDSN(testdb.Extra(t, "replication_migration"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for version := 1; version <= 13; version++ {
		name := migrationNameForVersion(t, version)
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO servers
		(id,name,engine_url,username,created_at,updated_at)
		VALUES ('server','legacy','https://engine.example','user',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO backup_jobs
		(id,name,server_id,type,created_at,updated_at)
		VALUES ('legacy-job','legacy','server','full',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO backup_runs
		(id, server_id, vm_id, type, status, storage_target_id, repo_path,
		 disk_count, stored_bytes, started_at, ended_at, created_at)
		VALUES ('legacy-run','server','vm','full','succeeded','storage',
		 'jhvirt/server/vm/legacy-run/',2,1234,$1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	body, err := migrationFS.ReadFile("migrations/0014_backup_replication.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(body)); err != nil {
		t.Fatal(err)
	}

	var role, status, path string
	var objects int
	var total int64
	if err := db.QueryRowContext(ctx, `SELECT role,status,repo_path,object_count,total_bytes
		FROM backup_copies WHERE run_id='legacy-run'`).Scan(&role, &status, &path, &objects, &total); err != nil {
		t.Fatal(err)
	}
	if role != "primary" || status != "succeeded" || path != "jhvirt/server/vm/legacy-run/" || objects != 5 || total != 1234 {
		t.Fatalf("unexpected backfill: role=%s status=%s path=%s objects=%d total=%d", role, status, path, objects, total)
	}
	var replicationEnabled, forceFull bool
	if err := db.QueryRowContext(ctx, `SELECT replication_enabled,force_full_next
		FROM backup_jobs WHERE id='legacy-job'`).Scan(&replicationEnabled, &forceFull); err != nil {
		t.Fatal(err)
	}
	if replicationEnabled || forceFull {
		t.Fatalf("legacy job unexpectedly migrated to replication: enabled=%v force_full=%v", replicationEnabled, forceFull)
	}
}

func migrationNameForVersion(t *testing.T, version int) string {
	t.Helper()
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("%04d_", version)
	for _, entry := range entries {
		if len(entry.Name()) >= len(prefix) && entry.Name()[:len(prefix)] == prefix {
			return entry.Name()
		}
	}
	t.Fatalf("migration %d not found", version)
	return ""
}
