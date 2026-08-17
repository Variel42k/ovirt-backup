package store

import (
	"context"
	"testing"

	"adveng/jh_virt/internal/model"
)

// Настройки хранилища должны переживать запись и чтение целиком. Список полей в
// INSERT, UPDATE и Scan длинный и позиционный: сдвиг на одну колонку не ломает
// сборку, а тихо кладёт домен в поле имени папки.
func TestStorageTargetRoundTripKeepsEveryField(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	targets := []*model.StorageTarget{
		{
			ID: "smb-1", Name: "шара NAS", Kind: model.StorageSMB, Enabled: true,
			Host: "nas.example.org", Port: 445, Share: "backups", Domain: "EXAMPLE",
			Username: "svc-backup", Password: "пароль шары", BasePath: "jhvirt",
			RateLimit: 50 << 20,
		},
		{
			ID: "dav-1", Name: "Nextcloud", Kind: model.StorageWebDAV, Enabled: true,
			Endpoint: "https://cloud.example.org/remote.php/dav/files/backup",
			Username: "backup", Password: "пароль приложения", BasePath: "копии",
			InsecureTLS: true,
		},
	}
	for _, target := range targets {
		if err := st.CreateStorageTarget(ctx, target); err != nil {
			t.Fatalf("создание хранилища %s: %v", target.ID, err)
		}
	}

	for _, want := range targets {
		got, err := st.GetStorageTarget(ctx, want.ID)
		if err != nil {
			t.Fatalf("чтение хранилища %s: %v", want.ID, err)
		}
		for field, pair := range map[string][2]string{
			"Name":     {got.Name, want.Name},
			"Kind":     {string(got.Kind), string(want.Kind)},
			"Host":     {got.Host, want.Host},
			"Share":    {got.Share, want.Share},
			"Domain":   {got.Domain, want.Domain},
			"Endpoint": {got.Endpoint, want.Endpoint},
			"Username": {got.Username, want.Username},
			"Password": {got.Password, want.Password},
			"BasePath": {got.BasePath, want.BasePath},
		} {
			if pair[0] != pair[1] {
				t.Errorf("хранилище %s, поле %s: получено %q, ожидалось %q",
					want.ID, field, pair[0], pair[1])
			}
		}
		if got.InsecureTLS != want.InsecureTLS {
			t.Errorf("хранилище %s: InsecureTLS %v, ожидалось %v",
				want.ID, got.InsecureTLS, want.InsecureTLS)
		}
		if got.Port != want.Port || got.RateLimit != want.RateLimit {
			t.Errorf("хранилище %s: порт %d и лимит %d, ожидались %d и %d",
				want.ID, got.Port, got.RateLimit, want.Port, want.RateLimit)
		}
	}

	// Пароль в базе лежит зашифрованным: в открытом виде его там быть не должно.
	var stored string
	if err := st.db.QueryRow(ctx,
		`SELECT password_enc FROM storage_targets WHERE id=?`, "smb-1").Scan(&stored); err != nil {
		t.Fatalf("чтение зашифрованного пароля: %v", err)
	}
	if stored == "" || stored == "пароль шары" {
		t.Errorf("пароль хранится не зашифрованным: %q", stored)
	}

	// Пустой пароль при правке означает «оставить прежний»: иначе смена любого
	// другого поля требовала бы вводить пароль заново.
	updated := *targets[0]
	updated.Password, updated.Share, updated.Domain = "", "archive", ""
	if err := st.UpdateStorageTarget(ctx, &updated); err != nil {
		t.Fatalf("правка хранилища: %v", err)
	}
	got, err := st.GetStorageTarget(ctx, "smb-1")
	if err != nil {
		t.Fatalf("чтение после правки: %v", err)
	}
	if got.Password != "пароль шары" {
		t.Errorf("пароль после правки: %q", got.Password)
	}
	if got.Share != "archive" || got.Domain != "" {
		t.Errorf("правка не применилась: папка %q, домен %q", got.Share, got.Domain)
	}
}

// Deleting a repository that a job writes to turns that job into a scheduled
// failure at two in the morning. Counting only the backups already in it is not
// enough: a target configured yesterday has none yet and is still in use.
func TestJobsOnTargetFindsJobsWithoutBackups(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	kept := &model.StorageTarget{
		ID: "target-kept", Name: "s3-main", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true,
	}
	other := &model.StorageTarget{
		ID: "target-free", Name: "s3-spare", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true,
	}
	for _, tgt := range []*model.StorageTarget{kept, other} {
		if err := st.CreateStorageTarget(ctx, tgt); err != nil {
			t.Fatalf("создание хранилища: %v", err)
		}
	}

	srv := &model.Server{
		ID: "srv-1", Name: "kvm", Kind: model.KindKVM,
		Username: "root", SSHHost: "h", Password: "p", Enabled: true,
	}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("создание сервера: %v", err)
	}

	job := &model.BackupJob{
		ID: "job-1", Name: "nightly", ServerID: srv.ID, VMIDs: []string{"vm-1"},
		Type: model.BackupFull, StorageTargetIDs: []string{kept.ID}, Enabled: true,
	}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatalf("создание задания: %v", err)
	}

	// Ни одного бэкапа в хранилище нет — только ссылка из задания.
	if n, err := st.CountRunsOnTarget(ctx, kept.ID); err != nil || n != 0 {
		t.Fatalf("бэкапов %d, ошибка %v — ожидалось пусто", n, err)
	}

	names, err := st.JobsOnTarget(ctx, kept.ID)
	if err != nil {
		t.Fatalf("JobsOnTarget: %v", err)
	}
	if len(names) != 1 || names[0] != "nightly" {
		t.Errorf("получено %v, ожидалось [nightly]", names)
	}

	free, err := st.JobsOnTarget(ctx, other.ID)
	if err != nil {
		t.Fatalf("JobsOnTarget: %v", err)
	}
	if len(free) != 0 {
		t.Errorf("хранилище без заданий отмечено занятым: %v", free)
	}
}

// A job listing several targets must be reported for each of them, and exactly
// once even if a target were repeated.
func TestJobsOnTargetHandlesMultipleTargets(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	var ids []string
	for _, name := range []string{"a", "b"} {
		tgt := &model.StorageTarget{
			ID: "target-" + name, Name: "store-" + name, Kind: model.StorageLocal,
			BasePath: t.TempDir(), Enabled: true,
		}
		if err := st.CreateStorageTarget(ctx, tgt); err != nil {
			t.Fatalf("создание хранилища: %v", err)
		}
		ids = append(ids, tgt.ID)
	}

	srv := &model.Server{
		ID: "srv-1", Name: "kvm", Kind: model.KindKVM,
		Username: "root", SSHHost: "h", Password: "p", Enabled: true,
	}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("создание сервера: %v", err)
	}

	job := &model.BackupJob{
		ID: "job-1", Name: "3-2-1", ServerID: srv.ID, VMIDs: []string{"vm-1"},
		Type: model.BackupFull, StorageTargetIDs: []string{ids[0], ids[0], ids[1]}, Enabled: true,
	}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatalf("создание задания: %v", err)
	}

	for _, id := range ids {
		names, err := st.JobsOnTarget(ctx, id)
		if err != nil {
			t.Fatalf("JobsOnTarget(%s): %v", id, err)
		}
		if len(names) != 1 {
			t.Errorf("хранилище %s: получено %v, ожидалась одна запись", id, names)
		}
	}
}
