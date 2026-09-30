package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// ensureTempBase returns the effective scratch directory and makes sure it
// exists. The explicit error names backup.temp_dir because a bare mkdir error
// after a snapshot has been created is too late and gives the operator no clue
// which host mount needs fixing.
func ensureTempBase(configured string) (string, error) {
	base := strings.TrimSpace(configured)
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", fmt.Errorf("backup.temp_dir %q нельзя создать: %w", base, err)
	}
	return base, nil
}

// checkTempWorkspace verifies actual write access by creating a directory.
// Mode bits alone are insufficient for bind mounts, NFS, SELinux and a
// container running under a numeric UID.
func checkTempWorkspace(configured string) error {
	base, err := ensureTempBase(configured)
	if err != nil {
		return err
	}
	probe, err := os.MkdirTemp(base, ".jhv-temp-probe-")
	if err != nil {
		return fmt.Errorf("backup.temp_dir %q недоступен для записи пользователю службы: %w", base, err)
	}
	if err := os.RemoveAll(probe); err != nil {
		return fmt.Errorf("backup.temp_dir %q: пробный каталог создан, но не удаляется: %w", base, err)
	}
	return nil
}

// Каталог сборки образа на движке без Backup API.
//
// Тома qcow2 скачиваются целиком и собираются локально: на время бэкапа
// нужно место под данные диска — во временном каталоге, а не в хранилище.
// Установщик по умолчанию кладёт backup.temp_dir на корневой раздел сервера,
// а он обычно мал: 142 ГБ данных ВМ туда не помещаются, хотя хранилище бэкапа
// на соседнем разделе почти пустое. Поэтому, если в backup.temp_dir места
// мало, образ собирается в служебной области локального хранилища, выбранного
// для бэкапа (<хранилище>/.jhvirt/tmp): туда же потом пишутся и данные. Если
// места нет и там, бэкап останавливается до заморозки гостя и снапшота.

// tempSpaceMargin — запас сверх оценки: сжатые слои и метаданные qcow2.
const tempSpaceMargin = 1 << 30

// legacyWorkspaceBase выбирает каталог для сборки образа объёмом need байт.
func (e *Engine) legacyWorkspaceBase(ctx context.Context, run *model.BackupRun, need int64) (string, error) {
	configured, err := ensureTempBase(e.cfg.TempDir)
	if err != nil {
		return "", err
	}
	free, _, statErr := repo.DiskUsage(configured)
	if statErr != nil || free >= need+tempSpaceMargin {
		// Размер не узнать — работаем как раньше: ошибка записи назовёт каталог.
		return configured, nil
	}
	reasons := []string{fmt.Sprintf("в backup.temp_dir %s свободно %s", configured, humanBytes(free))}
	if run != nil && run.StorageTargetID != "" && e.store != nil {
		if target, err := e.store.GetStorageTarget(ctx, run.StorageTargetID); err == nil &&
			target.Kind == model.StorageLocal && target.BasePath != "" {
			alt := filepath.Join(target.BasePath, ".jhvirt", "tmp")
			if checkTempWorkspace(alt) == nil {
				altFree, _, err := repo.DiskUsage(alt)
				// Туда же потом пишутся данные бэкапа: запас — ещё столько же.
				if err == nil && altFree >= 2*need+tempSpaceMargin {
					return alt, nil
				}
				reasons = append(reasons, fmt.Sprintf("в хранилище %s свободно %s", target.Name, humanBytes(altFree)))
			}
		}
	}
	return "", fmt.Errorf("для сборки образа нужно около %s во временном каталоге, а %s. Укажите "+
		"backup.temp_dir (JHV_BACKUP_TEMP_DIR) на разделе с местом — например, внутри раздела хранилища",
		humanBytes(need), strings.Join(reasons, ", "))
}

// legacyAssemblyNeed — сколько места займёт сборка образа диска: скачанные
// тома и собранный сырой образ (для тома raw без предков — только он).
func legacyAssemblyNeed(d ovirt.Disk, format string) int64 {
	data := d.ActualSize.Int64()
	if size := d.ProvisionedSize.Int64(); size > 0 && (data <= 0 || data > size) {
		data = size
	}
	if format == "cow" {
		return 2 * data
	}
	return data
}

func makeTempWorkspace(configured, pattern string) (string, error) {
	base, err := ensureTempBase(configured)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, pattern)
	if err != nil {
		return "", fmt.Errorf("backup.temp_dir %q недоступен для записи пользователю службы: %w", base, err)
	}
	return dir, nil
}
