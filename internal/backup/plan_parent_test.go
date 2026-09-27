package backup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// Смешанная ВМ: системный диск qcow2 с режимом incremental, диск данных raw.
// Запуск остаётся инкрементом от последней точки, а raw-диск отмечен для
// копирования целиком — временный снапшот не нужен.
// mixedCase — история ВМ с qcow2- и raw-диском перед очередным инкрементом.
type mixedCase struct {
	rejectedAgo time.Duration // > 0 — столько назад движок отверг смешанный бэкап
	adHoc       bool          // разовый запуск, а не по заданию
	incAfter    bool          // после отказа уже снят инкремент
}

// resolveMixedPlan строит план инкремента для ВМ с qcow2- и raw-диском.
func resolveMixedPlan(t *testing.T, c mixedCase) plan {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	srv := &model.Server{ID: "srv", Name: "engine", Kind: model.KindOVirt, Username: "admin@internal",
		Password: "x", SupportsCBT: true}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sso/oauth/token"):
			_, _ = w.Write([]byte(`{"access_token":"t","exp":"9999999999999"}`))
		case strings.HasSuffix(r.URL.Path, "/vms/vm-1/checkpoints"):
			_, _ = w.Write([]byte(`{"checkpoint":[{"id":"cp-1"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(engine.Close)
	srv.EngineURL = engine.URL
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	target := &model.StorageTarget{ID: "t", Name: "local", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	usable := func(id string, typ model.BackupType, created time.Time) *model.BackupRun {
		run := &model.BackupRun{ID: id, ServerID: srv.ID, VMID: "vm-1", VMName: "mixed", Type: typ,
			Status: model.RunSucceeded, StorageTargetID: target.ID, ToCheckpointID: "cp-1",
			ChainID: "full", CreatedAt: created}
		if err := st.CreateBackupRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		for _, d := range []*model.BackupDisk{
			{RunID: id, DiskID: "d-os", Alias: "os", Status: model.RunSucceeded},
			{RunID: id, DiskID: "d-raw", Alias: "data", Index: 1, Status: model.RunSucceeded},
		} {
			if err := st.UpsertBackupDisk(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
		return run
	}
	full := usable("full", model.BackupFull, now.Add(-c.rejectedAgo-time.Hour))
	if c.rejectedAgo > 0 {
		// Полный запуск, в котором движок отверг смешанный бэкап.
		if err := st.AddRunEvent(ctx, &model.RunEvent{
			RunID: full.ID, Kind: model.RunEventDowngradedFull, At: now.Add(-c.rejectedAgo),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if c.incAfter {
		usable("inc", model.BackupIncremental, now.Add(-time.Minute))
	}
	client, err := ovirt.New(ovirt.Config{EngineURL: engine.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}

	e := NewEngine(st, nil, config.BackupConfig{}, nil, zerolog.Nop())
	run := &model.BackupRun{ID: "next", ServerID: srv.ID, VMID: "vm-1"}
	req := RunRequest{ServerID: srv.ID, VMID: "vm-1", Type: model.BackupIncremental, StorageTargetID: target.ID,
		JobID: "job-1"}
	if c.adHoc {
		req.JobID = ""
	}
	disks := []ovirt.Disk{
		{ID: "d-os", Alias: "os", Format: "cow", Backup: "incremental"},
		{ID: "d-raw", Alias: "data", Format: "raw", Backup: "none"},
	}
	p, err := e.resolvePlan(ctx, client, srv, run, req, disks)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMixedVMIncrementalCopiesRawDiskWhole(t *testing.T) {
	p := resolveMixedPlan(t, mixedCase{})
	if p.Type != model.BackupIncremental || p.FromCheckpointID != "cp-1" {
		t.Fatalf("смешанная ВМ: тип %s, основа %q; ожидался инкремент от cp-1", p.Type, p.FromCheckpointID)
	}
	if _, whole := p.FullDisks["d-raw"]; !whole {
		t.Fatalf("raw-диск не отмечен для копирования целиком: %v", p.FullDisks)
	}
	if _, whole := p.FullDisks["d-os"]; whole {
		t.Fatal("qcow2-диск с режимом incremental не должен копироваться целиком")
	}
}

// Движок старше 4.4.5 отверг смешанный бэкап — следующие запуски по заданию
// сразу полные: без лишней попытки и второй заморозки гостя. Через неделю,
// разовым запуском или после удачного инкремента служба пробует снова:
// движок могли обновить.
func TestMixedRejectionRemembered(t *testing.T) {
	p := resolveMixedPlan(t, mixedCase{rejectedAgo: time.Hour})
	if p.Type != model.BackupFull || p.FromCheckpointID != "" || len(p.FullDisks) != 0 {
		t.Fatalf("после свежего отказа: тип %s, основа %q, целиком %v; ожидался полный", p.Type, p.FromCheckpointID, p.FullDisks)
	}
	if !strings.Contains(p.Note, "отверг смешанный бэкап") {
		t.Fatalf("причина не объяснена: %q", p.Note)
	}

	for name, c := range map[string]mixedCase{
		"отказ старше недели":        {rejectedAgo: mixedRejectionRetry + time.Hour},
		"разовый запуск":             {rejectedAgo: time.Hour, adHoc: true},
		"инкремент уже после отказа": {rejectedAgo: time.Hour, incAfter: true},
	} {
		t.Run(name, func(t *testing.T) {
			p := resolveMixedPlan(t, c)
			if p.Type != model.BackupIncremental || p.FromCheckpointID != "cp-1" {
				t.Fatalf("тип %s, основа %q; ожидалась попытка смешанного инкремента", p.Type, p.FromCheckpointID)
			}
		})
	}
}
