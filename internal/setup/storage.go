package setup

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

type StorageRegistry interface {
	ListStorageTargets(context.Context) ([]*model.StorageTarget, error)
	CreateStorageTarget(context.Context, *model.StorageTarget) error
}

// RegisterBackupStorage only registers a local directory. It never imports,
// deletes, rewrites, or recursively changes permissions on repository objects.
// The operator reviews catalog entries in the web UI before importing them.
func RegisterBackupStorage(ctx context.Context, st StorageRegistry, directory string, readOnly bool, out io.Writer) error {
	if !filepath.IsAbs(directory) {
		return fmt.Errorf("каталог копий должен быть абсолютным")
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("каталог копий недоступен: %s", directory)
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("каталог копий: %w", err)
	}
	targets, err := st.ListStorageTargets(ctx)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if target.Kind != model.StorageLocal {
			continue
		}
		existing, err := filepath.EvalSymlinks(target.BasePath)
		if err == nil && existing == directory {
			fmt.Fprintf(out, "Хранилище уже зарегистрировано: %s (%s), путь %s; настройки сохранены\n", target.Name, target.ID, directory)
			return nil
		}
	}
	sum := sha256.Sum256([]byte(directory))
	name := fmt.Sprintf("Локальные копии (%x)", sum[:6])
	target := &model.StorageTarget{Name: name, Kind: model.StorageLocal,
		Enabled: true, ReadOnly: readOnly, BasePath: directory}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		return err
	}
	fmt.Fprintf(out, "Хранилище зарегистрировано: %s (%s), путь %s, только чтение: %t\n", target.Name, target.ID, directory, readOnly)
	if readOnly {
		fmt.Fprintln(out, "Для подключения точек: Хранилища → Каталог → Сканировать → Импортировать. Файлы не копируются и не удаляются.")
	}
	return nil
}
