package kvm

import (
	"encoding/xml"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
)

func TestBootTestDefaults(t *testing.T) {
	got := (BootTest{}).withDefaults()
	if got.MemoryMiB != 2048 || got.VCPUs != 2 || got.Timeout != 5*time.Minute ||
		got.Profile.Architecture != "x86_64" || got.Profile.Firmware != "bios" {
		t.Fatalf("неожиданные значения по умолчанию: %#v", got)
	}
}

func TestBootTestUsesProfileResourcesByDefault(t *testing.T) {
	got := (BootTest{Profile: &backup.VMProfile{
		MemoryMiB: 6144, VCPUs: 6, Architecture: "aarch64", Firmware: "efi",
	}}).withDefaults()
	if got.MemoryMiB != 6144 || got.VCPUs != 6 || got.Profile.Machine != "virt" {
		t.Fatalf("ресурсы исходной VM не применились: %#v", got)
	}
}

func TestBootTestDomainIsIsolatedAndValidXML(t *testing.T) {
	doc := bootTestDomainXML("verify<&", BootTest{
		Disks: []BootDisk{
			{RemoteImage: "/var/lib/libvirt/a'b<&.raw", Format: "raw", Target: "vda", Bus: "virtio", BootOrder: 1},
			{RemoteImage: "/var/lib/libvirt/data.raw", Format: "raw", Target: "sdb", Bus: "scsi"},
		},
		Profile:   &backup.VMProfile{Architecture: "x86_64", Machine: "q35", Firmware: "efi", SecureBoot: true},
		MemoryMiB: 2048, VCPUs: 2,
	})

	if strings.Contains(doc, "<interface") {
		t.Fatal("в проверочную VM добавлен сетевой интерфейс")
	}
	if !strings.Contains(doc, "listen='127.0.0.1'") {
		t.Fatal("графическая консоль доступна не только локально")
	}
	if strings.Contains(doc, "verify<&") || strings.Contains(doc, "a'b<&.raw") {
		t.Fatal("значения не экранированы в XML")
	}
	for _, want := range []string{
		"firmware='efi'", "name='secure-boot'", "name='enrolled-keys'", "machine='q35'",
		"target dev='vda' bus='virtio'", "target dev='sdb' bus='scsi'",
		"controller type='scsi'", "boot order='1'",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("в XML нет %q:\n%s", want, doc)
		}
	}
	var parsed any
	if err := xml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("libvirt XML не разбирается: %v\n%s", err, doc)
	}
	if validator, err := exec.LookPath("virt-xml-validate"); err == nil {
		file := t.TempDir() + "/domain.xml"
		if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command(validator, file, "domain").CombinedOutput(); err != nil {
			t.Fatalf("libvirt отверг XML: %v\n%s\n%s", err, output, doc)
		}
	}
}

func TestBootTestDomainUsesARMFeatures(t *testing.T) {
	doc := bootTestDomainXML("arm", BootTest{
		Disks:   []BootDisk{{RemoteImage: "/tmp/root.raw", Target: "vda", Bus: "virtio"}},
		Profile: &backup.VMProfile{Architecture: "aarch64", Firmware: "efi"},
	})
	if !strings.Contains(doc, "<gic version='3'/>") || strings.Contains(doc, "<apic/>") {
		t.Fatalf("неверные ARM features:\n%s", doc)
	}
}

func TestValidateBootTestRejectsDuplicateTargets(t *testing.T) {
	test := BootTest{Disks: []BootDisk{
		{RemoteImage: "/a", Format: "raw", Target: "vda", Bus: "virtio"},
		{RemoteImage: "/b", Format: "raw", Target: "vda", Bus: "virtio"},
	}}.withDefaults()
	if err := validateBootTest(test); err == nil || !strings.Contains(err.Error(), "vda") {
		t.Fatalf("дублирующий target принят: %v", err)
	}
}

func TestValidateBootTestRejectsDuplicateBootOrder(t *testing.T) {
	test := BootTest{Disks: []BootDisk{
		{RemoteImage: "/a", Format: "raw", Target: "vda", Bus: "virtio", BootOrder: 1},
		{RemoteImage: "/b", Format: "raw", Target: "vdb", Bus: "virtio", BootOrder: 1},
	}}.withDefaults()
	if err := validateBootTest(test); err == nil || !strings.Contains(err.Error(), "1") {
		t.Fatalf("повторяющийся boot order принят: %v", err)
	}
}

func TestShellQuoteHandlesApostrophe(t *testing.T) {
	if got, want := shellQuote("/tmp/a'b"), `'/tmp/a'\''b'`; got != want {
		t.Fatalf("shellQuote = %q, ожидалось %q", got, want)
	}
}

func TestParseAgentFilesystems(t *testing.T) {
	got := parseAgentFilesystems(`{"return":[
		{"name":"dm-1","mountpoint":"/var","type":"xfs","used-bytes":2048,"total-bytes":4096,"disk":[]},
		{"name":"vda2","mountpoint":"/","type":"ext4","used-bytes":10,"total-bytes":100,"disk":[]},
		{"name":"vda2","mountpoint":"/","type":"ext4","disk":[]},
		{"name":"vda1","mountpoint":"/boot","type":"ext4","disk":[]}]}`)
	if len(got) != 3 || got[0].Mountpoint != "/" || got[0].Device != "vda2" || got[0].UsedBytes != 10 ||
		got[0].TotalBytes != 100 || got[1].Mountpoint != "/boot" || got[1].TotalBytes != 0 ||
		got[2].Mountpoint != "/var" || got[2].Type != "xfs" {
		t.Fatalf("неожиданный список файловых систем: %+v", got)
	}
	if got := parseAgentFilesystems(`{"error":{"class":"CommandNotFound"}}`); len(got) != 0 {
		t.Fatalf("ответ без списка разобран в %+v", got)
	}
}
