// Package storetest даёт тестам готовое хранилище поверх настоящей PostgreSQL.
//
// Отдельный пакет, а не помощник внутри store: база нужна тестам api и
// monitor тоже, а собственная копия подготовки в каждом из них — несколько
// реализаций одного и того же, которые со временем разъезжаются.
//
// Сам store им пользоваться не может (получился бы цикл импортов) и берёт
// базу напрямую из internal/testdb.
package storetest

import (
	"context"
	"path/filepath"
	"testing"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/secret"
	"adveng/jh_virt/internal/store"
	"adveng/jh_virt/internal/testdb"
)

// New открывает базу этого тестового бинаря, применяет схему, очищает таблицы
// и возвращает готовое хранилище.
func New(t *testing.T) *store.Store {
	t.Helper()

	ctx := context.Background()
	db, err := store.Open(ctx, testdb.Config(t))
	if err != nil {
		t.Fatalf("открытие базы: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("миграции: %v", err)
	}
	testdb.Truncate(t, db.DB)

	cipher, err := secret.NewFromConfig(config.SecretsConfig{
		KeyFile: filepath.Join(t.TempDir(), "key"),
	})
	if err != nil {
		t.Fatalf("ключ шифрования: %v", err)
	}
	return store.New(db, cipher)
}
