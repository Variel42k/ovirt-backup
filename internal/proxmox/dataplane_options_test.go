package proxmox

import (
	"reflect"
	"testing"
)

func TestParseProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want DataPlaneCaps
	}{
		{"помощник первой версии", "jhvirt-pve-data-plane/1\n", DataPlaneCaps{Protocol: 1}},
		{"вторая версия, узел старше 8.2", "jhvirt-pve-data-plane/2\n", DataPlaneCaps{Protocol: 2}},
		{"вторая версия с fleecing", "jhvirt-pve-data-plane/2\nfleecing\n", DataPlaneCaps{Protocol: 2, Fleecing: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProbe(tc.out)
			if err != nil || got != tc.want {
				t.Fatalf("ParseProbe(%q) = %+v, %v; ожидалось %+v", tc.out, got, err, tc.want)
			}
		})
	}
	if _, err := ParseProbe("something-else/9\n"); err == nil {
		t.Fatal("чужой протокол должен отвергаться")
	}
}

func TestBackupOptionsArgs(t *testing.T) {
	args, err := BackupOptions{BandwidthKiB: 51200, FleecingStorage: "local-lvm"}.args("qemu")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bwlimit=51200", "fleecing=local-lvm"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %q, ожидалось %q", args, want)
	}

	if args, err := (BackupOptions{}).args("lxc"); err != nil || args != nil {
		t.Fatalf("пустые параметры должны давать команду как раньше: %q, %v", args, err)
	}
	// Всё, что уходит в команду помощника, проверяется до отправки: помощник
	// отвергнет то же самое, но уже посреди ночного бэкапа.
	for _, bad := range []BackupOptions{
		{FleecingStorage: "local lvm"},
		{FleecingStorage: "../etc"},
		{BandwidthKiB: -1},
	} {
		if _, err := bad.args("qemu"); err == nil {
			t.Errorf("параметры %+v должны отвергаться", bad)
		}
	}
	if _, err := (BackupOptions{FleecingStorage: "local-lvm"}).args("lxc"); err == nil {
		t.Error("fleecing для контейнера должен отвергаться")
	}
}
