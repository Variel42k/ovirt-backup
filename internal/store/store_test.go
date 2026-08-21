package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/secret"
	"github.com/Variel42k/ovirt-backup/internal/testdb"
)

// newTestStore opens a throwaway database with the full schema applied.
//
// Нужна настоящая PostgreSQL. Подставлять вместо неё что-то попроще было бы
// самообманом: именно так и появился дефект, из-за которого служба не
// поднималась на PostgreSQL при полностью зелёных тестах — схема проверялась
// на SQLite, а он типизирован динамически и прощает то, чего PostgreSQL не
// прощает.
//
// База берётся из JHV_TEST_POSTGRES_DSN; ./run test поднимает временную сам.
func newTestStore(t *testing.T) *Store {
	t.Helper()

	ctx := context.Background()
	db, err := Open(ctx, testdb.Config(t))
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
	return New(db, cipher)
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestServerRoundTripEncryptsPassword(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	srv := &model.Server{
		Name:      "engine-1",
		Kind:      model.KindRedVirt,
		EngineURL: "https://engine.example.org",
		Username:  "admin@internal",
		Password:  "s3cr3t",
		Enabled:   true,
		Tags:      []string{"prod", "msk"},
	}
	if err := s.CreateServer(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The password must not be readable straight from the column.
	var stored string
	if err := s.db.QueryRow(ctx, `SELECT password_enc FROM servers WHERE id=?`, srv.ID).Scan(&stored); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if stored == "s3cr3t" || stored == "" {
		t.Fatalf("password column is not encrypted: %q", stored)
	}

	got, err := s.GetServer(ctx, srv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Password != "s3cr3t" {
		t.Errorf("password = %q, want s3cr3t", got.Password)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "prod" {
		t.Errorf("tags = %v", got.Tags)
	}

	// An update with an empty password keeps the stored one.
	got.Password = ""
	got.Notes = "обновлено"
	if err := s.UpdateServer(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := s.GetServer(ctx, srv.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if again.Password != "s3cr3t" {
		t.Errorf("password after update = %q, want it preserved", again.Password)
	}
	if again.Notes != "обновлено" {
		t.Errorf("notes = %q", again.Notes)
	}
}

func TestLibvirtServerRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----"
	srv := &model.Server{
		Name:          "kvm-1",
		Kind:          model.KindKVM,
		Username:      "root",
		SSHHost:       "kvm1.example.org",
		SSHPort:       2222,
		SSHPrivateKey: key,
		SSHHostKey:    "ssh-ed25519 AAAAC3Nz…",
		ScratchDir:    "/srv/backup-scratch",
		Enabled:       true,
	}
	if err := s.CreateServer(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Приватный ключ — такой же секрет, как пароль, и в столбце его быть не должно.
	var stored string
	if err := s.db.QueryRow(ctx, `SELECT ssh_private_key_enc FROM servers WHERE id=?`, srv.ID).Scan(&stored); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if stored == key || stored == "" {
		t.Fatalf("приватный ключ хранится незашифрованным: %q", stored)
	}

	got, err := s.GetServer(ctx, srv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SSHPrivateKey != key {
		t.Error("приватный ключ не восстановился")
	}
	if got.SSHHost != "kvm1.example.org" || got.SSHPort != 2222 {
		t.Errorf("адрес SSH: %s:%d", got.SSHHost, got.SSHPort)
	}
	if got.ScratchDir != "/srv/backup-scratch" {
		t.Errorf("каталог scratch: %q", got.ScratchDir)
	}
	if !got.Kind.UsesLibvirt() {
		t.Error("тип подключения потерян")
	}

	// Пустой ключ при обновлении означает «оставить прежний» — форма не
	// должна возвращать секрет обратно, чтобы его сохранить.
	got.SSHPrivateKey = ""
	got.ScratchDir = "/srv/other"
	if err := s.UpdateServer(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := s.GetServer(ctx, srv.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if again.SSHPrivateKey != key {
		t.Error("приватный ключ затёрт при обновлении с пустым полем")
	}
	if again.ScratchDir != "/srv/other" {
		t.Errorf("каталог scratch не обновился: %q", again.ScratchDir)
	}
}

func TestServerValidationByKind(t *testing.T) {
	// У oVirt обязателен адрес движка, у libvirt — адрес хоста и способ входа.
	ovirtSrv := &model.Server{Name: "e", Kind: model.KindOVirt, Username: "admin@internal"}
	if err := ovirtSrv.Validate(); err == nil {
		t.Error("подключение к oVirt без адреса движка должно отклоняться")
	}
	ovirtSrv.EngineURL = "https://engine"
	if err := ovirtSrv.Validate(); err != nil {
		t.Errorf("корректное подключение к oVirt отклонено: %v", err)
	}

	kvmSrv := &model.Server{Name: "k", Kind: model.KindKVM, Username: "root"}
	if err := kvmSrv.Validate(); err == nil {
		t.Error("подключение к libvirt без адреса хоста должно отклоняться")
	}
	kvmSrv.SSHHost = "kvm1"
	if err := kvmSrv.Validate(); err == nil {
		t.Error("подключение к libvirt без пароля и ключа должно отклоняться")
	}
	kvmSrv.SSHPrivateKey = "key"
	if err := kvmSrv.Validate(); err != nil {
		t.Errorf("корректное подключение к libvirt отклонено: %v", err)
	}
	if got := kvmSrv.Target(); got != "ssh://root@kvm1:22" {
		t.Errorf("Target() = %q", got)
	}
}

func TestSyncVMsPreservesOperatorIntent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	srv := &model.Server{Name: "e", EngineURL: "https://e", Username: "u", Enabled: true}
	if err := s.CreateServer(ctx, srv); err != nil {
		t.Fatalf("create server: %v", err)
	}

	vms := []*model.VM{
		{ID: "vm-1", Name: "db-01", Status: "up"},
		{ID: "vm-2", Name: "web-01", Status: "down"},
	}
	if err := s.SyncVMs(ctx, srv.ID, vms); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if err := s.SetVMDesiredState(ctx, srv.ID, "vm-1", model.DesiredUp, true); err != nil {
		t.Fatalf("set desired: %v", err)
	}

	// A later poll reports a new status; the operator's intent must survive and
	// the VM that disappeared upstream must be dropped.
	vms = []*model.VM{{ID: "vm-1", Name: "db-01", Status: "paused", PauseStatus: "eio"}}
	if err := s.SyncVMs(ctx, srv.ID, vms); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	got, err := s.GetVM(ctx, srv.ID, "vm-1")
	if err != nil {
		t.Fatalf("get vm: %v", err)
	}
	if got.Status != "paused" || got.PauseStatus != "eio" {
		t.Errorf("status = %q/%q, want paused/eio", got.Status, got.PauseStatus)
	}
	if got.DesiredState != model.DesiredUp {
		t.Errorf("desired_state = %q, want %q", got.DesiredState, model.DesiredUp)
	}
	if !got.RemediationOptOut {
		t.Error("remediation_opt_out was reset by a sync")
	}

	all, err := s.ListVMs(ctx, srv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("len(vms) = %d, want 1 (vm-2 should have been pruned)", len(all))
	}
}

// Наружу оповещение уходит один раз — когда загорелось.
//
// Монитор сообщает об одной и той же беде каждые полминуты: на стенде такие
// повторы дошли до тринадцати тысяч у одного оповещения. Отправка на каждый
// повтор — это гарантированный способ добиться, чтобы канал перестали читать.
// Повторно сообщать следует только о том, что погасло и загорелось снова.
func TestAlertCallbackFiresOnlyWhenItStartsBurning(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	var raised []model.Alert
	s.OnAlertRaised(func(a model.Alert) { raised = append(raised, a) })

	raise := func() {
		if err := s.RaiseAlert(ctx, &model.Alert{
			ServerID: "srv", Scope: model.ScopeVM, ObjectID: "vm-1", ObjectName: "db-01",
			Kind: model.AlertVMPaused, Severity: model.SeverityCritical, Message: "ВМ на паузе",
		}); err != nil {
			t.Fatalf("raise: %v", err)
		}
	}

	raise()
	raise()
	raise()
	if len(raised) != 1 {
		t.Fatalf("сообщений наружу: %d, ожидалось одно на три повтора", len(raised))
	}
	if raised[0].ObjectName != "db-01" || raised[0].Severity != model.SeverityCritical {
		t.Errorf("в сообщение попало не то: %+v", raised[0])
	}

	// Погасло и загорелось снова — это новая беда, и о ней сообщить нужно.
	if err := s.ResolveAlert(ctx, "srv", model.ScopeVM, "vm-1", model.AlertVMPaused); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	raise()
	if len(raised) != 2 {
		t.Fatalf("после повторного возгорания сообщений: %d, ожидалось два", len(raised))
	}
}

func TestAlertDeduplication(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	raise := func() {
		if err := s.RaiseAlert(ctx, &model.Alert{
			ServerID: "srv", Scope: model.ScopeVM, ObjectID: "vm-1", ObjectName: "db-01",
			Kind: model.AlertVMPaused, Severity: model.SeverityCritical, Message: "ВМ на паузе",
		}); err != nil {
			t.Fatalf("raise: %v", err)
		}
	}
	raise()
	raise()
	raise()

	alerts, err := s.ListAlerts(ctx, AlertFilter{ServerID: "srv"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("len(alerts) = %d, want 1 deduplicated row", len(alerts))
	}
	if alerts[0].Count != 3 {
		t.Errorf("count = %d, want 3", alerts[0].Count)
	}
	if alerts[0].State != model.AlertFiring {
		t.Errorf("state = %q, want firing", alerts[0].State)
	}

	// Acknowledging must survive a re-raise, otherwise a flapping object would
	// keep un-acking itself and drown the operator.
	if err := s.AckAlert(ctx, alerts[0].ID, "operator"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	raise()
	alerts, _ = s.ListAlerts(ctx, AlertFilter{ServerID: "srv"})
	if alerts[0].State != model.AlertAcked {
		t.Errorf("state after re-raise = %q, want acked", alerts[0].State)
	}

	if err := s.ResolveAlert(ctx, "srv", model.ScopeVM, "vm-1", model.AlertVMPaused); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	alerts, _ = s.ListAlerts(ctx, AlertFilter{ServerID: "srv"})
	if alerts[0].State != model.AlertResolved {
		t.Errorf("state after resolve = %q, want resolved", alerts[0].State)
	}
	if alerts[0].ResolvedAt == nil {
		t.Error("resolved_at was not set")
	}

	// Re-raising a resolved alert reopens it.
	raise()
	alerts, _ = s.ListAlerts(ctx, AlertFilter{ServerID: "srv"})
	if alerts[0].State != model.AlertFiring {
		t.Errorf("state after reopen = %q, want firing", alerts[0].State)
	}
	if alerts[0].ResolvedAt != nil {
		t.Error("resolved_at should be cleared when the alert fires again")
	}
}

// Отпечаток манифеста досчитывается только там, где его нет.
//
// Копии, снятые до появления этого поля, лежат с пустым значением, и разбор
// каталога заполняет его из run.json. Но перезаписать уже сохранённый
// отпечаток нельзя ни при каких условиях: расхождение с ним и есть тот самый
// признак подмены, ради которого он хранится.
func TestManifestFingerprintIsFilledOnlyWhenMissing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	run := &model.BackupRun{
		ServerID: "srv", VMID: "vm-1", VMName: "db-01", Type: model.BackupSnapshot,
		Status: model.RunSucceeded, StorageTargetID: "tgt-1", CreatedAt: time.Now().UTC(),
	}
	if err := s.CreateBackupRun(ctx, run); err != nil {
		t.Fatalf("создание точки: %v", err)
	}

	filled, err := s.SetRunManifestSHA256(ctx, run.ID, "отпечаток-из-хранилища")
	if err != nil || !filled {
		t.Fatalf("досчёт отпечатка: filled=%v err=%v", filled, err)
	}
	stored, err := s.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("чтение точки: %v", err)
	}
	if stored.ManifestSHA256 != "отпечаток-из-хранилища" {
		t.Fatalf("отпечаток %q", stored.ManifestSHA256)
	}

	// Второй разбор с другим значением ничего не меняет: именно так выглядит
	// подменённый манифест, и затирать прежний отпечаток недопустимо.
	filled, err = s.SetRunManifestSHA256(ctx, run.ID, "чужой-отпечаток")
	if err != nil {
		t.Fatalf("повторный досчёт: %v", err)
	}
	if filled {
		t.Error("существующий отпечаток был перезаписан")
	}
	if stored, err = s.GetBackupRun(ctx, run.ID); err != nil {
		t.Fatalf("чтение точки: %v", err)
	}
	if stored.ManifestSHA256 != "отпечаток-из-хранилища" {
		t.Errorf("отпечаток изменился на %q", stored.ManifestSHA256)
	}
}

func TestBackupRunChainLookup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	mk := func(typ model.BackupType, chain string, idx int, checkpoint string, status model.RunStatus) *model.BackupRun {
		r := &model.BackupRun{
			ServerID: "srv", VMID: "vm-1", VMName: "db-01", Type: typ, Status: status,
			ChainID: chain, ChainIndex: idx, ToCheckpointID: checkpoint,
			StorageTargetID: "tgt-1", CreatedAt: time.Now().UTC().Add(time.Duration(idx) * time.Minute),
		}
		if err := s.CreateBackupRun(ctx, r); err != nil {
			t.Fatalf("create run: %v", err)
		}
		return r
	}

	full := mk(model.BackupFull, "", 0, "cp-full", model.RunSucceeded)
	if full.ChainID != full.ID {
		t.Errorf("a full run should root its own chain, got chain_id=%q id=%q", full.ChainID, full.ID)
	}
	mk(model.BackupIncremental, full.ID, 1, "cp-inc1", model.RunSucceeded)
	inc2 := mk(model.BackupIncremental, full.ID, 2, "cp-inc2", model.RunSucceeded)
	// A failed run must never be picked as a base.
	mk(model.BackupIncremental, full.ID, 3, "", model.RunFailed)

	base, err := s.LatestUsableRun(ctx, "srv", "vm-1", "tgt-1", false)
	if err != nil {
		t.Fatalf("latest usable: %v", err)
	}
	if base.ID != inc2.ID {
		t.Errorf("incremental base = %q, want %q", base.ID, inc2.ID)
	}

	fullBase, err := s.LatestUsableRun(ctx, "srv", "vm-1", "tgt-1", true)
	if err != nil {
		t.Fatalf("latest full: %v", err)
	}
	if fullBase.ID != full.ID {
		t.Errorf("differential base = %q, want the full run %q", fullBase.ID, full.ID)
	}

	chain, err := s.ListBackupRuns(ctx, RunFilter{ChainID: full.ID})
	if err != nil {
		t.Fatalf("list chain: %v", err)
	}
	if len(chain) != 4 {
		t.Errorf("chain length = %d, want 4", len(chain))
	}
}

func TestRemediationRateLimitCounters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for i := 0; i < 2; i++ {
		if err := s.RecordRemediation(ctx, &model.RemediationRecord{
			ServerID: "srv", Scope: model.ScopeVM, ObjectID: "vm-1", Action: model.ActionVMStart,
			Status: model.RemSucceeded, Attempt: i + 1, TriggeredBy: "monitor",
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	// A skipped decision is recorded for the audit trail but must not consume
	// the rate-limit budget.
	if err := s.RecordRemediation(ctx, &model.RemediationRecord{
		ServerID: "srv", Scope: model.ScopeVM, ObjectID: "vm-1", Action: model.ActionVMStart,
		Status: model.RemSkipped, TriggeredBy: "monitor",
	}); err != nil {
		t.Fatalf("record skipped: %v", err)
	}

	n, err := s.CountRecentRemediations(ctx, "srv", "vm-1", model.ActionVMStart, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("attempts = %d, want 2 (skipped must not count)", n)
	}

	last, err := s.LastRemediationAt(ctx, "srv", "vm-1", model.ActionVMStart)
	if err != nil {
		t.Fatalf("last: %v", err)
	}
	if last.IsZero() {
		t.Error("last remediation time is zero")
	}

	none, err := s.LastRemediationAt(ctx, "srv", "vm-9", model.ActionHostFence)
	if err != nil {
		t.Fatalf("last for unknown object: %v", err)
	}
	if !none.IsZero() {
		t.Errorf("expected zero time for an object that was never remediated, got %s", none)
	}
}

// «Успешный» бэкап с тихо выпавшим диском — самый опасный случай в системе,
// поэтому список пропущенного обязан пережить запись в базу и чтение обратно.
// Проверяется и обновление: список заполняется уже после создания записи.
func TestSkippedDisksSurviveRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	srv := &model.Server{
		ID: "srv-1", Name: "kvm", Kind: model.KindKVM,
		Username: "root", SSHHost: "h", Password: "p", Enabled: true,
	}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("сервер: %v", err)
	}

	run := &model.BackupRun{
		ID: "run-1", ServerID: srv.ID, VMID: "vm-1", VMName: "db-01",
		Type: model.BackupFull, Status: model.RunRunning, ChainID: "run-1",
		StorageTargetID: "tgt-1", CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatalf("создание запуска: %v", err)
	}

	// Пусто на входе — пусто на выходе, а не пустой JSON-массив.
	if got, err := st.GetBackupRun(ctx, run.ID); err != nil {
		t.Fatalf("чтение: %v", err)
	} else if len(got.SkippedDisks) != 0 {
		t.Errorf("без пропусков список должен быть пуст, получено %+v", got.SkippedDisks)
	}

	run.SkippedDisks = []model.SkippedDisk{
		{DiskID: "d-1", Name: "shared-data", Reason: "общий (shareable) диск", Excluded: false},
		{DiskID: "d-2", Name: "vdb", Reason: "исключён настройкой задания", Excluded: true},
	}
	run.Status = model.RunSucceeded
	if err := st.UpdateBackupRun(ctx, run); err != nil {
		t.Fatalf("обновление запуска: %v", err)
	}

	got, err := st.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("чтение после обновления: %v", err)
	}
	if len(got.SkippedDisks) != 2 {
		t.Fatalf("пропущенных дисков %d, ожидалось 2", len(got.SkippedDisks))
	}
	if got.SkippedDisks[0].Reason != "общий (shareable) диск" || got.SkippedDisks[0].Excluded {
		t.Errorf("первый пропуск разобран неверно: %+v", got.SkippedDisks[0])
	}
	if !got.SkippedDisks[1].Excluded {
		t.Error("исключение по настройке задания должно отличаться от ограничения")
	}
	// Статус при этом остаётся успешным: пропуск общего диска — не сбой.
	if got.Status != model.RunSucceeded {
		t.Errorf("статус %q, ожидался succeeded", got.Status)
	}
}
