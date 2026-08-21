package backup

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// The scanning path is what disaster recovery depends on: no database, no
// service, just the objects. This test builds a real two-link chain in a real
// repository and reads it back the way the CLI does.

func writeStoredRun(t *testing.T, backend repo.Backend, prefix string, doc RunManifest,
	disks map[string]map[int64][]byte, chunkSize int64) {
	t.Helper()
	ctx := context.Background()

	for diskID, chunks := range disks {
		manifestKey := repo.DiskManifestKey(prefix, 0, diskID)
		dataKey := repo.DiskDataKey(prefix, 0, diskID)

		m := &DiskManifest{
			RunID:            doc.RunID,
			ChainID:          doc.ChainID,
			ParentRunID:      doc.ParentRunID,
			ChainIndex:       doc.ChainIndex,
			Type:             doc.Type,
			VMID:             doc.VMID,
			VMName:           doc.VMName,
			DiskID:           diskID,
			Alias:            diskID,
			VirtualSize:      testVirtualSize,
			FromCheckpointID: doc.FromCheckpointID,
			ToCheckpointID:   doc.ToCheckpointID,
		}
		w, err := NewDiskWriter(ctx, m, WriterOptions{
			Backend: backend, DataKey: dataKey, ChunkSize: chunkSize, Compression: CompressionZstd,
		})
		if err != nil {
			t.Fatalf("writer: %v", err)
		}

		indices := make([]int64, 0, len(chunks))
		for i := range chunks {
			indices = append(indices, i)
		}
		for i := 0; i < len(indices); i++ {
			for j := i + 1; j < len(indices); j++ {
				if indices[j] < indices[i] {
					indices[i], indices[j] = indices[j], indices[i]
				}
			}
		}
		for _, i := range indices {
			if err := w.WriteChunk(i, chunks[i]); err != nil {
				t.Fatalf("write chunk: %v", err)
			}
		}
		final, err := w.Close()
		if err != nil {
			t.Fatalf("close: %v", err)
		}

		encoded, err := EncodeManifest(final)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if _, err := backend.Put(ctx, manifestKey, bytes.NewReader(encoded), int64(len(encoded))); err != nil {
			t.Fatalf("put manifest: %v", err)
		}

		doc.Disks = append(doc.Disks, RunManifestDisk{
			DiskID: diskID, Alias: diskID, Index: 0,
			VirtualSize: testVirtualSize,
			ManifestKey: manifestKey, DataKey: dataKey,
			ChunkCount: final.ChunkCount(), StoredBytes: final.StoredBytes,
			DataSHA256: final.DataSHA256,
		})
	}

	if err := WriteRunManifest(ctx, backend, prefix, &doc); err != nil {
		t.Fatalf("write run manifest: %v", err)
	}
}

func TestScanRepositoryFindsChain(t *testing.T) {
	ctx := context.Background()
	backend := testBackend(t)
	base := time.Date(2026, 8, 4, 1, 0, 0, 0, time.UTC)

	fullPrefix := repo.RunPrefix("kvm1", "db-01", "db-01", base, "run-full")
	writeStoredRun(t, backend, fullPrefix, RunManifest{
		RunID: "run-full", ChainID: "run-full", Type: model.BackupFull,
		ServerName: "kvm1", VMID: "db-01", VMName: "db-01",
		ToCheckpointID: "cp-full", CreatedAt: base,
	}, map[string]map[int64][]byte{
		"vda": {0: pattern('A', testChunkSize), 3: pattern('C', testChunkSize)},
	}, testChunkSize)

	incTime := base.Add(24 * time.Hour)
	incPrefix := repo.RunPrefix("kvm1", "db-01", "db-01", incTime, "run-inc")
	writeStoredRun(t, backend, incPrefix, RunManifest{
		RunID: "run-inc", ChainID: "run-full", ParentRunID: "run-full", ChainIndex: 1,
		Type: model.BackupIncremental, ServerName: "kvm1", VMID: "db-01", VMName: "db-01",
		FromCheckpointID: "cp-full", ToCheckpointID: "cp-inc", CreatedAt: incTime,
	}, map[string]map[int64][]byte{
		"vda": {3: pattern('X', testChunkSize), 5: pattern('Y', testChunkSize)},
	}, testChunkSize)

	runs, err := ScanRepository(ctx, backend, "")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("найдено %d запусков, ожидалось 2", len(runs))
	}
	// Отсортировано от старых к новым.
	if runs[0].Manifest.RunID != "run-full" {
		t.Errorf("порядок нарушен: %s первым", runs[0].Manifest.RunID)
	}

	chain, err := FindChain(runs, "run-inc")
	if err != nil {
		t.Fatalf("FindChain: %v", err)
	}
	if len(chain) != 2 || chain[0].Manifest.RunID != "run-full" {
		t.Fatalf("цепочка собрана неверно: %+v", chain)
	}

	byDisk, order, err := LoadChainManifests(ctx, backend, chain)
	if err != nil {
		t.Fatalf("LoadChainManifests: %v", err)
	}
	if len(order) != 1 || order[0] != "vda" {
		t.Fatalf("порядок дисков: %v", order)
	}

	reader, err := NewChainReader(backend, nil, byDisk["vda"])
	if err != nil {
		t.Fatalf("NewChainReader: %v", err)
	}
	defer reader.Close()

	// Слияние: чанк 3 перезаписан инкрементом, чанк 0 остался из полного,
	// чанк 5 добавлен инкрементом, остальное — нули.
	cases := map[int64][]byte{
		0: pattern('A', testChunkSize),
		3: pattern('X', testChunkSize),
		5: pattern('Y', testChunkSize),
	}
	for index, want := range cases {
		got, err := reader.ReadChunk(ctx, index)
		if err != nil {
			t.Fatalf("чанк %d: %v", index, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("чанк %d собран неверно", index)
		}
	}
	if got, err := reader.ReadChunk(ctx, 1); err != nil || got != nil {
		t.Errorf("чанк 1 должен читаться как нули, получено %v (%v)", got != nil, err)
	}
}

func TestFindChainRejectsBrokenAncestry(t *testing.T) {
	ctx := context.Background()
	backend := testBackend(t)
	base := time.Date(2026, 8, 4, 1, 0, 0, 0, time.UTC)

	// Инкремент есть, а полного бэкапа в хранилище нет: восстановить нечего,
	// и узнать об этом надо до начала восстановления, а не в середине.
	incPrefix := repo.RunPrefix("kvm1", "db-01", "db-01", base, "orphan")
	writeStoredRun(t, backend, incPrefix, RunManifest{
		RunID: "orphan", ChainID: "missing-parent", ParentRunID: "missing-parent", ChainIndex: 1,
		Type: model.BackupIncremental, ServerName: "kvm1", VMID: "db-01", VMName: "db-01",
		CreatedAt: base,
	}, map[string]map[int64][]byte{
		"vda": {0: pattern('Z', testChunkSize)},
	}, testChunkSize)

	runs, err := ScanRepository(ctx, backend, "")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if _, err := FindChain(runs, "orphan"); err == nil {
		t.Fatal("цепочка без корня должна отвергаться")
	}
}

func TestLatestUsableSkipsBrokenChains(t *testing.T) {
	ctx := context.Background()
	backend := testBackend(t)
	base := time.Date(2026, 8, 4, 1, 0, 0, 0, time.UTC)

	goodPrefix := repo.RunPrefix("kvm1", "db-01", "db-01", base, "good")
	writeStoredRun(t, backend, goodPrefix, RunManifest{
		RunID: "good", ChainID: "good", Type: model.BackupFull,
		ServerName: "kvm1", VMID: "db-01", VMName: "db-01",
		ToCheckpointID: "cp-good", CreatedAt: base,
	}, map[string]map[int64][]byte{"vda": {0: pattern('A', testChunkSize)}}, testChunkSize)

	// Более свежий, но осиротевший инкремент не должен становиться опорой.
	orphanPrefix := repo.RunPrefix("kvm1", "db-01", "db-01", base.Add(time.Hour), "orphan")
	writeStoredRun(t, backend, orphanPrefix, RunManifest{
		RunID: "orphan", ChainID: "gone", ParentRunID: "gone", ChainIndex: 1,
		Type: model.BackupIncremental, ServerName: "kvm1", VMID: "db-01", VMName: "db-01",
		ToCheckpointID: "cp-orphan", CreatedAt: base.Add(time.Hour),
	}, map[string]map[int64][]byte{"vda": {0: pattern('B', testChunkSize)}}, testChunkSize)

	runs, err := ScanRepository(ctx, backend, "")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}

	best, ok := LatestUsable(runs, "db-01", false)
	if !ok {
		t.Fatal("должна была найтись пригодная опора")
	}
	if best.Manifest.RunID != "good" {
		t.Errorf("опорой выбран %s, ожидался good", best.Manifest.RunID)
	}
}

func TestIncompleteRunIsIgnored(t *testing.T) {
	ctx := context.Background()
	backend := testBackend(t)

	// Данные есть, run.json нет: запуск не завершился, точкой восстановления
	// он не является.
	prefix := "jhvirt/kvm1/db-01/2026/08/04/half/"
	if _, err := backend.Put(ctx, prefix+"disk-00-vda.data",
		bytes.NewReader([]byte("partial")), 7); err != nil {
		t.Fatalf("put: %v", err)
	}

	runs, err := ScanRepository(ctx, backend, "")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("незавершённый запуск не должен попадать в список: %+v", runs)
	}
}
