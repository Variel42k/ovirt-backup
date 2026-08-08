package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
)

// Reading a repository without the service's database.
//
// The database is a convenience: it makes listing fast and carries scheduling
// state. It is not the source of truth. Every run writes a self-describing
// run.json next to its data, so a repository can be enumerated, verified and
// restored from with nothing but the objects themselves — which is exactly the
// situation a disaster recovery finds itself in.

// StoredRun is one backup found by scanning a repository.
type StoredRun struct {
	// Prefix — путь запуска в хранилище, оканчивающийся на «/».
	Prefix   string       `json:"prefix"`
	Manifest *RunManifest `json:"manifest"`
}

// ScanRepository enumerates completed runs under a prefix.
//
// A directory without run.json is a run that never finished; it is skipped
// rather than reported, because half a backup is not a restore point.
func ScanRepository(ctx context.Context, backend repo.Backend, prefix string) ([]StoredRun, error) {
	if prefix == "" {
		prefix = repo.Root + "/"
	}

	objects, err := backend.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("перечисление %s: %w", prefix, err)
	}

	var runs []StoredRun
	for _, obj := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(obj.Key, "/run.json") {
			continue
		}
		runPrefix := strings.TrimSuffix(obj.Key, "run.json")

		manifest, err := readRunManifest(ctx, backend, obj.Key)
		if err != nil {
			// One unreadable manifest must not hide the rest of the archive.
			continue
		}
		runs = append(runs, StoredRun{Prefix: runPrefix, Manifest: manifest})
	}

	sort.Slice(runs, func(i, j int) bool {
		return runs[i].Manifest.CreatedAt.Before(runs[j].Manifest.CreatedAt)
	})
	return runs, nil
}

func readRunManifest(ctx context.Context, backend repo.Backend, key string) (*RunManifest, error) {
	rc, err := backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	var doc RunManifest
	if err := DecodeManifest(rc, &doc); err != nil {
		return nil, err
	}
	if doc.Format != FormatName {
		return nil, fmt.Errorf("чужой формат манифеста в %s: %q", key, doc.Format)
	}
	return &doc, nil
}

// FindChain assembles the chain a stored run depends on, using only what is in
// the repository.
func FindChain(runs []StoredRun, leafRunID string) ([]StoredRun, error) {
	byID := make(map[string]StoredRun, len(runs))
	for _, r := range runs {
		byID[r.Manifest.RunID] = r
	}

	leaf, ok := byID[leafRunID]
	if !ok {
		return nil, fmt.Errorf("бэкап %s не найден в хранилище", leafRunID)
	}

	var chain []StoredRun
	seen := map[string]bool{}
	cur := leaf
	for {
		if seen[cur.Manifest.RunID] {
			return nil, fmt.Errorf("цепочка повреждена: обнаружен цикл на %s", cur.Manifest.RunID)
		}
		seen[cur.Manifest.RunID] = true
		chain = append(chain, cur)

		parentID := cur.Manifest.ParentRunID
		if parentID == "" {
			break
		}
		parent, ok := byID[parentID]
		if !ok {
			return nil, fmt.Errorf("в хранилище нет бэкапа %s, от которого зависит %s",
				parentID, cur.Manifest.RunID)
		}
		cur = parent
	}

	// Reverse into root-to-leaf order, which is what the reader expects.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	if root := chain[0].Manifest; root.Type != model.BackupFull && root.Type != model.BackupSnapshot {
		return nil, fmt.Errorf("корень цепочки %s имеет тип %q — полной точки нет",
			root.RunID, root.Type)
	}
	return chain, nil
}

// LatestUsable finds the newest run for a VM that a new incremental can be
// based on: it must carry a checkpoint and its chain must be intact.
func LatestUsable(runs []StoredRun, vmID string, onlyFull bool) (StoredRun, bool) {
	var best StoredRun
	found := false

	for _, r := range runs {
		m := r.Manifest
		if m.VMID != vmID && m.VMName != vmID {
			continue
		}
		if m.ToCheckpointID == "" {
			continue
		}
		if onlyFull && m.Type != model.BackupFull {
			continue
		}
		// Anything whose ancestry is broken cannot serve as a base either.
		if _, err := FindChain(runs, m.RunID); err != nil {
			continue
		}
		if !found || m.CreatedAt.After(best.Manifest.CreatedAt) {
			best, found = r, true
		}
	}
	return best, found
}

// LoadChainManifests reads the per-disk manifests of a chain, grouped by disk.
func LoadChainManifests(ctx context.Context, backend repo.Backend, chain []StoredRun) (map[string][]*DiskManifest, []string, error) {
	byDisk := map[string][]*DiskManifest{}
	var order []string

	// Only disks present in the newest link are restorable: a disk detached
	// before this point has no current version.
	leaf := chain[len(chain)-1]
	present := map[string]bool{}
	for _, d := range leaf.Manifest.Disks {
		present[d.DiskID] = true
		order = append(order, d.DiskID)
	}

	for _, run := range chain {
		for _, disk := range run.Manifest.Disks {
			if !present[disk.DiskID] {
				continue
			}
			key := disk.ManifestKey
			if key == "" {
				key = run.Prefix + strings.TrimPrefix(disk.DataKey, run.Prefix)
			}
			m, err := loadDiskManifest(ctx, backend, key)
			if err != nil {
				return nil, nil, err
			}
			byDisk[disk.DiskID] = append(byDisk[disk.DiskID], m)
		}
	}

	for _, diskID := range order {
		if len(byDisk[diskID]) == 0 {
			return nil, nil, fmt.Errorf("для диска %s не найдено ни одного манифеста", diskID)
		}
	}
	return byDisk, order, nil
}

// WriteRunManifest publishes the run-level document, which is what marks a
// backup as complete in the repository.
func WriteRunManifest(ctx context.Context, backend repo.Backend, prefix string, doc *RunManifest) error {
	doc.Format = FormatName
	doc.Version = FormatVersion

	encoded, err := EncodeManifest(doc)
	if err != nil {
		return err
	}
	_, err = backend.Put(ctx, repo.RunManifestKey(prefix), bytesReader(encoded), int64(len(encoded)))
	return err
}
