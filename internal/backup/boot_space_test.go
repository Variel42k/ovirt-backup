package backup

import (
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestCheckBootDomain(t *testing.T) {
	// Домен 1 ТиБ: запас 5 % — 51,2 ГиБ.
	domain := func(free int64, status string) *model.StorageDomain {
		return &model.StorageDomain{ID: "d", Name: "data1", Status: status,
			AvailableSize: free, UsedSize: 1024*gib - free}
	}
	cases := []struct {
		name     string
		sd       *model.StorageDomain
		data     int64
		full     int64
		verdict  string
		contains string
	}{
		{"с запасом", domain(600*gib, "active"), 50 * gib, 200 * gib, BootSpaceOK, "хватает"},
		{"полный размер не помещается", domain(200*gib, "active"), 50 * gib, 500 * gib, BootSpaceTight, "может не хватить"},
		{"данные не помещаются", domain(80*gib, "active"), 50 * gib, 500 * gib, BootSpaceShort, "не хватит"},
		{"домен в обслуживании", domain(600*gib, "maintenance"), 50 * gib, 200 * gib, BootSpaceInactive, "не активен"},
		{"место неизвестно", &model.StorageDomain{ID: "d", Status: "active"}, 50 * gib, 200 * gib, BootSpaceUnknown, "не сообщил"},
		{"объём копии неизвестен", domain(600*gib, "active"), -1, -1, BootSpaceUnknown, "объём копии неизвестен"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CheckBootDomain(tc.sd, tc.data, tc.full)
			if c.Verdict != tc.verdict || !strings.Contains(c.Message, tc.contains) {
				t.Fatalf("verdict=%s message=%q", c.Verdict, c.Message)
			}
		})
	}
	if c := CheckBootDomain(domain(600*gib, "active"), gib, gib); c.Reserve != DomainReserve(1024*gib) {
		t.Fatalf("запас %d, want %d — как у сторожа домена при горячем бэкапе", c.Reserve, DomainReserve(1024*gib))
	}
}

func TestQcow2InitialSize(t *testing.T) {
	// ADV-GITLAB: диск 300 ГиБ, в копии около 181 ГиБ данных.
	got := Qcow2InitialSize(181*gib, 300*gib)
	if got < 181*gib || got > 182*gib || got%(1<<20) != 0 {
		t.Fatalf("начальный размер %d: данные, метаданные qcow2 и запас, кратно МиБ", got)
	}
	// Данных почти на весь диск: не больше полного размера qcow2.
	full := Qcow2InitialSize(300*gib, 300*gib)
	if full < 300*gib || full > 301*gib {
		t.Fatalf("полный размер qcow2 для 300 ГиБ: %d", full)
	}
	if Qcow2InitialSize(400*gib, 300*gib) != full {
		t.Fatal("данных больше размера диска не бывает")
	}
	if small := Qcow2InitialSize(0, 10*gib); small <= 0 || small > gib {
		t.Fatalf("пустой диск: %d", small)
	}
}

func TestNewDiskLayout(t *testing.T) {
	cases := []struct {
		source, storage string
		converts        bool
		format          string
		sparse          bool
	}{
		{"cow", "nfs", true, "cow", true},
		{"raw", "nfs", true, "raw", true},
		{"raw", "iscsi", true, "cow", true}, // тонкого raw на блочном домене не бывает
		{"cow", "fcp", true, "cow", true},
		{"cow", "nfs", false, "raw", true},    // imageio 4.3 пишет поток в файл тома как есть
		{"cow", "iscsi", false, "raw", false}, // raw на блочном — только полный
		{"", "", true, "raw", true},
	}
	for _, tc := range cases {
		format, sparse := NewDiskLayout(tc.source, tc.storage, tc.converts)
		if format != tc.format || sparse != tc.sparse {
			t.Fatalf("NewDiskLayout(%q, %q, %v) = %s/%v, want %s/%v",
				tc.source, tc.storage, tc.converts, format, sparse, tc.format, tc.sparse)
		}
	}
	if !RestoreAllocatesFull(false, "iscsi") || RestoreAllocatesFull(true, "iscsi") || RestoreAllocatesFull(false, "nfs") {
		t.Fatal("полный размер занимает только raw на блочном домене движка без Backup API")
	}
}
