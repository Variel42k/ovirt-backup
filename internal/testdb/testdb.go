// Package testdb выдаёт тестам отдельную базу PostgreSQL.
//
// Отдельную — потому что `go test ./...` запускает пакеты параллельно, и все
// они ходили бы в одну базу: миграции наперегонки создавали бы одни и те же
// таблицы, а очистка перед тестом сносила бы данные из-под соседнего пакета.
// Проявляется это не как понятная ошибка, а как «duplicate key value violates
// unique constraint pg_type_typname_nsp_index» из недр PostgreSQL.
//
// Пакет намеренно не зависит от internal/store: тесты самого store должны им
// пользоваться, а это дало бы цикл импортов.
package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/Variel42k/ovirt-backup/internal/config"
)

// EnvDSN — переменная со строкой подключения к серверу PostgreSQL, на котором
// заводятся тестовые базы.
const EnvDSN = "JHV_TEST_POSTGRES_DSN"

var (
	once     sync.Once
	cachedDS string
	cachedEr error
)

// DSN возвращает строку подключения к базе, принадлежащей этому тестовому
// бинарю. База создаётся заново при первом обращении и переиспользуется
// дальше.
//
// Без EnvDSN тест падает, а не пропускается: пропуск выглядит в выводе go test
// как «ok», и набор, половина которого молча ничего не проверила, — ровно тот
// способ, которым дефект PostgreSQL дожил до боевой установки.
func DSN(t *testing.T) string {
	t.Helper()

	admin := os.Getenv(EnvDSN)
	if admin == "" {
		t.Fatalf(`тесту нужна PostgreSQL, задайте %s.

Проще всего — ./run test: он поднимает временную базу в docker сам.
Вручную:
  ./run test-db start   # напечатает строку подключения`, EnvDSN)
	}

	once.Do(func() { cachedDS, cachedEr = provision(admin) })
	if cachedEr != nil {
		t.Fatalf("подготовка тестовой базы: %v", cachedEr)
	}
	return cachedDS
}

// Config — то же самое, но сразу в виде конфигурации подключения.
func Config(t *testing.T) config.DatabaseConfig {
	t.Helper()

	cfg, err := config.DatabaseFromDSN(DSN(t))
	if err != nil {
		t.Fatalf("разбор строки подключения: %v", err)
	}
	return cfg
}

// provision создаёт базу под текущий тестовый бинарь.
func provision(admin string) (string, error) {
	name := databaseName()

	adminCfg, err := pgx.ParseConfig(admin)
	if err != nil {
		return "", fmt.Errorf("разбор %s: %w", EnvDSN, err)
	}
	db := stdlib.OpenDB(*adminCfg)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	// DROP перед CREATE, а не CREATE IF NOT EXISTS: прогон должен начинаться с
	// пустой схемы, иначе вчерашние данные будут молча влиять на сегодняшние
	// проверки.
	if _, err := db.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
		return "", fmt.Errorf("удаление прежней базы %s: %w", name, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		return "", fmt.Errorf("создание базы %s: %w", name, err)
	}

	return withDatabase(admin, name), nil
}

// databaseName делает имя из имени тестового бинаря: store.test -> ..._store.
// Так у каждого пакета своя база, а имя остаётся читаемым, если понадобится
// заглянуть внутрь после падения.
var unsafeChars = regexp.MustCompile(`[^a-z0-9_]+`)

func databaseName() string {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, ".test")
	base = unsafeChars.ReplaceAllString(strings.ToLower(base), "_")
	if base == "" {
		base = "pkg"
	}
	const maxIdent = 63 // предел длины идентификатора в PostgreSQL
	name := "jhvirt_test_" + base
	if len(name) > maxIdent {
		name = name[:maxIdent]
	}
	return name
}

// withDatabase подменяет имя базы в строке подключения, сохраняя её форму.
func withDatabase(dsn, name string) string {
	if strings.Contains(dsn, "://") {
		// В URL база — это путь после адреса.
		i := strings.Index(dsn, "://") + 3
		rest := dsn[i:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return dsn + "/" + name
		}
		tail := ""
		if q := strings.IndexByte(rest[slash:], '?'); q >= 0 {
			tail = rest[slash+q:]
		}
		return dsn[:i] + rest[:slash+1] + name + tail
	}

	fields := strings.Fields(dsn)
	replaced := false
	for i, f := range fields {
		if strings.HasPrefix(f, "dbname=") {
			fields[i] = "dbname=" + name
			replaced = true
		}
	}
	if !replaced {
		fields = append(fields, "dbname="+name)
	}
	return strings.Join(fields, " ")
}

// Truncate очищает все таблицы, кроме служебной schema_migrations: её очистка
// заставила бы миграции применяться заново к уже существующим таблицам.
func Truncate(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `SELECT tablename FROM pg_tables
		WHERE schemaname='public' AND tablename <> 'schema_migrations'`)
	if err != nil {
		t.Fatalf("список таблиц: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatalf("чтение имени таблицы: %v", err)
		}
		tables = append(tables, name)
	}
	_ = rows.Close()
	if len(tables) == 0 {
		return
	}
	if _, err := db.ExecContext(ctx,
		`TRUNCATE TABLE `+strings.Join(tables, ", ")+` RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("очистка таблиц: %v", err)
	}
}

// Extra заводит дополнительную пустую базу для тестов, которым нужна не та,
// что общая для пакета: например для проверки обновления со старой схемы,
// которая начинается с частично применённых миграций.
func Extra(t *testing.T, suffix string) string {
	t.Helper()

	admin := os.Getenv(EnvDSN)
	if admin == "" {
		t.Fatalf("тесту нужна PostgreSQL, задайте %s (проще всего — ./run test)", EnvDSN)
	}

	name := databaseName() + "_" + unsafeChars.ReplaceAllString(strings.ToLower(suffix), "_")
	if len(name) > 63 {
		name = name[:63]
	}

	adminCfg, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatalf("разбор %s: %v", EnvDSN, err)
	}
	db := stdlib.OpenDB(*adminCfg)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
		t.Fatalf("удаление прежней базы %s: %v", name, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("создание базы %s: %v", name, err)
	}
	return withDatabase(admin, name)
}
