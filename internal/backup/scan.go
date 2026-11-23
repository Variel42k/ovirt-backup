package backup

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// RunManifestSHA256 Отпечатки пальцев точного портативного представления манифеста.
// Реплики с одним и тем же идентификатором запуска объединяются только тогда, когда это
// значение совпадает.
func RunManifestSHA256(doc *RunManifest) (string, error) {
	encoded, err := EncodeManifest(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:]), nil
}

// Чтение репозитория без базы данных сервиса.
//
// База ускоряет просмотр и хранит состояние расписаний. Каждый запуск записывает
// самодостаточный run.json рядом с данными, чтобы после аварии можно было перечислить,
// проверить и восстановить копии по объектам репозитория.

// StoredRun — одна резервная копия, найденная при сканировании хранилища.
type StoredRun struct {
	// Prefix — путь запуска в хранилище, оканчивающийся на «/».
	Prefix   string       `json:"prefix"`
	Manifest *RunManifest `json:"manifest"`
}

// ScanRepository перечисляет завершённые запуски под заданным префиксом репозитория.
//
// Каталог без run.json — запуск, который никогда не завершается; он пропущен, а не
// сообщается, потому что половина резервной копии не является точкой восстановления.
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

		manifest, err := ReadRunManifest(ctx, backend, obj.Key)
		if err != nil {
			// Один нечитаемый манифест не должен скрывать остальную часть архива.
			continue
		}
		runs = append(runs, StoredRun{Prefix: runPrefix, Manifest: manifest})
	}

	sort.Slice(runs, func(i, j int) bool {
		return runs[i].Manifest.CreatedAt.Before(runs[j].Manifest.CreatedAt)
	})
	return runs, nil
}

// ReadRunManifest загружает самоописывающийся документ запуска из хранилища.
func ReadRunManifest(ctx context.Context, backend repo.Backend, key string) (*RunManifest, error) {
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
	if doc.Version > FormatVersion {
		return nil, fmt.Errorf("манифест запуска версии %d новее поддерживаемой %d", doc.Version, FormatVersion)
	}
	return &doc, nil
}

// FindChain собирает цепь, от которой зависит сохраненный запуск, используя только то, что
// находится в хранилище.
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

	// Обернитесь в порядок от корня к листу, что и ожидает читатель.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	if root := chain[0].Manifest; root.Type != model.BackupFull && root.Type != model.BackupSnapshot {
		return nil, fmt.Errorf("корень цепочки %s имеет тип %q — полной точки нет",
			root.RunID, root.Type)
	}
	return chain, nil
}

// LatestUsable находит новейший запуск ВМ, на котором может основываться новый инкремент:
// он должен нести checkpoint и его цепь должна быть неповрежденной.
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
		// Все, чья родословная сломана, также не может служить основой.
		if _, err := FindChain(runs, m.RunID); err != nil {
			continue
		}
		if !found || m.CreatedAt.After(best.Manifest.CreatedAt) {
			best, found = r, true
		}
	}
	return best, found
}

// LoadChainManifests считывает манифесты на диск цепочки, сгруппированные по диску.
func LoadChainManifests(ctx context.Context, backend repo.Backend, chain []StoredRun) (map[string][]*DiskManifest, []string, error) {
	byDisk := map[string][]*DiskManifest{}
	var order []string

	// Восстановимыми являются только диски, присутствующие в новейшей ссылке: диск,
	// отсоединенный до этого момента, не имеет текущей версии.
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

// WriteRunManifest публикует документ уровня выполнения, который помечает резервную копию
// как завершенную в хранилище.
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
