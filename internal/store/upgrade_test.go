package store

import (
	"context"
	"testing"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/testdb"
)

// Обновление существующей установки — путь, который обычные тесты не
// проверяют вовсе: они всегда начинают с пустой базы, где миграция создаёт
// правильные типы и конвертировать нечего. Здесь база собирается такой, какой
// она была до перехода на TIMESTAMPTZ, наполняется данными в миллисекундах, и
// только потом запускается настоящий Migrate.
func TestMigrateConvertsExistingMilliseconds(t *testing.T) {
	ctx := context.Background()
	dsn := testdb.Extra(t, "upgrade")

	cfg, err := config.DatabaseFromDSN(dsn)
	if err != nil {
		t.Fatalf("строка подключения: %v", err)
	}
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("открытие базы: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	buildLegacySchema(t, db)

	// 1754472345123 мс = 2025-08-06 09:25:45.123 UTC
	const legacyMillis = 1754472345123
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, password_hash, role, disabled, last_login_at, created_at, updated_at)
		VALUES ('u1','admin','x','admin',false,$1,$1,$1),
		       ('u2','viewer','x','viewer',false,NULL,$2,$2)`,
		legacyMillis, 1700000000000); err != nil {
		t.Fatalf("наполнение старой базы: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("обновление существующей базы: %v", err)
	}

	var got string
	if err := db.QueryRow(ctx,
		`SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.MS') FROM users WHERE id=?`,
		"u1").Scan(&got); err != nil {
		t.Fatalf("чтение времени: %v", err)
	}
	if want := "2025-08-06 09:25:45.123"; got != want {
		t.Fatalf("время после конвертации = %q, ожидалось %q; потеряны миллисекунды "+
			"или деление выполнено целочисленно", got, want)
	}

	// NULL должен остаться NULL: пользователь, ни разу не входивший, не должен
	// получить дату 1970 года.
	var nulls int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE id=? AND last_login_at IS NULL`, "u2").Scan(&nulls); err != nil {
		t.Fatalf("проверка NULL: %v", err)
	}
	if nulls != 1 {
		t.Fatal("NULL не пережил конвертацию")
	}

	// Служебная таблица применятеля тоже должна была смениться, иначе следующая
	// запись о миграции упадёт на несовпадении типов.
	var appliedType string
	if err := db.QueryRow(ctx, `SELECT data_type FROM information_schema.columns
		WHERE table_name='schema_migrations' AND column_name='applied_at'`).Scan(&appliedType); err != nil {
		t.Fatalf("тип applied_at: %v", err)
	}
	if appliedType != "timestamp with time zone" {
		t.Fatalf("schema_migrations.applied_at остался %s", appliedType)
	}

	// Повторный запуск ничего не должен сломать.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("повторное обновление: %v", err)
	}
}

// buildLegacySchema воспроизводит базу такой, какой её оставляли миграции до
// 0007: служебная таблица с BIGINT и применённые 0001..0006.
func buildLegacySchema(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at BIGINT NOT NULL
	)`); err != nil {
		t.Fatalf("служебная таблица: %v", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("список миграций: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		version, err := migrationVersion(name)
		if err != nil || version >= 7 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("чтение %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("применение %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES ($1,$2,$3)`,
			version, name, 1754472345123); err != nil {
			t.Fatalf("отметка %s: %v", name, err)
		}
	}
}
