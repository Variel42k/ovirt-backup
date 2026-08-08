package dispatch

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/secret"
)

const (
	chunkSize   = 1 << 16
	virtualSize = chunkSize * 8
)

func testBackend(t *testing.T) repo.Backend {
	t.Helper()
	b, err := repo.Open(context.Background(), &model.StorageTarget{
		Name: "test", Kind: model.StorageLocal, BasePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func testCipher(t *testing.T) *secret.Cipher {
	t.Helper()
	c, err := secret.NewFromConfig(config.SecretsConfig{KeyFile: filepath.Join(t.TempDir(), "key")})
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

func fill(b byte) []byte {
	buf := make([]byte, chunkSize)
	for i := range buf {
		buf[i] = b
	}
	return buf
}

// writeChunks stores a run holding only the given chunk indices.
func writeChunks(t *testing.T, backend repo.Backend, key string, chunks map[int64][]byte) *backup.DiskManifest {
	t.Helper()

	m := backup.DiskManifest{
		DiskID: "vm:vda", Alias: "vda", VirtualSize: virtualSize,
	}
	w, err := backup.NewDiskWriter(context.Background(), &m, backup.WriterOptions{
		Backend: backend, DataKey: key, ChunkSize: chunkSize,
	})
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	for i := int64(0); i < virtualSize/chunkSize; i++ {
		data, ok := chunks[i]
		if !ok {
			continue
		}
		if err := w.WriteChunk(i, data); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	final, err := w.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	return final
}

// A sparse disk must reach the hypervisor at its full logical length: dd writes
// the stream sequentially, so a skipped hole would move everything after it.
func TestWriteImageKeepsHolesInPlace(t *testing.T) {
	backend := testBackend(t)

	// Chunks 0 and 5 hold data; everything between and after them is a hole.
	manifest := writeChunks(t, backend, "runs/a/vda.data", map[int64][]byte{
		0: fill(0xA1),
		5: fill(0xB2),
	})

	reader, err := backup.NewChainReader(backend, testCipher(t), []*backup.DiskManifest{manifest})
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer reader.Close()

	var buf bytes.Buffer
	written, err := writeImage(context.Background(), reader, &buf, nil)
	if err != nil {
		t.Fatalf("writeImage: %v", err)
	}

	if written != virtualSize {
		t.Fatalf("записано %d байт, а логический размер диска %d", written, virtualSize)
	}
	if buf.Len() != virtualSize {
		t.Fatalf("длина образа %d, ожидалось %d", buf.Len(), virtualSize)
	}

	img := buf.Bytes()
	if !bytes.Equal(img[:chunkSize], fill(0xA1)) {
		t.Error("первый чанк потерян")
	}
	// The point of the test: chunk 5 has to sit at offset 5*chunkSize, not
	// right after chunk 0.
	if !bytes.Equal(img[5*chunkSize:6*chunkSize], fill(0xB2)) {
		t.Error("чанк 5 оказался не на своём смещении — дыры сдвинули образ")
	}
	for _, hole := range []int64{1, 2, 3, 4, 6, 7} {
		region := img[hole*chunkSize : (hole+1)*chunkSize]
		if !bytes.Equal(region, make([]byte, chunkSize)) {
			t.Errorf("дыра в чанке %d заполнена не нулями", hole)
		}
	}
}

// The newest run in a chain must win, or the boot test would start an image
// assembled from stale data and blame the guest for it.
func TestWriteImageMergesChain(t *testing.T) {
	backend := testBackend(t)

	full := writeChunks(t, backend, "runs/a/vda.data", map[int64][]byte{
		0: fill(0x11), 1: fill(0x22),
	})
	incr := writeChunks(t, backend, "runs/b/vda.data", map[int64][]byte{
		1: fill(0x33),
	})

	reader, err := backup.NewChainReader(backend, testCipher(t), []*backup.DiskManifest{full, incr})
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer reader.Close()

	var buf bytes.Buffer
	if _, err := writeImage(context.Background(), reader, &buf, nil); err != nil {
		t.Fatalf("writeImage: %v", err)
	}

	img := buf.Bytes()
	if !bytes.Equal(img[:chunkSize], fill(0x11)) {
		t.Error("чанк 0 должен прийти из полного бэкапа")
	}
	if !bytes.Equal(img[chunkSize:2*chunkSize], fill(0x33)) {
		t.Error("чанк 1 должен прийти из инкремента, а не из полного бэкапа")
	}
}

func chainSet(disks ...*backup.DiskManifest) *backup.ChainSet {
	set := &backup.ChainSet{
		Leaf:      &model.BackupRun{ID: "run-1", VMName: "vm"},
		Manifests: map[string][]*backup.DiskManifest{},
	}
	for _, m := range disks {
		set.DiskOrder = append(set.DiskOrder, m.DiskID)
		set.Manifests[m.DiskID] = []*backup.DiskManifest{m}
	}
	return set
}

func TestSelectBootDiskPrefersBootable(t *testing.T) {
	set := chainSet(
		&backup.DiskManifest{DiskID: "vm:vdb", Alias: "data"},
		&backup.DiskManifest{DiskID: "vm:vda", Alias: "root", Bootable: true},
	)

	id, m, err := selectBootDisk(set, "")
	if err != nil {
		t.Fatalf("выбор диска: %v", err)
	}
	if id != "vm:vda" || m.Alias != "root" {
		t.Fatalf("выбран %s (%s), ожидался загрузочный vm:vda", id, m.Alias)
	}
}

func TestSelectBootDiskTakesTheOnlyDisk(t *testing.T) {
	set := chainSet(&backup.DiskManifest{DiskID: "vm:vda", Alias: "root"})

	id, _, err := selectBootDisk(set, "")
	if err != nil {
		t.Fatalf("единственный диск должен подойти без пометки bootable: %v", err)
	}
	if id != "vm:vda" {
		t.Fatalf("выбран %s", id)
	}
}

// Guessing here would produce a confident "не загрузилась" about a data volume
// that was never bootable in the first place.
func TestSelectBootDiskRefusesToGuess(t *testing.T) {
	set := chainSet(
		&backup.DiskManifest{DiskID: "vm:vdb", Alias: "data"},
		&backup.DiskManifest{DiskID: "vm:vdc", Alias: "logs"},
	)

	if _, _, err := selectBootDisk(set, ""); err == nil {
		t.Fatal("выбор из нескольких незагрузочных дисков должен быть отклонён")
	}
}

func TestSelectBootDiskRejectsUnknownDisk(t *testing.T) {
	set := chainSet(&backup.DiskManifest{DiskID: "vm:vda", Bootable: true})

	if _, _, err := selectBootDisk(set, "vm:missing"); err == nil {
		t.Fatal("несуществующий диск должен быть отклонён")
	}
}
