package store

import (
	"context"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/testdb"
)

func TestSessionProtectionMigrationInvalidatesPlaintextSessions(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.DatabaseFromDSN(testdb.Extra(t, "session_upgrade"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		version, err := migrationVersion(entry.Name())
		if err != nil || version >= 38 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := db.Exec(ctx,
			`INSERT INTO schema_migrations (version,name,applied_at) VALUES (?,?,?)`,
			version, entry.Name(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO users
		(id,username,password_hash,role,disabled,created_at,updated_at,provider,external_id)
		VALUES ('legacy-user','legacy','hash','admin',false,now(),now(),'local','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions
		(token,user_id,user_agent,remote_ip,expires_at,created_at,oidc_id_token)
		VALUES ('plaintext-cookie','legacy-user','','',now()+interval '1 hour',now(),'plaintext-id-token')`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade to protected sessions: %v", err)
	}
	var sessions int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("legacy plaintext sessions survived migration: %d", sessions)
	}
	var newColumn, oldColumn int
	if err := db.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE column_name='token_hash'),
		count(*) FILTER (WHERE column_name='token')
		FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='sessions'`).
		Scan(&newColumn, &oldColumn); err != nil {
		t.Fatal(err)
	}
	if newColumn != 1 || oldColumn != 0 {
		t.Fatalf("session columns after migration: token_hash=%d token=%d", newColumn, oldColumn)
	}
}
