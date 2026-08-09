package config

import (
	"testing"
)

// В Docker вся настройка идёт переменными окружения, поэтому список каталогов
// восстановления должен задаваться ими же. Список — не строка, и разбор
// «через запятую» обеспечивает viper; тест фиксирует, что это действительно
// так, а не предположение из документации.
func TestRestoreDirsFromEnv(t *testing.T) {
	t.Setenv("JHV_BACKUP_RESTORE_DIRS", "/mnt/restore,/srv/restore")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}

	got := cfg.Backup.RestoreDirs
	if len(got) != 2 || got[0] != "/mnt/restore" || got[1] != "/srv/restore" {
		t.Fatalf("список каталогов разобран как %#v", got)
	}
}

// temp_dir входит в разрешённые корни всегда: иначе восстановление в файл не
// работало бы из коробки.
func TestRestoreRootsAlwaysIncludeTempDir(t *testing.T) {
	b := BackupConfig{TempDir: "/app/data/tmp", RestoreDirs: []string{"/mnt/restore"}}

	roots := b.RestoreRoots()
	if len(roots) != 2 || roots[0] != "/app/data/tmp" {
		t.Fatalf("temp_dir не первым в списке корней: %#v", roots)
	}

	empty := BackupConfig{TempDir: "/app/data/tmp"}
	if r := empty.RestoreRoots(); len(r) != 1 {
		t.Fatalf("без restore_dirs должен остаться один корень, получено %#v", r)
	}
}

// Переменная окружения должна перекрывать значение из файла — на этом стоит
// вся настройка контейнера.
func TestEnvOverridesFile(t *testing.T) {
	t.Setenv("JHV_BACKUP_WORKERS", "7")
	t.Setenv("JHV_DATABASE_DRIVER", "postgres")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	if cfg.Backup.Workers != 7 {
		t.Fatalf("workers из окружения не подхватился: %d", cfg.Backup.Workers)
	}
	if cfg.Database.Driver != "postgres" {
		t.Fatalf("driver из окружения не подхватился: %q", cfg.Database.Driver)
	}
}
