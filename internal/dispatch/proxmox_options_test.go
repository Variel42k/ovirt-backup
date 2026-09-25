package dispatch

import (
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/proxmox"
)

var (
	helperV1         = proxmox.DataPlaneCaps{Protocol: 1}
	helperV2Old      = proxmox.DataPlaneCaps{Protocol: 2}
	helperV2Fleecing = proxmox.DataPlaneCaps{Protocol: 2, Fleecing: true}
)

func TestProxmoxBackupOptionsAppliesLimitAndFleecing(t *testing.T) {
	opts, applied, skipped := proxmoxBackupOptions("local-lvm", "qemu/101", 50, helperV2Fleecing)
	if opts.BandwidthKiB != 50*1024 || opts.FleecingStorage != "local-lvm" {
		t.Fatalf("параметры = %+v", opts)
	}
	if len(applied) != 2 || len(skipped) != 0 {
		t.Fatalf("применено %q, пропущено %q", applied, skipped)
	}
}

// Бэкап без параметров остаётся таким же, как до их появления: старый
// помощник на узле не должен получать того, чего не понимает.
func TestProxmoxBackupOptionsEmptyByDefault(t *testing.T) {
	opts, applied, skipped := proxmoxBackupOptions("", "qemu/101", 0, helperV1)
	if opts != (proxmox.BackupOptions{}) || applied != nil || skipped != nil {
		t.Fatalf("ожидался бэкап без параметров: %+v %q %q", opts, applied, skipped)
	}
}

func TestProxmoxBackupOptionsOldHelperSkipsEverything(t *testing.T) {
	opts, _, skipped := proxmoxBackupOptions("local-lvm", "qemu/101", 50, helperV1)
	if opts != (proxmox.BackupOptions{}) {
		t.Fatalf("старому помощнику нельзя передавать параметры: %+v", opts)
	}
	if len(skipped) != 2 || !strings.Contains(skipped[0], "обновите") {
		t.Fatalf("причины = %q", skipped)
	}
}

func TestProxmoxBackupOptionsFleecingNeedsPVE82(t *testing.T) {
	opts, _, skipped := proxmoxBackupOptions("local-lvm", "qemu/101", 0, helperV2Old)
	if opts.FleecingStorage != "" {
		t.Fatal("fleecing на узле без поддержки уронил бы vzdump")
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "8.2") {
		t.Fatalf("причины = %q", skipped)
	}
}

// У контейнера fleecing не бывает, и это не повод для предупреждения.
func TestProxmoxBackupOptionsContainerHasNoFleecing(t *testing.T) {
	opts, applied, skipped := proxmoxBackupOptions("local-lvm", "lxc/205", 0, helperV2Fleecing)
	if opts.FleecingStorage != "" || applied != nil || skipped != nil {
		t.Fatalf("для контейнера ожидалось без fleecing и без замечаний: %+v %q %q", opts, applied, skipped)
	}
}
