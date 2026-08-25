package config

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestProtectedFilesResolveSecrets(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Setenv("JHV_DATABASE_POSTGRES_HOST", "postgres")
	t.Setenv("JHV_DATABASE_POSTGRES_USER", "ovirt_backup_app")
	t.Setenv("JHV_DATABASE_POSTGRES_PASSWORD_FILE", write("database-password", "db-secret"))
	t.Setenv("JHV_AUTH_BOOTSTRAP_PASSWORD_FILE", write("bootstrap-password", "admin-secret"))
	t.Setenv("JHV_AUTH_OIDC_CLIENT_SECRET_FILE", write("oidc-secret", "oidc-value"))

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("загрузка: %v", err)
	}
	if cfg.Database.Postgres.Password != "db-secret" ||
		cfg.Auth.BootstrapPassword != "admin-secret" || cfg.Auth.OIDC.ClientSecret != "oidc-value" {
		t.Fatalf("секреты прочитаны неверно: db=%q admin=%q oidc=%q",
			cfg.Database.Postgres.Password, cfg.Auth.BootstrapPassword, cfg.Auth.OIDC.ClientSecret)
	}
}

func TestDatabaseURLFileAndDirectValueConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(path, []byte("host=postgres user=u dbname=jhvirt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o600)
	t.Setenv("JHV_DATABASE_URL", "postgres://u@postgres/jhvirt")
	t.Setenv("JHV_DATABASE_URL_FILE", path)

	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "одновременно") {
		t.Fatalf("конфликт прямого значения и файла не обнаружен: %v", err)
	}
}

func TestProtectedFileRejectsBroadPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(path, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JHV_DATABASE_POSTGRES_PASSWORD_FILE", path)

	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("слишком широкие права приняты: %v", err)
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

// Умолчание должно шифровать там, где сервер это умеет. Значение проверяется
// именно здесь, а не читается из документации: disable как умолчание дожил бы
// до боя незамеченным — установка с ним работает и выглядит исправной.
func TestDefaultSSLModeIsPrefer(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	if got := cfg.Database.Postgres.SSLMode; got != "prefer" {
		t.Fatalf("умолчание sslmode: %q, ожидалось prefer", got)
	}
}

func TestOIDCBackchannelURLMustBeAnOrigin(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	cfg.Auth.OIDC = OIDCConfig{
		Enabled:        true,
		Issuer:         "https://keycloak.example.org/realms/infra",
		BackchannelURL: "http://keycloak:8080/realms/infra",
		ClientID:       "jhvirt",
		RedirectURL:    "https://backup.example.org/api/v1/auth/oidc/callback",
		RoleMapping:    map[string]string{"admins": "admin"},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "backchannel_url") {
		t.Fatalf("внутренний адрес с путём принят: %v", err)
	}

	cfg.Auth.OIDC.BackchannelURL = "http://keycloak:8080"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("внутренний origin отклонён: %v", err)
	}
}

// validateWithDatabase прогоняет проверку конфигурации с заданным подключением
// и возвращает текст ошибки.
//
// Остальные поля заполнены допустимыми значениями, поэтому любая жалоба на
// sslmode приходит именно от проверки подключения, а не от соседней.
func validateWithDatabase(t *testing.T, db DatabaseConfig) string {
	t.Helper()
	c := Config{Database: db}
	c.Backup = BackupConfig{ChunkSize: 65536, Workers: 1, Compression: "none", CompressionLevel: 3}
	c.Server.Port = 8080
	c.Monitor.Interval = 30_000_000_000
	c.Scheduler.Timezone = "UTC"
	c.Logging = LoggingConfig{MaxSizeMB: 100, MaxBackups: 5, MaxAgeDays: 30}

	err := c.Validate()
	if err == nil {
		return ""
	}
	return err.Error()
}

// Отказ от TLS до удалённой базы — это пароль и данные по сети открытым
// текстом. Внутри машины и в сети контейнеров это допустимо, снаружи нет.
func TestSSLModeDisableAllowedOnlyLocally(t *testing.T) {
	tests := []struct {
		name     string
		db       DatabaseConfig
		rejected bool
	}{
		{"поля, localhost", DatabaseConfig{Postgres: PostgresConfig{
			Host: "localhost", SSLMode: "disable"}}, false},
		{"поля, петля по адресу", DatabaseConfig{Postgres: PostgresConfig{
			Host: "127.0.0.1", SSLMode: "disable"}}, false},
		{"поля, служба compose", DatabaseConfig{Postgres: PostgresConfig{
			Host: "postgres", SSLMode: "disable"}}, false},
		{"поля, чужой хост", DatabaseConfig{Postgres: PostgresConfig{
			Host: "db.example.org", SSLMode: "disable"}}, true},
		// Адрес из диапазона, отведённого стандартом под документацию
		// (RFC 5737). Настоящие адреса в примерах со временем расходятся с
		// действительностью и заодно рассказывают о чужой сети.
		{"поля, чужой адрес", DatabaseConfig{Postgres: PostgresConfig{
			Host: "203.0.113.10", SSLMode: "disable"}}, true},
		{"поля, чужой хост с prefer", DatabaseConfig{Postgres: PostgresConfig{
			Host: "db.example.org", SSLMode: "prefer"}}, false},

		{"URL, чужой хост", DatabaseConfig{Postgres: PostgresConfig{
			URL: "postgres://u:p@db.example.org:5432/jhvirt?sslmode=disable"}}, true},
		{"URL, чужой хост с require", DatabaseConfig{Postgres: PostgresConfig{
			URL: "postgres://u:p@db.example.org:5432/jhvirt?sslmode=require"}}, false},
		{"URL, служба compose", DatabaseConfig{Postgres: PostgresConfig{
			URL: "postgres://u:p@postgres:5432/jhvirt?sslmode=disable"}}, false},

		{"ключ=значение, чужой хост", DatabaseConfig{Postgres: PostgresConfig{
			URL: "host=db.example.org port=5432 user=u password=p dbname=jhvirt sslmode=disable"}}, true},
		{"ключ=значение, localhost", DatabaseConfig{Postgres: PostgresConfig{
			URL: "host=localhost port=5432 user=u password=p dbname=jhvirt sslmode=disable"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateWithDatabase(t, tt.db)
			complained := strings.Contains(got, "sslmode=disable")

			if tt.rejected && !complained {
				t.Fatalf("открытое подключение к удалённой базе принято, ошибка: %q", got)
			}
			if !tt.rejected && complained {
				t.Fatalf("подключение отвергнуто напрасно: %s", got)
			}
		})
	}
}

// Пути к сертификатам нужны для verify-ca и verify-full. Пустые значения
// дописывать нельзя: sslrootcert= — это не «умолчание», а имя файла из нуля
// символов, и подключение падает на попытке его открыть.
func TestDSNCarriesCertificatePathsOnlyWhenSet(t *testing.T) {
	base := PostgresConfig{
		Host: "db.example.org", Port: 5432, User: "u", Password: "p",
		Database: "jhvirt", SSLMode: "verify-full",
	}

	if got := base.DSN(); strings.Contains(got, "sslrootcert") {
		t.Fatalf("пустой путь к сертификату попал в строку подключения: %s", got)
	}

	base.SSLRootCert = "/app/data/tls/ca.crt"
	got := base.DSN()
	if !strings.Contains(got, "sslrootcert=/app/data/tls/ca.crt") {
		t.Fatalf("путь к корневому сертификату потерян: %s", got)
	}
	if strings.Contains(got, "sslcert=") || strings.Contains(got, "sslkey=") {
		t.Fatalf("незаданные пути дописаны: %s", got)
	}
}

// Управление включено по умолчанию: обновление не должно молча отобрать у
// оператора кнопку запуска ВМ. Небезопасное состояние объясняется в журнале, а
// не создаётся втихую обратное.
func TestManagementEnabledByDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	if !cfg.Management.Enabled {
		t.Fatal("управление должно быть включено по умолчанию")
	}
}

// Выключатель обязан читаться из окружения: в контейнере файла настроек может
// не быть вовсе, и настройка, доступная только через YAML, там недостижима.
func TestManagementDisabledFromEnv(t *testing.T) {
	t.Setenv("JHV_MANAGEMENT_ENABLED", "false")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	if cfg.Management.Enabled {
		t.Fatal("JHV_MANAGEMENT_ENABLED=false не выключил управление")
	}
}
