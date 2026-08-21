package monitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/secret"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"github.com/Variel42k/ovirt-backup/internal/testdb"
)

func testRemediator(t *testing.T) (*Remediator, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()

	db, err := store.Open(context.Background(), testdb.Config(t))
	if err != nil {
		t.Fatalf("открытие базы: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("миграции: %v", err)
	}
	testdb.Truncate(t, db.DB)
	cipher, err := secret.NewFromConfig(config.SecretsConfig{KeyFile: filepath.Join(dir, "key")})
	if err != nil {
		t.Fatalf("ключ: %v", err)
	}
	st := store.New(db, cipher)

	archives := filepath.Join(dir, "archives")
	cfg := config.RemediationConfig{Enabled: true, DryRun: true, ArchiveDir: archives}
	return NewRemediator(st, nil, nil, cfg, nil, zerolog.Nop()), st, archives
}

// Наблюдение не должно превращать историю в ленту опросов: пока ВМ лежит,
// мониторинг предлагает одно и то же решение каждую минуту, и без защиты каждая
// минута оседала бы отдельной записью на странице оповещений. Отсекается тем же
// интервалом, что и повторные попытки в боевом режиме, — история периода
// наблюдения показывает, что происходило бы в бою.
func TestObserveModeRecordsRepeatedSituationOnce(t *testing.T) {
	ctx := context.Background()
	_, st, dir := testRemediator(t)

	cfg := config.RemediationConfig{
		Enabled: true, DryRun: true, AllowVMStart: true,
		Cooldown: time.Hour, ArchiveDir: dir,
	}
	r := NewRemediator(st, nil, nil, cfg, nil, zerolog.Nop())

	sit := Situation{
		ServerID: "srv-1", Scope: model.ScopeVM, ObjectID: "vm-1", ObjectName: "db-01",
		Action: model.ActionVMStart, Reason: "ВМ выключена, а должна работать",
	}

	for i := 0; i < 4; i++ {
		if _, err := r.Consider(ctx, sit); err != nil {
			t.Fatalf("опрос %d: %v", i+1, err)
		}
	}

	records, err := st.ListRemediations(ctx, "srv-1", 100)
	if err != nil {
		t.Fatalf("чтение истории: %v", err)
	}
	if len(records) != 1 {
		statuses := make([]string, 0, len(records))
		for _, rec := range records {
			statuses = append(statuses, string(rec.Status))
		}
		t.Fatalf("четыре опроса одной ситуации оставили %d записей (%v), ожидалась одна",
			len(records), statuses)
	}
	if records[0].Status != model.RemDryRun {
		t.Errorf("статус записи %q, ожидался %q", records[0].Status, model.RemDryRun)
	}

	// Решение оператора — не повтор мониторинга: его запись должна пройти, иначе
	// нажатие кнопки в интерфейсе осталось бы без следа в истории.
	forced := sit
	forced.Force = true
	forced.TriggeredBy = "user:admin"
	if _, err := r.Consider(ctx, forced); err != nil {
		t.Fatalf("ручной запуск: %v", err)
	}
	records, err = st.ListRemediations(ctx, "srv-1", 100)
	if err != nil {
		t.Fatalf("чтение истории: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("после ручного запуска записей %d, ожидалось 2", len(records))
	}
}

// The mode must survive a restart. An operator halfway through observing the
// automation would be badly served by a service that quietly starts acting
// because it was restarted for an unrelated reason.
func TestModeSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	r, st, _ := testRemediator(t)

	if err := r.RestoreMode(ctx); err != nil {
		t.Fatalf("восстановление режима: %v", err)
	}
	if !r.DryRun() {
		t.Fatal("по умолчанию должен быть режим проверки")
	}

	if _, _, err := r.SwitchMode(ctx, false, "admin", "проверено"); err != nil {
		t.Fatalf("переключение: %v", err)
	}
	if r.DryRun() {
		t.Fatal("после переключения должен быть боевой режим")
	}

	// Новый процесс с той же базой: конфигурация по-прежнему говорит dry_run,
	// но сохранённый режим должен победить.
	restarted := NewRemediator(st, nil, nil,
		config.RemediationConfig{Enabled: true, DryRun: true}, nil, zerolog.Nop())
	if err := restarted.RestoreMode(ctx); err != nil {
		t.Fatalf("восстановление после перезапуска: %v", err)
	}
	if restarted.DryRun() {
		t.Error("перезапуск вернул систему в режим проверки, хотя оператор из него вышел")
	}
}

// Leaving check mode must leave behind what was observed: that is the evidence
// the decision to go live rests on.
func TestLeavingCheckModeWritesArchive(t *testing.T) {
	ctx := context.Background()
	r, st, archiveDir := testRemediator(t)
	if err := r.RestoreMode(ctx); err != nil {
		t.Fatalf("восстановление режима: %v", err)
	}

	// Три решения внутри периода наблюдения.
	for i, status := range []model.RemediationStatus{
		model.RemDryRun, model.RemDryRun, model.RemSkipped,
	} {
		rec := &model.RemediationRecord{
			ID: string(rune('a'+i)) + "-rec", ServerID: "srv-1", Scope: model.ScopeVM,
			ObjectID: "vm-1", ObjectName: "db-01", Action: model.ActionVMStart,
			Reason: "ВМ выключена, а должна работать", Status: status,
			TriggeredBy: "monitor", CreatedAt: time.Now().UTC(),
		}
		if err := st.RecordRemediation(ctx, rec); err != nil {
			t.Fatalf("запись решения: %v", err)
		}
	}

	closed, opened, err := r.SwitchMode(ctx, false, "admin", "решения верные")
	if err != nil {
		t.Fatalf("переключение: %v", err)
	}
	if opened.DryRun {
		t.Error("новый период должен быть боевым")
	}
	if closed.ArchivePath == "" {
		t.Fatal("архив не создан")
	}
	if _, err := os.Stat(closed.ArchivePath); err != nil {
		t.Fatalf("файла архива нет: %v", err)
	}
	if filepath.Dir(closed.ArchivePath) != archiveDir {
		t.Errorf("архив лёг в %s, ожидался каталог %s", closed.ArchivePath, archiveDir)
	}

	archive, err := ReadArchive(closed.ArchivePath)
	if err != nil {
		t.Fatalf("чтение архива: %v", err)
	}
	if len(archive.Decisions) != 3 {
		t.Errorf("в архиве %d решений, ожидалось 3", len(archive.Decisions))
	}
	if archive.Summary.Suppressed != 2 || archive.Summary.Skipped != 1 {
		t.Errorf("сводка: подавлено %d, пропущено %d — ожидалось 2 и 1",
			archive.Summary.Suppressed, archive.Summary.Skipped)
	}
	if archive.Summary.Objects != 1 {
		t.Errorf("объектов %d, ожидался 1", archive.Summary.Objects)
	}
	if archive.ClosedBy != "admin" || archive.CloseNote != "решения верные" {
		t.Errorf("обоснование перехода не сохранено: %+v", archive)
	}

	// Сводка должна лежать и в записи о периоде, чтобы список открывался
	// без чтения файлов.
	stored, err := st.GetRemediationPeriod(ctx, closed.ID)
	if err != nil {
		t.Fatalf("чтение периода: %v", err)
	}
	if stored.Summary == nil || stored.Summary.Total != 3 {
		t.Errorf("сводка периода не сохранена: %+v", stored.Summary)
	}
	if stored.Open() {
		t.Error("период остался открытым")
	}
}

// Going into check mode produces no archive: there is nothing observed yet, and
// what happened during live operation is the remediation history itself.
func TestEnteringCheckModeMakesNoArchive(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRemediator(t)
	if err := r.RestoreMode(ctx); err != nil {
		t.Fatalf("восстановление режима: %v", err)
	}

	if _, _, err := r.SwitchMode(ctx, false, "admin", ""); err != nil {
		t.Fatalf("выход в боевой: %v", err)
	}
	closed, _, err := r.SwitchMode(ctx, true, "admin", "снова наблюдаем")
	if err != nil {
		t.Fatalf("возврат в проверку: %v", err)
	}
	if closed.ArchivePath != "" {
		t.Error("боевой период не должен порождать архив")
	}
	if !r.DryRun() {
		t.Error("режим не переключился")
	}
}

// Switching to the mode already in force is a mistake worth reporting rather
// than a no-op: it would otherwise close a period and open an identical one,
// splitting the observation window for no reason.
func TestSwitchingToSameModeIsRejected(t *testing.T) {
	ctx := context.Background()
	r, _, _ := testRemediator(t)
	if err := r.RestoreMode(ctx); err != nil {
		t.Fatalf("восстановление режима: %v", err)
	}

	if _, _, err := r.SwitchMode(ctx, true, "admin", ""); err == nil {
		t.Error("повторное включение того же режима должно отклоняться")
	}
}

// Only the decisions inside the window belong to the archive.
func TestArchiveExcludesDecisionsOutsideTheWindow(t *testing.T) {
	ctx := context.Background()
	r, st, _ := testRemediator(t)

	older := &model.RemediationRecord{
		ID: "old", ServerID: "srv-1", Scope: model.ScopeVM, ObjectID: "vm-9",
		ObjectName: "old-vm", Action: model.ActionVMStart, Status: model.RemDryRun,
		TriggeredBy: "monitor", CreatedAt: time.Now().UTC().Add(-2 * time.Hour),
	}
	if err := st.RecordRemediation(ctx, older); err != nil {
		t.Fatalf("запись старого решения: %v", err)
	}

	// Период открывается уже после этого решения.
	if err := r.RestoreMode(ctx); err != nil {
		t.Fatalf("восстановление режима: %v", err)
	}
	inside := &model.RemediationRecord{
		ID: "new", ServerID: "srv-1", Scope: model.ScopeVM, ObjectID: "vm-1",
		ObjectName: "db-01", Action: model.ActionVMUnpause, Status: model.RemDryRun,
		TriggeredBy: "monitor", CreatedAt: time.Now().UTC(),
	}
	if err := st.RecordRemediation(ctx, inside); err != nil {
		t.Fatalf("запись решения: %v", err)
	}

	closed, _, err := r.SwitchMode(ctx, false, "admin", "")
	if err != nil {
		t.Fatalf("переключение: %v", err)
	}
	archive, err := ReadArchive(closed.ArchivePath)
	if err != nil {
		t.Fatalf("чтение архива: %v", err)
	}
	if len(archive.Decisions) != 1 || archive.Decisions[0].ID != "new" {
		t.Errorf("в архив попало лишнее: %+v", archive.Decisions)
	}
}
