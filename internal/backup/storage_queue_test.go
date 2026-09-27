package backup

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestWaitDomainsQueuesOnlyHotBackupsOfRunningVMs(t *testing.T) {
	e := &Engine{places: NewPlaceLimiter(1), log: zerolog.Nop()}
	srv := &model.Server{ID: "engine"}
	up := &model.VM{Name: "db", Status: "up"}
	down := &model.VM{Name: "old", Status: "down"}
	full := &model.BackupRun{Type: model.BackupFull}

	hold, err := e.waitDomains(context.Background(), srv, up, full, []ovirt.Disk{diskOn("d1")})
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	quick := func(name string, vm *model.VM, run *model.BackupRun, domains ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		release, err := e.waitDomains(ctx, srv, vm, run, []ovirt.Disk{diskOn(domains...)})
		if err != nil {
			t.Fatalf("%s: не должен ждать очереди: %v", name, err)
		}
		release()
	}
	quick("другой домен", up, full, "d2")
	quick("выключенная ВМ", down, full, "d1")
	quick("только конфигурация", up, &model.BackupRun{Type: model.BackupConfig}, "d1")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.waitDomains(ctx, srv, up, &model.BackupRun{Type: model.BackupIncremental},
		[]ovirt.Disk{diskOn("d3", "d1")}); err == nil {
		t.Fatal("второй горячий бэкап на занятом домене должен ждать очереди")
	}

	// Тот же домен другого подключения — другая очередь.
	other := &Engine{places: e.places, log: zerolog.Nop()}
	release, err := other.waitDomains(context.Background(), &model.Server{ID: "engine-2"}, up, full, []ovirt.Disk{diskOn("d1")})
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestWaitDomainsDisabledByDefault(t *testing.T) {
	e := &Engine{places: NewPlaceLimiter(0), log: zerolog.Nop()}
	up := &model.VM{Status: "up"}
	for i := 0; i < 5; i++ {
		release, err := e.waitDomains(context.Background(), &model.Server{ID: "engine"}, up,
			&model.BackupRun{Type: model.BackupFull}, []ovirt.Disk{diskOn("d1")})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
}
