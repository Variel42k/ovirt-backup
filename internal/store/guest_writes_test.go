package store

import (
	"context"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestGuestWritesDuringRuns(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	server := &model.Server{Name: "writes-kvm", Kind: model.KindKVM, Enabled: true}
	if err := st.CreateServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Truncate(time.Second)
	newRun := func() *model.BackupRun {
		run := &model.BackupRun{
			ServerID: server.ID, VMID: "vm", VMName: "database", Type: model.BackupFull,
			Status: model.RunSucceeded, StorageTargetID: "target", CreatedAt: started,
		}
		if err := st.CreateBackupRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		return run
	}
	first, second, other := newRun(), newRun(), newRun()

	const mib = 1 << 20
	if err := st.AddDiskSamples(ctx, []model.DiskSample{
		// Первый запуск: vda 10 с по 1 и 3 МиБ/с — в среднем 2 МиБ/с, 20 МиБ.
		{ServerID: server.ID, RunID: first.ID, VMID: "vm", Disk: "vda", At: started, WriteBytesPerSec: 1 * mib},
		{ServerID: server.ID, RunID: first.ID, VMID: "vm", Disk: "vda", At: started.Add(10 * time.Second), WriteBytesPerSec: 3 * mib},
		// Один замер — время не посчитать, в ответ не попадает.
		{ServerID: server.ID, RunID: first.ID, VMID: "vm", Disk: "vdb", At: started, WriteBytesPerSec: 9 * mib},
		// Второй запуск: гость не писал.
		{ServerID: server.ID, RunID: second.ID, VMID: "vm", Disk: "vda", At: started, WriteBytesPerSec: 0},
		{ServerID: server.ID, RunID: second.ID, VMID: "vm", Disk: "vda", At: started.Add(4 * time.Second), WriteBytesPerSec: 0},
		// Запуск, о котором не спрашивали.
		{ServerID: server.ID, RunID: other.ID, VMID: "vm", Disk: "vda", At: started, WriteBytesPerSec: 5 * mib},
		{ServerID: server.ID, RunID: other.ID, VMID: "vm", Disk: "vda", At: started.Add(time.Second), WriteBytesPerSec: 5 * mib},
		// Обычный мониторинг без запуска.
		{ServerID: server.ID, VMID: "vm", Disk: "vda", At: started, WriteBytesPerSec: 7 * mib},
	}); err != nil {
		t.Fatal(err)
	}

	writes, err := st.GuestWritesDuringRuns(ctx, server.ID, []string{first.ID, second.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, w := range writes {
		got[w.RunID+"/"+w.Disk] = w.Bytes
	}
	want := map[string]int64{first.ID + "/vda": 20 * mib, second.ID + "/vda": 0}
	if len(got) != len(want) {
		t.Fatalf("writes = %+v, want %+v", got, want)
	}
	for key, bytes := range want {
		if got[key] != bytes {
			t.Fatalf("writes[%s] = %d, want %d (all: %+v)", key, got[key], bytes, got)
		}
	}

	if writes, err := st.GuestWritesDuringRuns(ctx, server.ID, nil); err != nil || writes != nil {
		t.Fatalf("empty run list: writes=%+v err=%v", writes, err)
	}
}
