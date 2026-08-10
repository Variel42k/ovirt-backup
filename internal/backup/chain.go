package backup

import (
	"context"
	"fmt"
	"slices"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/store"
)

// ChainSet is everything needed to read one backup point: the chain of runs it
// depends on, and the per-disk manifests in root-to-leaf order.
type ChainSet struct {
	// Leaf — запрошенная точка восстановления.
	Leaf *model.BackupRun
	// Runs — цепочка от полного бэкапа к запрошенной точке.
	Runs []*model.BackupRun
	// Manifests[diskID] — манифесты этого диска по цепочке, от корня к листу.
	Manifests map[string][]*DiskManifest
	// DiskOrder сохраняет порядок подключения дисков к ВМ.
	DiskOrder []string
	// RunManifest carries the portable VM profile for a real boot test. It is
	// optional so backups created before profiles were introduced remain
	// restorable.
	RunManifest      *RunManifest
	RunManifestError error
	Target           *model.StorageTarget
	Backend          repo.Backend
}

// Close releases the repository connection.
func (c *ChainSet) Close() {
	if c.Backend != nil {
		_ = c.Backend.Close()
	}
}

// DiskAliases maps disk ids to their human names for presentation.
func (c *ChainSet) DiskAliases() map[string]string {
	out := map[string]string{}
	for id, chain := range c.Manifests {
		if len(chain) > 0 {
			out[id] = chain[len(chain)-1].Alias
		}
	}
	return out
}

// LoadChain resolves a restore point.
//
// The chain is walked through parent links rather than through the chain id,
// because a chain id groups runs while a parent link orders them — and order
// is what decides which version of a chunk wins.
func (e *Engine) LoadChain(ctx context.Context, runID string) (*ChainSet, error) {
	leaf, err := e.store.GetBackupRunFull(ctx, runID)
	if err != nil {
		return nil, err
	}
	if leaf.Deleted {
		return nil, fmt.Errorf("данные бэкапа %s уже удалены из хранилища", runID)
	}
	if leaf.Type == model.BackupOVA {
		return nil, fmt.Errorf("бэкап типа OVA хранится на хосте гипервизора и не восстанавливается этим механизмом")
	}
	if leaf.Type == model.BackupConfig {
		return nil, fmt.Errorf("бэкап типа «только конфигурация» не содержит данных дисков")
	}

	// Walk up to the chain root, guarding against a cycle: a corrupted parent
	// link must not turn a restore into an infinite loop.
	var runs []*model.BackupRun
	seen := map[string]bool{}
	cur := leaf
	for {
		if seen[cur.ID] {
			return nil, fmt.Errorf("цепочка бэкапов повреждена: обнаружен цикл на %s", cur.ID)
		}
		seen[cur.ID] = true
		runs = append(runs, cur)

		if cur.ParentRunID == "" {
			break
		}
		parent, err := e.store.GetBackupRunFull(ctx, cur.ParentRunID)
		if err == store.ErrNotFound {
			return nil, fmt.Errorf("в цепочке отсутствует бэкап %s, от которого зависит %s",
				cur.ParentRunID, cur.ID)
		}
		if err != nil {
			return nil, err
		}
		if parent.Deleted {
			return nil, fmt.Errorf("бэкап %s удалён, но от него зависит %s — восстановление невозможно",
				parent.ID, cur.ID)
		}
		cur = parent
	}
	slices.Reverse(runs) // теперь от корня к листу

	if root := runs[0]; root.Type != model.BackupFull && root.Type != model.BackupSnapshot {
		return nil, fmt.Errorf("корень цепочки %s имеет тип %q — полной точки для восстановления нет",
			root.ID, root.Type)
	}

	target, err := e.store.GetStorageTarget(ctx, leaf.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("хранилище бэкапа: %w", err)
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("открытие хранилища %q: %w", target.Name, err)
	}

	set := &ChainSet{
		Leaf:      leaf,
		Runs:      runs,
		Manifests: map[string][]*DiskManifest{},
		Target:    target,
		Backend:   backend,
	}
	if doc, manifestErr := ReadRunManifest(ctx, backend, repo.RunManifestKey(leaf.RepoPath)); manifestErr == nil {
		set.RunManifest = doc
	} else {
		set.RunManifestError = manifestErr
	}

	// Only disks present in the leaf are restorable: a disk detached before
	// this point has no current version to restore.
	for _, d := range leaf.Disks {
		if d.Status != model.RunSucceeded {
			continue
		}
		set.DiskOrder = append(set.DiskOrder, d.DiskID)
	}

	for _, run := range runs {
		for _, d := range run.Disks {
			if d.Status != model.RunSucceeded || d.ManifestKey == "" {
				continue
			}
			if !slices.Contains(set.DiskOrder, d.DiskID) {
				continue
			}
			m, err := loadDiskManifest(ctx, backend, d.ManifestKey)
			if err != nil {
				backend.Close()
				return nil, err
			}
			set.Manifests[d.DiskID] = append(set.Manifests[d.DiskID], m)
		}
	}

	for _, diskID := range set.DiskOrder {
		chain := set.Manifests[diskID]
		if len(chain) == 0 {
			backend.Close()
			return nil, fmt.Errorf("для диска %s не найдено ни одного манифеста в цепочке", diskID)
		}
		if root := chain[0]; root.Type == model.BackupIncremental || root.Type == model.BackupDifferential {
			backend.Close()
			return nil, fmt.Errorf("для диска %s цепочка начинается с инкремента %s — базовый бэкап потерян",
				diskID, root.RunID)
		}
	}
	if len(set.DiskOrder) == 0 {
		backend.Close()
		return nil, fmt.Errorf("в бэкапе %s нет успешно сохранённых дисков", runID)
	}

	return set, nil
}

// ReaderFor builds a chain reader for one disk of the restore point.
func (e *Engine) ReaderFor(set *ChainSet, diskID string) (*ChainReader, error) {
	chain, ok := set.Manifests[diskID]
	if !ok {
		return nil, fmt.Errorf("диск %s отсутствует в бэкапе %s", diskID, set.Leaf.ID)
	}
	return NewChainReader(set.Backend, e.cipher, chain)
}
