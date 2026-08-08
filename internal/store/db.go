// Package store provides persistence for the service.
//
// The schema is written on the intersection of the SQLite and PostgreSQL
// dialects, so one set of migrations and one set of repositories serve both
// engines. The only dialect-specific piece is the placeholder style, handled by
// DB.Rebind.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"adveng/jh_virt/internal/config"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Dialect identifies the SQL flavour in use.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// DB wraps *sql.DB with the dialect it was opened against.
type DB struct {
	*sql.DB
	dialect Dialect
}

// Dialect returns the flavour this handle talks.
func (db *DB) Dialect() Dialect { return db.dialect }

// Open connects to the configured database and prepares it for use.
func Open(ctx context.Context, cfg config.DatabaseConfig) (*DB, error) {
	switch cfg.Driver {
	case "sqlite":
		return openSQLite(ctx, cfg.SQLite)
	case "postgres":
		return openPostgres(ctx, cfg.Postgres)
	default:
		return nil, fmt.Errorf("unsupported database driver %q", cfg.Driver)
	}
}

func openSQLite(ctx context.Context, cfg config.SQLiteConfig) (*DB, error) {
	if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create sqlite dir: %w", err)
		}
	}
	busyMS := int(cfg.BusyTimeout / time.Millisecond)
	if busyMS <= 0 {
		busyMS = 10000
	}
	// WAL keeps the monitor's writes from blocking the API's reads; the busy
	// timeout absorbs the remaining short writer contention.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)",
		cfg.Path, busyMS)

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite tolerates exactly one writer; more connections only add contention.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &DB{DB: sqlDB, dialect: DialectSQLite}, nil
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
	return &DB{DB: sqlDB, dialect: DialectPostgres}, nil
}

// Rebind converts a query written with `?` placeholders into the dialect's
// native form. PostgreSQL wants $1, $2, ...; SQLite takes `?` as written.
func (db *DB) Rebind(query string) string {
	if db.dialect != DialectPostgres {
		return query
	}
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
		applied_at BIGINT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
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
			_, err := tx.ExecContext(ctx, q, version, name, time.Now().UnixMilli())
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
