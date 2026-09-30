package backup

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestStuckSnapshotVolumes(t *testing.T) {
	fake := &fakeLockedEngine{volumeState: "locked"}
	client := startFakeLockedEngine(t, fake)
	ctx := context.Background()
	snap := ovirt.Snapshot{ID: "s1", Description: "jhvirt backup old", SnapshotStatus: "ok"}

	if got := stuckSnapshotVolumes(ctx, client, "vm1", snap, nil); len(got) != 1 || got[0] != "vol-1" {
		t.Fatalf("застрявший том: %v", got)
	}
	busyDisk := []ovirt.ImageTransfer{{ID: "t", Phase: "transferring", Disk: ovirt.Ref{ID: "d1"}}}
	if got := stuckSnapshotVolumes(ctx, client, "vm1", snap, busyDisk); got != nil {
		t.Fatalf("том с идущей передачей не застрявший: %v", got)
	}
	snap.SnapshotStatus = "locked"
	if got := stuckSnapshotVolumes(ctx, client, "vm1", snap, nil); got != nil {
		t.Fatalf("снапшот в операции (слияние) — не застрявший том: %v", got)
	}
}

func TestStuckChainErrorStopsBeforeSnapshot(t *testing.T) {
	ctx := context.Background()
	vm := &model.VM{ID: "vm1", Name: "ADV-GITLAB"}
	disks := []ovirt.Disk{{ID: "d1", Alias: "ADV-GITLAB_Disk1"}}
	srv := &model.Server{Name: "ovirt", EngineURL: "https://engine.example"}

	stuck := startFakeLockedEngine(t, &fakeLockedEngine{diskState: "ok", volumeState: "locked"})
	e := &Engine{log: zerolog.Nop()}
	run := &model.BackupRun{ID: "r1"}
	err := e.stuckChainError(ctx, stuck, srv, vm, run, disks)
	if err == nil || !strings.Contains(err.Error(), "бэкап не начат") ||
		!strings.Contains(err.Error(), "vol-1 (снапшот «jhvirt-backup»)") || !strings.Contains(err.Error(), "Новый снапшот не создан") {
		t.Fatalf("ожидался отказ до снапшота с именем тома: %v", err)
	}
	unlock := false
	for _, s := range run.ManualSteps {
		unlock = unlock || strings.Contains(s.Command, "unlock_entity.sh -t disk 'd1'")
	}
	if !unlock {
		t.Fatalf("нужна команда администратору oVirt: %+v", run.ManualSteps)
	}

	free := startFakeLockedEngine(t, &fakeLockedEngine{diskState: "ok", volumeState: "ok"})
	if err := e.stuckChainError(ctx, free, srv, vm, &model.BackupRun{ID: "r2"}, disks); err != nil {
		t.Fatalf("свободная цепочка: %v", err)
	}
}

func TestOtherActiveRun(t *testing.T) {
	runs := []*model.BackupRun{{ID: "current"}}
	if otherActiveRun(runs, "current") {
		t.Fatal("сам запуск, который убирает перед снапшотом, не мешает уборке")
	}
	if !otherActiveRun(append(runs, &model.BackupRun{ID: "other"}), "current") || !otherActiveRun(runs, "") {
		t.Fatal("другой идущий бэкап ВМ откладывает уборку")
	}
}
