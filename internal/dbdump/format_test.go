package dbdump

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

func localBackend(t *testing.T) (repo.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	backend, err := repo.Open(context.Background(), &model.StorageTarget{
		ID: "local", Name: "local", Kind: model.StorageLocal, BasePath: dir, Enabled: true,
	})
	if err != nil {
		t.Fatalf("локальное хранилище: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend, dir
}

func testCipher(t *testing.T) *secret.Cipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// dumpLike даёт поток, похожий на дамп: сжимаемый текст и несжимаемый хвост,
// с длиной не кратной чанку.
func dumpLike(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	for buf.Len() < 3<<20 {
		buf.WriteString("INSERT INTO orders VALUES (42, 'пример строки', now());\n")
	}
	tail := make([]byte, 700_001)
	if _, err := rand.Read(tail); err != nil {
		t.Fatal(err)
	}
	buf.Write(tail)
	return buf.Bytes()
}

func TestStreamRoundTripWithEncryption(t *testing.T) {
	ctx := context.Background()
	backend, _ := localBackend(t)
	cipher := testCipher(t)
	w := &streamWriter{backend: backend, cipher: cipher, chunkSize: 1 << 20, compression: "zstd", level: 3}

	want := dumpLike(t)
	prefix := RunPrefix("db-01", model.DBEnginePostgreSQL, time.Now(), "run-1")
	data, err := w.write(ctx, DataKey(prefix, 0, "billing"), "run-1", 0, "billing", bytes.NewReader(want))
	if err != nil {
		t.Fatalf("запись дампа: %v", err)
	}
	if !data.Encrypted || data.VirtualSize != int64(len(want)) || data.LogicalBytes != int64(len(want)) {
		t.Fatalf("манифест дампа: encrypted=%v size=%d logical=%d, want %d",
			data.Encrypted, data.VirtualSize, data.LogicalBytes, len(want))
	}
	if data.StoredBytes >= int64(len(want)) {
		t.Fatalf("сжатие не сработало: %d байт из %d", data.StoredBytes, len(want))
	}

	var got bytes.Buffer
	if err := StreamDump(ctx, backend, cipher, data, &got); err != nil {
		t.Fatalf("чтение дампа: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("восстановленный поток отличается: %d байт против %d", got.Len(), len(want))
	}

	// Без ключа зашифрованный дамп не читается.
	if err := StreamDump(ctx, backend, nil, data, io.Discard); err == nil {
		t.Fatal("зашифрованный дамп прочитан без ключа")
	}
}

func TestStreamDetectsTamperedChunk(t *testing.T) {
	ctx := context.Background()
	backend, dir := localBackend(t)
	w := &streamWriter{backend: backend, chunkSize: 1 << 20, compression: "none"}

	want := dumpLike(t)
	key := DataKey(RunPrefix("db-01", model.DBEngineMySQL, time.Now(), "run-2"), 0, "shop")
	data, err := w.write(ctx, key, "run-2", 0, "shop", bytes.NewReader(want))
	if err != nil {
		t.Fatalf("запись дампа: %v", err)
	}

	path := filepath.Join(dir, filepath.FromSlash(key))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("объект данных: %v", err)
	}
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err = StreamDump(ctx, backend, nil, data, io.Discard)
	if err == nil {
		t.Fatal("подменённый чанк прошёл проверку")
	}
}

func TestParseProbe(t *testing.T) {
	res, err := ParseProbe("jhvirt-db-dump/1\nengine postgresql 16.4\nerror mysql Access denied for user\nrestore enabled\n")
	if err != nil {
		t.Fatalf("разбор probe: %v", err)
	}
	if len(res.Engines) != 1 || res.Engines[0].Engine != model.DBEnginePostgreSQL || res.Engines[0].Version != "16.4" {
		t.Fatalf("СУБД: %+v", res.Engines)
	}
	if !strings.Contains(res.Errors[model.DBEngineMySQL], "Access denied") || !res.RestoreEnabled {
		t.Fatalf("ошибки и восстановление: %+v %v", res.Errors, res.RestoreEnabled)
	}
	if _, err := ParseProbe("SSH-2.0-OpenSSH\n"); err == nil {
		t.Fatal("чужой ответ принят за хелпер")
	}
}

func TestKeysStayInsideRunPrefix(t *testing.T) {
	prefix := RunPrefix("БД Продакшн", model.DBEnginePostgreSQL, time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC), "abc")
	if !strings.HasPrefix(prefix, "jhvirt/db/") || strings.Contains(prefix, " ") || !strings.HasSuffix(prefix, "/2026/09/19/abc/") {
		t.Fatalf("префикс запуска: %q", prefix)
	}
	key := DataKey(prefix, 3, "../etc")
	if !strings.HasPrefix(key, prefix) || strings.Contains(strings.TrimPrefix(key, prefix), "/") {
		t.Fatalf("ключ базы выходит из каталога запуска: %q", key)
	}
}

func TestVerifyEntryCatchesDamage(t *testing.T) {
	ctx := context.Background()
	backend, dir := localBackend(t)
	cipher := testCipher(t)
	w := &streamWriter{backend: backend, cipher: cipher, chunkSize: 1 << 20, compression: "zstd", level: 3}
	key := DataKey(RunPrefix("db-01", model.DBEnginePostgreSQL, time.Now(), "run-3"), 0, "billing")
	data, err := w.write(ctx, key, "run-3", 0, "billing", bytes.NewReader(dumpLike(t)))
	if err != nil {
		t.Fatalf("запись дампа: %v", err)
	}
	entry := ManifestEntry{Database: "billing", Kind: KindDatabase, Format: "custom", Data: data}
	if err := verifyEntry(ctx, backend, cipher, entry); err != nil {
		t.Fatalf("целый дамп не прошёл проверку: %v", err)
	}

	// Манифест обещает больше, чем лежит в объекте.
	short := *data
	short.LogicalBytes++
	if err := verifyEntry(ctx, backend, cipher, ManifestEntry{Database: "billing", Data: &short}); err == nil {
		t.Fatal("расхождение длины с манифестом не замечено")
	}

	path := filepath.Join(dir, filepath.FromSlash(key))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-5] ^= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyEntry(ctx, backend, cipher, entry); err == nil {
		t.Fatal("изменённый шифртекст прошёл проверку")
	}
}
