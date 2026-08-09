package config

import (
	"strings"
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

// Подключение задаётся одной строкой в любой из двух форм.
func TestDatabaseURLAccepted(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{"URL", "postgres://u:p@db:5432/jhvirt?sslmode=require"},
		{"URL postgresql://", "postgresql://u:p@db:5432/jhvirt"},
		{"форма ключ=значение", "host=db port=5432 user=u password=p dbname=jhvirt sslmode=disable"},
		// Ради этого случая форма key=value и принимается: openssl rand -base64
		// выдаёт / и +, а в URL их пришлось бы percent-кодировать.
		{"пароль со слешем", "host=db user=u password=aB/c+d= dbname=jhvirt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("JHV_DATABASE_URL", tt.dsn)

			cfg, err := Load("")
			if err != nil {
				t.Fatalf("загрузка: %v", err)
			}
			if got := cfg.Database.Postgres.DSN(); got != tt.dsn {
				t.Fatalf("DSN = %q, ожидалась исходная строка", got)
			}
		})
	}
}

// SQLite должен отвергаться внятно, а не «неизвестным форматом»: у людей есть
// установки на нём, и им нужно объяснение, а не недоумение.
func TestDatabaseURLRejectsSQLite(t *testing.T) {
	t.Setenv("JHV_DATABASE_URL", "sqlite:///var/lib/jhvirt/jhvirt.db")

	_, err := Load("")
	if err == nil {
		t.Fatal("sqlite принят молча")
	}
	if !strings.Contains(err.Error(), "SQLite больше не поддерживается") {
		t.Fatalf("ошибка не объясняет причину: %v", err)
	}
	if !strings.Contains(err.Error(), "jvbackup") {
		t.Fatalf("ошибка не говорит, что копии не потеряны: %v", err)
	}
}

func TestDatabaseURLRejectsGarbage(t *testing.T) {
	t.Setenv("JHV_DATABASE_URL", "mysql://u:p@db/jhvirt")

	if _, err := Load(""); err == nil {
		t.Fatal("незнакомый формат принят молча")
	}
}

// Без подключения сервису делать нечего, и узнать об этом надо при разборе
// конфигурации, а не при первом запросе.
func TestValidateRequiresConnection(t *testing.T) {
	c := Config{}
	c.Backup = BackupConfig{ChunkSize: 65536, Workers: 1, Compression: "none"}
	c.Server.Port = 8080
	c.Monitor.Interval = 30_000_000_000
	c.Scheduler.Timezone = "UTC"

	if err := c.Validate(); err == nil {
		t.Fatal("пустое подключение принято")
	}
}

// Пароль не должен попадать в журнал, а адрес — должен остаться: строка в
// журнале существует ровно затем, чтобы видеть, куда подключились.
func TestTargetHidesPassword(t *testing.T) {
	tests := []struct {
		name   string
		dsn    string
		secret string
		keep   string
	}{
		{"URL", "postgres://jhvirt:s3cr3t@db:5432/jhvirt", "s3cr3t", "db:5432/jhvirt"},
		{"пароль с @", "postgres://jhvirt:p@ss@db:5432/jhvirt", "p@ss", "db:5432/jhvirt"},
		{"пароль не percent-кодирован", "postgres://jhvirt:очень-секретный@db:5432/jhvirt", "очень-секретный", "db:5432/jhvirt"},
		{"форма ключ=значение", "host=db user=jhvirt password=s3cr3t dbname=jhvirt", "s3cr3t", "host=db"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DatabaseConfig{Postgres: PostgresConfig{URL: tt.dsn}}
			got := d.Target()

			if strings.Contains(got, tt.secret) {
				t.Fatalf("пароль виден в строке для журнала: %s", got)
			}
			if !strings.Contains(got, tt.keep) {
				t.Fatalf("строка потеряла адрес подключения: %s", got)
			}
		})
	}
}

// Без учётных данных прятать нечего, строка должна остаться как есть.
func TestTargetKeepsDSNWithoutCredentials(t *testing.T) {
	d := DatabaseConfig{Postgres: PostgresConfig{URL: "postgres://db:5432/jhvirt"}}
	if got := d.Target(); got != "postgres://db:5432/jhvirt" {
		t.Fatalf("строка изменилась без нужды: %s", got)
	}
}

// Переменная окружения должна перекрывать значение из файла — на этом стоит
// вся настройка контейнера.
func TestEnvOverridesFile(t *testing.T) {
	t.Setenv("JHV_BACKUP_WORKERS", "7")
	t.Setenv("JHV_DATABASE_POSTGRES_HOST", "db.example.org")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	if cfg.Backup.Workers != 7 {
		t.Fatalf("workers из окружения не подхватился: %d", cfg.Backup.Workers)
	}
	if cfg.Database.Postgres.Host != "db.example.org" {
		t.Fatalf("хост из окружения не подхватился: %q", cfg.Database.Postgres.Host)
	}
}
