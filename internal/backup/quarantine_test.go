package backup

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
)

// quarantineFixture готовит хранилище с одной копией и лежащим в нём объектом.
func quarantineFixture(t *testing.T, purgeDelay time.Duration) (*store.Store, *Engine, *model.BackupRun, repo.Backend) {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)

	target := &model.StorageTarget{
		ID: "target", Name: "склад", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true,
	}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatalf("хранилище: %v", err)
	}

	prefix := "jhvirt/server/vm/run/"
	run := &model.BackupRun{
		ID: "run", ServerID: "server", VMID: "vm", VMName: "vm",
		Type: model.BackupFull, Status: model.RunSucceeded, ChainID: "run",
		StorageTargetID: target.ID, RepoPath: prefix, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatalf("копия: %v", err)
	}
	// Первичную физическую копию заводит сам CreateBackupRun — создавать её
	// здесь ещё раз значит получить конфликт уникальности.
	copies, err := st.ListBackupCopies(ctx, run.ID)
	if err != nil || len(copies) == 0 {
		t.Fatalf("первичная копия не создана вместе с запуском: %v", err)
	}
	copies[0].Status = model.CopySucceeded
	if err := st.UpdateBackupCopy(ctx, copies[0]); err != nil {
		t.Fatalf("состояние копии: %v", err)
	}

	backend, openErr := repo.Open(ctx, target)
	if openErr != nil {
		t.Fatalf("открытие хранилища: %v", openErr)
	}
	t.Cleanup(func() { _ = backend.Close() })

	payload := []byte("данные копии")
	if _, err := backend.Put(ctx, prefix+"disk.data", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("запись объекта: %v", err)
	}

	engine := NewEngine(st, nil, config.BackupConfig{HeavyWorkers: 1, PurgeDelay: purgeDelay},
		nil, zerolog.Nop())
	return st, engine, run, backend
}

// dataPresent сообщает, лежат ли ещё объекты копии в хранилище.
func dataPresent(t *testing.T, backend repo.Backend, key string) bool {
	t.Helper()
	_, err := backend.Stat(context.Background(), key)
	return err == nil
}

// Удаление при включённом карантине не трогает данные: копия помечается, но
// объекты остаются на месте до срока. Ради этого промежутка карантин и нужен —
// удаление копий делают перед тем, как зашифровать инфраструктуру.
func TestDeleteQuarantinesInsteadOfErasing(t *testing.T) {
	ctx := context.Background()
	st, engine, run, backend := quarantineFixture(t, 72*time.Hour)

	if err := engine.DeleteRunData(ctx, run.ID); err != nil {
		t.Fatalf("удаление: %v", err)
	}

	stored, err := st.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("чтение копии: %v", err)
	}
	if !stored.Deleted {
		t.Error("копия не помечена удалённой")
	}
	if stored.PurgeAfter == nil {
		t.Fatal("срок карантина не назначен")
	}
	if !dataPresent(t, backend, run.RepoPath+"disk.data") {
		t.Fatal("данные стёрты, хотя карантин ещё не истёк")
	}
}

// Пока срок не вышел, копию можно вернуть целиком.
func TestRestoreFromQuarantine(t *testing.T) {
	ctx := context.Background()
	st, engine, run, backend := quarantineFixture(t, 72*time.Hour)

	if err := engine.DeleteRunData(ctx, run.ID); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if err := st.RestoreRunFromQuarantine(ctx, run.ID); err != nil {
		t.Fatalf("возврат: %v", err)
	}

	stored, err := st.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("чтение копии: %v", err)
	}
	if stored.Deleted {
		t.Error("копия осталась помеченной удалённой")
	}
	if stored.PurgeAfter != nil {
		t.Error("срок карантина не снят")
	}
	if !dataPresent(t, backend, run.RepoPath+"disk.data") {
		t.Fatal("данные потеряны при возврате из карантина")
	}
}

// По истечении срока сборщик стирает данные.
func TestPurgeExpiredErasesData(t *testing.T) {
	ctx := context.Background()
	st, engine, run, backend := quarantineFixture(t, 72*time.Hour)

	if err := engine.DeleteRunData(ctx, run.ID); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	// Сдвигаем срок в прошлое вместо ожидания трёх суток.
	if err := st.QuarantineRun(ctx, run.ID, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("сдвиг срока: %v", err)
	}

	engine.PurgeExpired(ctx)

	if dataPresent(t, backend, run.RepoPath+"disk.data") {
		t.Fatal("данные не стёрты после истечения карантина")
	}
	stored, err := st.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("чтение копии: %v", err)
	}
	if stored.PurgeAfter != nil {
		t.Error("срок карантина остался после стирания")
	}
	if !stored.Deleted {
		t.Error("копия перестала считаться удалённой")
	}
}

// Возврат невозможен после того, как данные стёрты: возвращать нечего.
func TestRestoreAfterPurgeRefused(t *testing.T) {
	ctx := context.Background()
	st, engine, run, _ := quarantineFixture(t, 72*time.Hour)

	if err := engine.DeleteRunData(ctx, run.ID); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if err := st.QuarantineRun(ctx, run.ID, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("сдвиг срока: %v", err)
	}
	engine.PurgeExpired(ctx)

	if err := st.RestoreRunFromQuarantine(ctx, run.ID); err == nil {
		t.Fatal("копия «возвращена» после того, как данные стёрты")
	}
}

// Нулевой срок отключает карантин: удаление снова немедленное. Установки,
// которым карантин не нужен, не должны получать его молча.
func TestZeroDelayErasesImmediately(t *testing.T) {
	ctx := context.Background()
	st, engine, run, backend := quarantineFixture(t, 0)

	if err := engine.DeleteRunData(ctx, run.ID); err != nil {
		t.Fatalf("удаление: %v", err)
	}

	if dataPresent(t, backend, run.RepoPath+"disk.data") {
		t.Fatal("данные целы, хотя карантин выключен")
	}
	stored, err := st.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("чтение копии: %v", err)
	}
	if stored.PurgeAfter != nil {
		t.Error("назначен срок карантина при выключенном карантине")
	}
}

// Немедленное стирание доступно и при включённом карантине: место иногда нужно
// прямо сейчас.
func TestPurgeBypassesQuarantine(t *testing.T) {
	ctx := context.Background()
	_, engine, run, backend := quarantineFixture(t, 72*time.Hour)

	if err := engine.PurgeRunData(ctx, run.ID); err != nil {
		t.Fatalf("немедленное стирание: %v", err)
	}
	if dataPresent(t, backend, run.RepoPath+"disk.data") {
		t.Fatal("данные целы после запроса на немедленное стирание")
	}
}
