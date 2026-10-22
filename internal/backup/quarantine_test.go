package backup

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
)

func TestReadOnlyCopyCannotBeQuarantined(t *testing.T) {
	for _, replica := range []bool{false, true} {
		t.Run(fmtReadOnlyCase(replica), func(t *testing.T) {
			ctx := context.Background()
			st, engine, run, backend := quarantineFixture(t, 72*time.Hour)
			target, err := st.GetStorageTarget(ctx, run.StorageTargetID)
			if err != nil {
				t.Fatal(err)
			}
			if replica {
				target.ID, target.Name = "replica", "read-only replica"
				if err := st.CreateStorageTarget(ctx, target); err != nil {
					t.Fatal(err)
				}
				if err := st.CreateBackupCopy(ctx, &model.BackupCopy{RunID: run.ID, StorageTargetID: target.ID, Role: model.CopyReplica, Status: model.CopySucceeded, RepoPath: run.RepoPath}); err != nil {
					t.Fatal(err)
				}
			}
			target.ReadOnly = true
			if err := st.UpdateStorageTarget(ctx, target); err != nil {
				t.Fatal(err)
			}
			if err := engine.DeleteRunData(ctx, run.ID, DeleteReasonManual); !errors.Is(err, repo.ErrReadOnly) {
				t.Fatalf("read-only deletion: %v", err)
			}
			stored, err := st.GetBackupRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Deleted || stored.PurgeAfter != nil || !dataPresent(t, backend, run.RepoPath+"disk.data") {
				t.Fatal("read-only copy or its catalog was mutated")
			}
		})
	}
}

func fmtReadOnlyCase(replica bool) string {
	if replica {
		return "replica"
	}
	return "primary"
}

func TestRetentionPreviewReadOnlyAndChangedQuarantine(t *testing.T) {
	ctx := context.Background()
	st, engine, run, _ := quarantineFixture(t, 72*time.Hour)
	policy := model.RetentionPolicy{KeepLast: 1}
	plan, err := engine.ApplyRetention(ctx, run.ServerID, run.VMID, run.StorageTargetID, policy, true)
	if err != nil {
		t.Fatal(err)
	}
	engine.cfg.PurgeDelay = 24 * time.Hour
	if _, err := engine.ApplyRetentionConfirmed(ctx, run.ServerID, run.VMID, run.StorageTargetID, policy, plan.Token); !errors.Is(err, ErrRetentionPlanChanged) {
		t.Fatalf("changed quarantine accepted the old preview: %v", err)
	}
	target, err := st.GetStorageTarget(ctx, run.StorageTargetID)
	if err != nil {
		t.Fatal(err)
	}
	target.ReadOnly = true
	if err := st.UpdateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	plan, err = engine.ApplyRetention(ctx, run.ServerID, run.VMID, run.StorageTargetID, policy, true)
	if err != nil || len(plan.Warnings) == 0 {
		t.Fatalf("read-only preview: %+v %v", plan, err)
	}
	if _, err := engine.ApplyRetentionConfirmed(ctx, run.ServerID, run.VMID, run.StorageTargetID, policy, plan.Token); !errors.Is(err, repo.ErrReadOnly) {
		t.Fatalf("retention mutated a read-only target: %v", err)
	}
}

func TestBlockedPurgeAlertsAndResolvesAfterRetry(t *testing.T) {
	for _, restore := range []bool{false, true} {
		name := "retry"
		if restore {
			name = "restore"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st, engine, run, backend := quarantineFixture(t, 72*time.Hour)
			if err := st.QuarantineRun(ctx, run.ID, time.Now().Add(-time.Hour), "test"); err != nil {
				t.Fatal(err)
			}
			target, err := st.GetStorageTarget(ctx, run.StorageTargetID)
			if err != nil {
				t.Fatal(err)
			}
			target.ReadOnly = true
			if err := st.UpdateStorageTarget(ctx, target); err != nil {
				t.Fatal(err)
			}
			engine.PurgeExpired(ctx)
			engine.PurgeExpired(ctx)
			alerts, err := st.ListAlerts(ctx, store.AlertFilter{Scope: model.ScopeBackup})
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 1 || alerts[0].Kind != model.AlertBackupPurgeBlocked || alerts[0].Count != 2 || alerts[0].State != model.AlertFiring {
				t.Fatalf("blocked purge should be deduplicated and visible: %+v", alerts)
			}
			if !dataPresent(t, backend, run.RepoPath+"disk.data") {
				t.Fatal("blocked purge erased data")
			}
			if restore {
				if err := st.RestoreRunFromQuarantine(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				target.ReadOnly = false
				if err := st.UpdateStorageTarget(ctx, target); err != nil {
					t.Fatal(err)
				}
				engine.PurgeExpired(ctx)
				if dataPresent(t, backend, run.RepoPath+"disk.data") {
					t.Fatal("retry did not erase data")
				}
			}
			alerts, err = st.ListAlerts(ctx, store.AlertFilter{Scope: model.ScopeBackup})
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 1 || alerts[0].State != model.AlertResolved {
				t.Fatalf("alert was not resolved: %+v", alerts)
			}
		})
	}
}

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

	if err := engine.DeleteRunData(ctx, run.ID, DeleteReasonManual+": tester"); err != nil {
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
	if stored.DeleteReason != DeleteReasonManual+": tester" || stored.DeletedAt == nil {
		t.Fatalf("у копии в карантине нет причины и времени удаления: %q, %v", stored.DeleteReason, stored.DeletedAt)
	}
	if !dataPresent(t, backend, run.RepoPath+"disk.data") {
		t.Fatal("данные стёрты, хотя карантин ещё не истёк")
	}
}

// Пока срок не вышел, копию можно вернуть целиком.
func TestRestoreFromQuarantine(t *testing.T) {
	ctx := context.Background()
	st, engine, run, backend := quarantineFixture(t, 72*time.Hour)

	if err := engine.DeleteRunData(ctx, run.ID, DeleteReasonManual+": tester"); err != nil {
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

	if err := engine.DeleteRunData(ctx, run.ID, DeleteReasonManual+": tester"); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	// Сдвигаем срок в прошлое вместо ожидания трёх суток.
	if err := st.QuarantineRun(ctx, run.ID, time.Now().UTC().Add(-time.Minute), "тест"); err != nil {
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

	if err := engine.DeleteRunData(ctx, run.ID, DeleteReasonManual+": tester"); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if err := st.QuarantineRun(ctx, run.ID, time.Now().UTC().Add(-time.Minute), "тест"); err != nil {
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

	if err := engine.DeleteRunData(ctx, run.ID, DeleteReasonManual+": tester"); err != nil {
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

	if err := engine.PurgeRunData(ctx, run.ID, DeleteReasonErased+": tester"); err != nil {
		t.Fatalf("немедленное стирание: %v", err)
	}
	if dataPresent(t, backend, run.RepoPath+"disk.data") {
		t.Fatal("данные целы после запроса на немедленное стирание")
	}
}

// Стирание копии, которая уже лежит в карантине, снимает срок: иначе она так и
// числилась бы в карантине, хотя возвращать нечего. Прежняя причина остаётся,
// досрочное стирание дописывается к ней.
func TestPurgeResetsQuarantine(t *testing.T) {
	ctx := context.Background()
	st, engine, run, backend := quarantineFixture(t, 72*time.Hour)

	if err := engine.DeleteRunData(ctx, run.ID, retentionDeleteReason("сверх 7 копий")); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if err := engine.PurgeRunData(ctx, run.ID, DeleteReasonErased+": tester"); err != nil {
		t.Fatalf("стирание из карантина: %v", err)
	}

	stored, err := st.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("чтение копии: %v", err)
	}
	if !stored.Deleted || stored.PurgeAfter != nil {
		t.Fatalf("после стирания копия должна быть удалена без срока: deleted=%v purge_after=%v",
			stored.Deleted, stored.PurgeAfter)
	}
	if want := "политика хранения: сверх 7 копий; " + DeleteReasonErased + ": tester"; stored.DeleteReason != want {
		t.Fatalf("причина %q — ждали %q", stored.DeleteReason, want)
	}
	if dataPresent(t, backend, run.RepoPath+"disk.data") {
		t.Fatal("данные целы после стирания из карантина")
	}
	if err := st.RestoreRunFromQuarantine(ctx, run.ID); err == nil {
		t.Fatal("стёртую копию удалось вернуть из карантина")
	}
}

// Возврат из карантина стирает причину удаления: копия снова живая.
func TestRestoreFromQuarantineClearsReason(t *testing.T) {
	ctx := context.Background()
	st, engine, run, _ := quarantineFixture(t, 72*time.Hour)

	if err := engine.DeleteRunData(ctx, run.ID, DeleteReasonManual+": tester"); err != nil {
		t.Fatalf("удаление: %v", err)
	}
	if err := st.RestoreRunFromQuarantine(ctx, run.ID); err != nil {
		t.Fatalf("возврат: %v", err)
	}
	stored, err := st.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("чтение копии: %v", err)
	}
	if stored.Deleted || stored.DeleteReason != "" || stored.DeletedAt != nil {
		t.Fatalf("у возвращённой копии остались следы удаления: %+v", stored)
	}
}
