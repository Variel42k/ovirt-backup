package libvirtx

import (
	"strings"
	"testing"
)

const sampleDomainXML = `<domain type='kvm'>
  <name>db-01</name>
  <uuid>4a9b1f2e-0000-4000-8000-000000000001</uuid>
  <memory unit='KiB'>4194304</memory>
  <vcpu placement='static'>4</vcpu>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/db-01.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/var/lib/libvirt/images/db-01-data.raw'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/var/lib/libvirt/images/installer.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
    <disk type='block' device='disk'>
      <driver name='qemu' type='raw'/>
      <source dev='/dev/vg0/shared'/>
      <target dev='vdc' bus='virtio'/>
      <shareable/>
    </disk>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
    </channel>
  </devices>
</domain>`

func TestParseDomainXML(t *testing.T) {
	dom, err := ParseDomainXML(sampleDomainXML)
	if err != nil {
		t.Fatalf("ParseDomainXML: %v", err)
	}

	if dom.Name != "db-01" {
		t.Errorf("имя %q", dom.Name)
	}
	if dom.VCPUs != 4 {
		t.Errorf("vcpu %d, ожидалось 4", dom.VCPUs)
	}
	if dom.MemoryKiB != 4194304 {
		t.Errorf("память %d KiB", dom.MemoryKiB)
	}
	if !dom.GuestAgent {
		t.Error("канал гостевого агента объявлен, но не распознан")
	}
	if len(dom.Disks) != 4 {
		t.Fatalf("дисков %d, ожидалось 4", len(dom.Disks))
	}
}

func TestBackupDiskSelection(t *testing.T) {
	dom, err := ParseDomainXML(sampleDomainXML)
	if err != nil {
		t.Fatalf("ParseDomainXML: %v", err)
	}

	selected := dom.BackupDisks()
	if len(selected) != 2 {
		t.Fatalf("к бэкапу отобрано %d дисков, ожидалось 2 (vda и vdb): %+v", len(selected), selected)
	}
	targets := map[string]bool{}
	for _, d := range selected {
		targets[d.Target] = true
	}
	if !targets["vda"] || !targets["vdb"] {
		t.Errorf("отобраны не те диски: %v", targets)
	}

	// CD-ROM, общий диск — с объяснением, а не молча.
	for _, disk := range dom.Disks {
		switch disk.Target {
		case "sda":
			if disk.BackupCandidate() {
				t.Error("оптический привод не должен попадать в бэкап")
			}
			if disk.SkipReason() == "" {
				t.Error("для пропущенного привода нет объяснения")
			}
		case "vdc":
			if disk.BackupCandidate() {
				t.Error("общий диск не должен попадать в бэкап")
			}
			if !strings.Contains(disk.SkipReason(), "бщий") {
				t.Errorf("непонятное объяснение для общего диска: %q", disk.SkipReason())
			}
		}
	}
}

func TestCBTReadyReportsBlockingDisks(t *testing.T) {
	dom, err := ParseDomainXML(sampleDomainXML)
	if err != nil {
		t.Fatalf("ParseDomainXML: %v", err)
	}

	// vdb в формате raw — битмап там хранить негде, значит вся ВМ не готова
	// к инкрементальному бэкапу.
	ready, blockers := dom.CBTReady()
	if ready {
		t.Error("ВМ с raw-диском не может бэкапиться инкрементально")
	}
	if len(blockers) != 1 || !strings.Contains(blockers[0], "vdb") {
		t.Errorf("мешающие диски определены неверно: %v", blockers)
	}

	// Убираем raw-диск — ВМ становится пригодной.
	onlyQcow := `<domain type='kvm'><name>x</name><devices>
	  <disk type='file' device='disk'><driver name='qemu' type='qcow2'/>
	    <source file='/a.qcow2'/><target dev='vda' bus='virtio'/></disk>
	</devices></domain>`
	dom2, err := ParseDomainXML(onlyQcow)
	if err != nil {
		t.Fatalf("ParseDomainXML: %v", err)
	}
	if ready, blockers := dom2.CBTReady(); !ready {
		t.Errorf("ВМ только с qcow2-дисками должна быть готова: %v", blockers)
	}
}

func TestMemoryUnitNormalisation(t *testing.T) {
	cases := []struct {
		xml  string
		want int64
	}{
		{`<domain><name>a</name><memory unit='KiB'>1024</memory></domain>`, 1024},
		{`<domain><name>a</name><memory unit='MiB'>2</memory></domain>`, 2048},
		{`<domain><name>a</name><memory unit='GiB'>1</memory></domain>`, 1048576},
		{`<domain><name>a</name><memory unit='bytes'>2097152</memory></domain>`, 2048},
		{`<domain><name>a</name><memory>512</memory></domain>`, 512},
	}
	for _, tc := range cases {
		dom, err := ParseDomainXML(tc.xml)
		if err != nil {
			t.Fatalf("ParseDomainXML: %v", err)
		}
		if dom.MemoryKiB != tc.want {
			t.Errorf("для %s получено %d KiB, ожидалось %d", tc.xml, dom.MemoryKiB, tc.want)
		}
	}
}

func TestCheckpointXML(t *testing.T) {
	spec := CheckpointSpec{
		Name:        "jhv-abc123",
		Description: "полный бэкап",
		Disks:       []CheckpointDisk{{Target: "vda", Bitmap: true}, {Target: "vdb", Bitmap: true}},
	}
	out, err := spec.XML()
	if err != nil {
		t.Fatalf("XML: %v", err)
	}
	for _, want := range []string{
		"<name>jhv-abc123</name>",
		"<disk name='vda' checkpoint='bitmap'/>",
		"<disk name='vdb' checkpoint='bitmap'/>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в XML нет %q:\n%s", want, out)
		}
	}

	if _, err := (CheckpointSpec{}).XML(); err == nil {
		t.Error("checkpoint без имени должен отклоняться")
	}
}

// A raw disk cannot hold a persistent bitmap, and libvirt rejects the whole
// checkpoint — and with it the whole backup — if asked for one. Every disk of
// the backup has to be named explicitly, the raw ones as checkpoint='no'.
func TestCheckpointExcludesDisksWithoutBitmaps(t *testing.T) {
	spec := CheckpointSpec{
		Name: "jhv-mixed",
		Disks: []CheckpointDisk{
			{Target: "vda", Bitmap: true},
			{Target: "vdb", Bitmap: false},
		},
	}
	out, err := spec.XML()
	if err != nil {
		t.Fatalf("XML: %v", err)
	}
	if !strings.Contains(out, "<disk name='vda' checkpoint='bitmap'/>") {
		t.Errorf("qcow2-диск должен получить битмап:\n%s", out)
	}
	if !strings.Contains(out, "<disk name='vdb' checkpoint='no'/>") {
		t.Errorf("raw-диск должен быть исключён явно, а не умолчанием:\n%s", out)
	}
	if got := spec.BitmapDisks(); len(got) != 1 || got[0] != "vda" {
		t.Errorf("битмапы у %v, ожидался только vda", got)
	}
}

// A checkpoint where nothing can be bitmapped is not a checkpoint. Asking
// libvirt for one would fail; refusing early lets the caller fall back to a
// plain full backup, which is what a raw-only VM needs.
func TestCheckpointWithoutAnyBitmapIsRejected(t *testing.T) {
	spec := CheckpointSpec{
		Name:  "jhv-raw",
		Disks: []CheckpointDisk{{Target: "vda", Bitmap: false}},
	}
	if _, err := spec.XML(); err == nil {
		t.Error("checkpoint без единого битмапа должен отклоняться")
	}
}

func TestBackupXMLFull(t *testing.T) {
	spec := BackupSpec{
		SocketPath: "/var/lib/libvirt/qemu/jhv-run.sock",
		Disks: []BackupDiskSpec{
			{Target: "vda", ExportName: "vda", ExportBitmap: "jhv-vda", ScratchFile: "/scratch/vda"},
		},
	}
	out, err := spec.XML()
	if err != nil {
		t.Fatalf("XML: %v", err)
	}

	if !strings.Contains(out, "mode='pull'") {
		t.Error("бэкап должен быть в pull-режиме")
	}
	if strings.Contains(out, "<incremental>") {
		t.Error("полный бэкап не должен ссылаться на опорный checkpoint")
	}
	// Битмап без опорной точки бессмысленен: сравнивать не с чем.
	if strings.Contains(out, "exportbitmap") {
		t.Errorf("полный бэкап не должен экспортировать битмап:\n%s", out)
	}
	if !strings.Contains(out, "<scratch file='/scratch/vda'/>") {
		t.Errorf("нет scratch-файла:\n%s", out)
	}
}

func TestBackupXMLIncremental(t *testing.T) {
	spec := BackupSpec{
		SocketPath:  "/var/lib/libvirt/qemu/jhv-run.sock",
		Incremental: "jhv-parent",
		Disks: []BackupDiskSpec{
			{Target: "vda", ExportName: "vda", ExportBitmap: "jhv-vda", ScratchFile: "/scratch/vda"},
			{Target: "vdb", ExportName: "vdb", ExportBitmap: "jhv-vdb", ScratchFile: "/scratch/vdb"},
		},
	}
	out, err := spec.XML()
	if err != nil {
		t.Fatalf("XML: %v", err)
	}

	if !strings.Contains(out, "<incremental>jhv-parent</incremental>") {
		t.Errorf("нет опорного checkpoint:\n%s", out)
	}
	for _, want := range []string{
		"exportname='vda'", "exportbitmap='jhv-vda'",
		"exportname='vdb'", "exportbitmap='jhv-vdb'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в XML нет %q:\n%s", want, out)
		}
	}
}

func TestBackupXMLValidation(t *testing.T) {
	if _, err := (BackupSpec{Disks: []BackupDiskSpec{{Target: "vda"}}}).XML(); err == nil {
		t.Error("бэкап без сокета должен отклоняться")
	}
	if _, err := (BackupSpec{SocketPath: "/s"}).XML(); err == nil {
		t.Error("бэкап без дисков должен отклоняться")
	}
	// Диск без scratch-файла: libvirt примет XML, но бэкап упадёт уже в
	// процессе, когда гость начнёт писать. Лучше отказать сразу.
	_, err := (BackupSpec{
		SocketPath: "/s",
		Disks:      []BackupDiskSpec{{Target: "vda", ExportName: "vda"}},
	}).XML()
	if err == nil {
		t.Error("диск без scratch-файла должен отклоняться")
	}
}

func TestXMLEscaping(t *testing.T) {
	// Имя домена может содержать что угодно; подстановка без экранирования
	// сломала бы XML или позволила бы подменить его структуру.
	spec := CheckpointSpec{Name: "a&b<c>", Disks: []CheckpointDisk{{Target: "v'da", Bitmap: true}}}
	out, err := spec.XML()
	if err != nil {
		t.Fatalf("XML: %v", err)
	}
	if strings.Contains(out, "a&b<c>") {
		t.Errorf("специальные символы не экранированы:\n%s", out)
	}
	if !strings.Contains(out, "a&amp;b&lt;c&gt;") {
		t.Errorf("неверное экранирование:\n%s", out)
	}
}

func TestNameHelpers(t *testing.T) {
	runID := "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	if got := CheckpointName(runID); got != "jhv-3f2504e04f89" {
		t.Errorf("CheckpointName = %q", got)
	}
	if got := SocketPath("/var/lib/libvirt/qemu", runID); got != "/var/lib/libvirt/qemu/jhv-3f2504e04f89.sock" {
		t.Errorf("SocketPath = %q", got)
	}
	if got := ScratchPath("/scratch", runID, "vda"); got != "/scratch/jhv-3f2504e04f89-vda.scratch" {
		t.Errorf("ScratchPath = %q", got)
	}
	if got := BitmapName("vda"); got != "jhv-vda" {
		t.Errorf("BitmapName = %q", got)
	}
}

func TestStateMapping(t *testing.T) {
	cases := map[int32]State{
		1: StateRunning, 3: StatePaused, 5: StateShutOff, 6: StateCrashed, 99: StateUnknown,
	}
	for code, want := range cases {
		if got := stateFromCode(code); got != want {
			t.Errorf("код %d → %q, ожидалось %q", code, got, want)
		}
	}
	if !StateRunning.Running() || StatePaused.Running() {
		t.Error("Running() определяет состояние неверно")
	}
	if StatePaused.Title() == "" {
		t.Error("нет русской подписи для состояния")
	}
}
