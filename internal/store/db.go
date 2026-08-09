// Package store provides persistence for the service.
//
// СУБД одна — PostgreSQL. Раньше поддерживались две, и схема была написана на
// пересечении диалектов; от этого остались BIGINT-миллисекунды вместо
// TIMESTAMPTZ и JSON в TEXT вместо JSONB, которые постепенно переводятся на
// родные типы миграциями.
//
// Запросы пишутся с `?`, а DB.Rebind превращает их в $1, $2… Переписывать сто
// с лишним мест ради смены стиля плейсхолдеров — churn с риском опечатки и без
// выигрыша, поэтому Rebind остался.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"adveng/jh_virt/internal/config"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DB wraps *sql.DB.
type DB struct {
	*sql.DB
}

// Open connects to the configured database and prepares it for use.
func Open(ctx context.Context, cfg config.DatabaseConfig) (*DB, error) {
	return openPostgres(ctx, cfg.Postgres)
}

func openPostgres(ctx context.Context, cfg config.PostgresConfig) (*DB, error) {
	connCfg, err := pgx.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	sqlDB := stdlib.OpenDB(*connCfg)
	maxConns := int(cfg.MaxConns)
	if maxConns <= 0 {
		maxConns = 10
	}
	sqlDB.SetMaxOpenConns(maxConns)
	sqlDB.SetMaxIdleConns(maxConns / 2)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &DB{DB: sqlDB}, nil
}

// Rebind converts a query written with `?` placeholders into the dialect's
// native form: PostgreSQL wants $1, $2, ...
func (db *DB) Rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c != '?' {
			b.WriteByte(c)
			continue
		}
		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

// Exec runs a statement after rebinding placeholders.
func (db *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.ExecContext(ctx, db.Rebind(query), args...)
}

// Query runs a query after rebinding placeholders.
func (db *DB) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.QueryContext(ctx, db.Rebind(query), args...)
}

// QueryRow runs a single-row query after rebinding placeholders.
func (db *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return db.QueryRowContext(ctx, db.Rebind(query), args...)
}

// InTx runs fn inside a transaction, rolling back on error or panic.
func (db *DB) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Migrate applies every embedded migration that has not run yet. Migration
// files are named NNNN_description.sql and applied in numeric order; each file
// runs inside its own transaction.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	if err := db.upgradeMigrationsTable(ctx); err != nil {
		return err
	}

	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if applied[version] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		err = db.InTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("apply %s: %w", name, err)
			}
			q := db.Rebind(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`)
			_, err := tx.ExecContext(ctx, q, version, name, time.Now().UTC())
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func migrationVersion(name string) (int, error) {
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return 0, fmt.Errorf("migration %q must be named NNNN_description.sql", name)
	}
	v, err := strconv.Atoi(name[:idx])
	if err != nil {
		return 0, fmt.Errorf("migration %q has non-numeric version: %w", name, err)
	}
	return v, nil
}

// upgradeMigrationsTable переводит applied_at на TIMESTAMPTZ в базах, где
// таблица была создана прежней версией.
//
// Обычной миграцией это сделать нельзя: строку о применении каждой миграции
// пишет сам применятель, и до собственной очереди 0007 таблица успела бы
// принять шесть записей уже нового типа в колонку старого. Поэтому свою
// таблицу применятель приводит в порядок сам, до того как что-то в неё пишет.
func (db *DB) upgradeMigrationsTable(ctx context.Context) error {
	var dataType string
	err := db.QueryRowContext(ctx, `SELECT data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'schema_migrations'
		  AND column_name = 'applied_at'`).Scan(&dataType)
	if err != nil {
		return fmt.Errorf("тип schema_migrations.applied_at: %w", err)
	}
	if dataType != "bigint" {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE schema_migrations
		ALTER COLUMN applied_at TYPE TIMESTAMPTZ USING to_timestamp(applied_at / 1000.0)`); err != nil {
		return fmt.Errorf("перевод schema_migrations.applied_at в TIMESTAMPTZ: %w", err)
	}
	return nil
}
