package kvm

import (
	"testing"

	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/pkg/nbd"
)

const (
	testChunk = 1 << 20 // 1 МиБ
	testSize  = 16 * testChunk
)

func TestFullBackupSkipsHolesAndZeroes(t *testing.T) {
	extents := []nbd.Extent{
		{Offset: 0, Length: 2 * testChunk, Flags: 0},
		{Offset: 2 * testChunk, Length: 8 * testChunk, Flags: nbd.StateZero | nbd.StateHole},
		{Offset: 10 * testChunk, Length: 6 * testChunk, Flags: 0},
	}

	indices, coverage := SelectFromExtents(extents, false, testChunk, testSize)

	if coverage != 8*testChunk {
		t.Errorf("охват %d, ожидалось %d", coverage, 8*testChunk)
	}
	if len(indices) != 8 {
		t.Fatalf("выбрано %d чанков, ожидалось 8: %v", len(indices), indices)
	}
	for _, i := range indices {
		if i >= 2 && i < 10 {
			t.Errorf("чанк %d — дыра, копировать его не нужно", i)
		}
	}
}

// Ключевое правило: в инкрементальном бэкапе «отсутствует» означает
// «не изменилось». Область, которую гость обнулил, обязана попасть в копию
// явно, иначе восстановление вернёт старые данные из родителя.
func TestIncrementalStoresDirtyRegionsThatBecameZero(t *testing.T) {
	// qemu сообщает такую область одновременно как dirty (в битмапе) и как
	// дыру (в base:allocation). Мы смотрим только на битмап.
	extents := []nbd.Extent{
		{Offset: 0, Length: 4 * testChunk, Flags: 0},                          // не менялось
		{Offset: 4 * testChunk, Length: 2 * testChunk, Flags: nbd.StateDirty}, // изменено
		{Offset: 6 * testChunk, Length: 10 * testChunk, Flags: 0},             // не менялось
	}

	indices, coverage := SelectFromExtents(extents, true, testChunk, testSize)

	if coverage != 2*testChunk {
		t.Errorf("охват %d, ожидалось %d", coverage, 2*testChunk)
	}
	if len(indices) != 2 || indices[0] != 4 || indices[1] != 5 {
		t.Fatalf("выбраны чанки %v, ожидались [4 5]", indices)
	}

	// Тот же диапазон, но помеченный ещё и как дыра: он всё равно должен
	// попасть в копию, потому что изменился.
	discarded := []nbd.Extent{
		{Offset: 0, Length: 4 * testChunk, Flags: 0},
		{Offset: 4 * testChunk, Length: 2 * testChunk, Flags: nbd.StateDirty | nbd.StateZero | nbd.StateHole},
		{Offset: 6 * testChunk, Length: 10 * testChunk, Flags: 0},
	}
	indices2, coverage2 := SelectFromExtents(discarded, true, testChunk, testSize)
	if len(indices2) != 2 || coverage2 != 2*testChunk {
		t.Fatalf("обнулённая гостем область должна сохраняться явно, выбрано %v", indices2)
	}
}

func TestIncrementalIgnoresCleanRegions(t *testing.T) {
	extents := []nbd.Extent{
		{Offset: 0, Length: testSize, Flags: 0},
	}
	indices, coverage := SelectFromExtents(extents, true, testChunk, testSize)
	if len(indices) != 0 || coverage != 0 {
		t.Errorf("на неизменившемся диске копировать нечего, выбрано %v", indices)
	}
}

func TestUnalignedDirtyExtentPullsWholeChunks(t *testing.T) {
	// Один изменённый байт на границе чанков вытягивает оба чанка целиком:
	// восстановление работает по сетке, полчанка сохранить нельзя.
	extents := []nbd.Extent{
		{Offset: testChunk - 1, Length: 2, Flags: nbd.StateDirty},
	}
	indices, _ := SelectFromExtents(extents, true, testChunk, testSize)
	if len(indices) != 2 || indices[0] != 0 || indices[1] != 1 {
		t.Fatalf("выбрано %v, ожидались чанки [0 1]", indices)
	}
}

func TestExtentBeyondDiskIsClamped(t *testing.T) {
	extents := []nbd.Extent{
		{Offset: 0, Length: testSize * 2, Flags: 0},
	}
	indices, _ := SelectFromExtents(extents, false, testChunk, testSize)
	if len(indices) != 16 {
		t.Fatalf("выбрано %d чанков, диск вмещает 16", len(indices))
	}
}

func TestTailChunkIsShorterThanGrid(t *testing.T) {
	// Диск, не кратный размеру чанка: последний чанк короче.
	const odd = 3*testChunk + 512
	extents := []nbd.Extent{{Offset: 0, Length: odd, Flags: 0}}

	indices, coverage := SelectFromExtents(extents, false, testChunk, odd)
	if len(indices) != 4 {
		t.Fatalf("выбрано %d чанков, ожидалось 4", len(indices))
	}
	if coverage != odd {
		t.Errorf("охват %d, ожидалось %d", coverage, odd)
	}
}

func TestCBTReadinessOfDiskSet(t *testing.T) {
	qcow := libvirtx.Disk{Target: "vda", Device: "disk", Format: "qcow2"}
	raw := libvirtx.Disk{Target: "vdb", Device: "disk", Format: "raw"}

	if ready, _ := diskSetCBTReady([]libvirtx.Disk{qcow}); !ready {
		t.Error("набор только из qcow2 должен быть готов к инкрементам")
	}

	ready, blockers := diskSetCBTReady([]libvirtx.Disk{qcow, raw})
	if ready {
		t.Error("raw-диск в наборе делает инкрементальный бэкап невозможным")
	}
	if len(blockers) != 1 || blockers[0] != "vdb (raw)" {
		t.Errorf("мешающие диски: %v", blockers)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()

	if cfg.ScratchDir == "" {
		t.Error("каталог scratch должен иметь значение по умолчанию")
	}
	if cfg.ChunkSize <= 0 || cfg.ReadBatch <= 0 || cfg.MaxParallelDisks <= 0 {
		t.Errorf("некорректные значения по умолчанию: %+v", cfg)
	}
	// Заданные значения не должны затираться.
	custom := Config{ScratchDir: "/srv/scratch", MaxParallelDisks: 8}.withDefaults()
	if custom.ScratchDir != "/srv/scratch" || custom.MaxParallelDisks != 8 {
		t.Errorf("значения по умолчанию затёрли явную настройку: %+v", custom)
	}
}

// A raw disk cannot carry a persistent bitmap, so no checkpoint can be taken
// for a VM that has one. Asking libvirt for one anyway fails the whole backup —
// which is how a VM with raw disks ended up with no backup at all rather than a
// full one.
func TestNoCheckpointForRawDisks(t *testing.T) {
	plan := &Plan{
		Type: model.BackupFull,
		Disks: []libvirtx.Disk{
			{Target: "vda", Format: "raw", Device: "disk", Source: "/images/vda.img"},
		},
	}
	if cp := buildCheckpoint("jhv-1", "run-1", plan); cp != nil {
		t.Fatalf("для raw-диска checkpoint не должен создаваться, получено %+v", cp)
	}
}

// A VM whose disks are all qcow2 keeps its checkpoint: that is what makes the
// next run able to copy only what changed.
func TestCheckpointForQcowDisks(t *testing.T) {
	plan := &Plan{
		Type: model.BackupFull,
		Disks: []libvirtx.Disk{
			{Target: "vda", Format: "qcow2", Device: "disk", Source: "/images/vda.qcow2"},
			{Target: "vdb", Format: "qcow2", Device: "disk", Source: "/images/vdb.qcow2"},
		},
	}
	cp := buildCheckpoint("jhv-1", "run-1", plan)
	if cp == nil {
		t.Fatal("для qcow2-дисков checkpoint обязателен")
	}
	if got := cp.BitmapDisks(); len(got) != 2 {
		t.Errorf("битмапы у %v, ожидались оба диска", got)
	}
}

// Mixed VM: an increment could never cover it consistently, so no bitmaps are
// created at all. Bitmapping only the qcow2 disks would grow structures in
// their headers that no later run could use.
func TestMixedVMTakesNoCheckpoint(t *testing.T) {
	plan := &Plan{
		Type: model.BackupFull,
		Disks: []libvirtx.Disk{
			{Target: "vda", Format: "qcow2", Device: "disk", Source: "/images/vda.qcow2"},
			{Target: "vdb", Format: "raw", Device: "disk", Source: "/images/data.img"},
		},
	}
	if cp := buildCheckpoint("jhv-1", "run-1", plan); cp != nil {
		t.Error("смешанная ВМ не должна получать checkpoint")
	}
}

// The raw disks still take part in the backup: they are read hot through the
// same pull-mode export, just without a "what changed" shortcut.
func TestRawDisksStayInThePlan(t *testing.T) {
	disks := []libvirtx.Disk{
		{Target: "vda", Format: "raw", Device: "disk", Source: "/images/vda.img"},
		{Target: "vdb", Format: "qcow2", Device: "disk", Source: "/images/vdb.qcow2"},
	}
	for _, d := range disks {
		if !d.BackupCandidate() {
			t.Errorf("диск %s (%s) должен попадать в бэкап", d.Target, d.Format)
		}
	}
	if ready, blockers := diskSetCBTReady(disks); ready || len(blockers) != 1 {
		t.Errorf("ожидался ровно один блокер CBT, получено ready=%v blockers=%v", ready, blockers)
	}
}
