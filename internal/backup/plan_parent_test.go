package backup

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
)

// Случай dtseven: в полном бэкапе системный диск сохранился, а диск на
// 1000 GiB — нет, хотя движок создал checkpoint для обоих. Инкремент от такой
// точки скопировал бы для большого диска одни изменения без основы — диск,
// который не восстановить. Следующий запуск обязан стать полным.
func TestIncrementalFromPartialParentBecomesFull(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	srv := &model.Server{ID: "srv", Name: "engine", Kind: model.KindOVirt, EngineURL: "https://engine",
		Username: "admin@internal", Password: "x", SupportsCBT: true}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	target := &model.StorageTarget{ID: "t", Name: "local", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	parent := &model.BackupRun{ID: "partial", ServerID: srv.ID, VMID: "vm-1", VMName: "dtseven",
		Type: model.BackupFull, Status: model.RunPartial, StorageTargetID: target.ID, ToCheckpointID: "cp-1"}
	if err := st.CreateBackupRun(ctx, parent); err != nil {
		t.Fatal(err)
	}
	for _, d := range []*model.BackupDisk{
		{RunID: parent.ID, DiskID: "d-os", Alias: "Pseven05-singledisk", Status: model.RunSucceeded},
		{RunID: parent.ID, DiskID: "d-big", Alias: "DTSeven_05_Disk1", Index: 1, Status: model.RunFailed},
	} {
		if err := st.UpsertBackupDisk(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	e := NewEngine(st, nil, config.BackupConfig{}, nil, zerolog.Nop())
	run := &model.BackupRun{ID: "next", ServerID: srv.ID, VMID: "vm-1"}
	req := RunRequest{ServerID: srv.ID, VMID: "vm-1", Type: model.BackupIncremental, StorageTargetID: target.ID}
	disks := []ovirt.Disk{
		{ID: "d-os", Alias: "Pseven05-singledisk", Backup: "incremental"},
		{ID: "d-big", Alias: "DTSeven_05_Disk1", Backup: "incremental"},
	}

	// Клиент движка не нужен: решение принимается до проверки checkpoint.
	p, err := e.resolvePlan(ctx, nil, srv, run, req, disks)
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != model.BackupFull || p.FromCheckpointID != "" {
		t.Fatalf("инкремент от неполной точки: тип %s, основа %q", p.Type, p.FromCheckpointID)
	}
	if !strings.Contains(p.Note, "DTSeven_05_Disk1") {
		t.Fatalf("причина не названа: %q", p.Note)
	}
}
