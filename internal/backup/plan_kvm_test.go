package backup

import (
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// На KVM смешанного бэкапа нет: один raw-диск делает каждый запуск полным.
// Рекомендатель не должен обещать инкремент, который драйвер не снимет.
func TestKVMRawDiskBlocksIncrements(t *testing.T) {
	a := Assessment{
		Libvirt: true, EngineSupportsCBT: true,
		DiskCount: 2, CBTEnabled: 1, CBTPossible: 1, RawDisks: 1,
		TotalUsed: 250 * gib, AverageIncrement: 3 * gib,
		Disks: []DiskFacts{
			{Alias: "vda", Format: "cow", BackupMode: "incremental", ActualSize: 50 * gib},
			{Alias: "vdb", Format: "raw", CBTBlocker: "формат raw", ActualSize: 200 * gib},
		},
	}
	options := buildOptions(a)

	for _, typ := range []model.BackupType{model.BackupIncremental, model.BackupDifferential} {
		o := optionOf(t, options, typ)
		if o.Available || !strings.Contains(o.Blocker, "на KVM инкремент требует qcow2") || !strings.Contains(o.Blocker, "vdb") {
			t.Fatalf("%s: available=%v blocker=%q", typ, o.Available, o.Blocker)
		}
		if len(o.Prerequisites) != 1 || !strings.Contains(o.Prerequisites[0], "vdb") ||
			!strings.Contains(o.Prerequisites[0], "200.0 ГБ") {
			t.Fatalf("%s: prerequisites=%q", typ, o.Prerequisites)
		}
	}
	full := optionOf(t, options, model.BackupFull)
	if !full.Available || !full.Recommended || !strings.Contains(full.Rationale, "libvirt") ||
		strings.Contains(full.Rationale, "Backup API") {
		t.Fatalf("full = %+v", full)
	}
	for _, o := range options {
		if strings.Contains(o.Rationale, "oVirt") {
			t.Fatalf("%s: на KVM не место тексту про oVirt: %q", o.Type, o.Rationale)
		}
	}

	presets := buildPresets(a)
	for _, p := range presets {
		if p.Type == model.BackupIncremental && p.Recommended {
			t.Fatalf("расписание %q с инкрементами рекомендовано ВМ, где инкрементов не будет", p.Name)
		}
		if p.Type == model.BackupFull && !p.Recommended {
			t.Fatalf("расписание %q должно быть рекомендовано", p.Name)
		}
	}
}

func TestKVMAllQcow2OffersIncrements(t *testing.T) {
	a := Assessment{
		Libvirt: true, EngineSupportsCBT: true,
		DiskCount: 2, CBTEnabled: 2, CBTPossible: 2, TotalUsed: 100 * gib,
		Disks: []DiskFacts{
			{Alias: "vda", Format: "cow", BackupMode: "incremental"},
			{Alias: "vdb", Format: "cow", BackupMode: "incremental"},
		},
	}
	options := buildOptions(a)
	inc := optionOf(t, options, model.BackupIncremental)
	if !inc.Available || !inc.Recommended || strings.Contains(inc.Rationale, "смешанный") {
		t.Fatalf("incremental = %+v", inc)
	}
}

// oVirt с тем же набором дисков по-прежнему получает смешанный инкремент.
func TestOVirtMixedStillOffered(t *testing.T) {
	a := Assessment{
		EngineSupportsCBT: true, DiskCount: 2, CBTEnabled: 1, CBTPossible: 1, RawDisks: 1, TotalUsed: 100 * gib,
		Disks: []DiskFacts{
			{Alias: "system", Format: "cow", BackupMode: "incremental"},
			{Alias: "data", Format: "raw", CBTBlocker: "формат raw"},
		},
	}
	inc := optionOf(t, buildOptions(a), model.BackupIncremental)
	if !inc.Available || !strings.Contains(inc.Rationale, "смешанный бэкап") {
		t.Fatalf("incremental = %+v", inc)
	}
}
