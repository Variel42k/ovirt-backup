// Package config loads the service configuration from a YAML file and the
// environment. Every key can be overridden with a JHV_-prefixed variable where
// dots in the key path become underscores (JHV_DATABASE_POSTGRES_PASSWORD).
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const envPrefix = "JHV"

// Config is the fully resolved runtime configuration.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Auth      AuthConfig      `mapstructure:"auth"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Logging   LoggingConfig   `mapstructure:"logging"`
	Secrets   SecretsConfig   `mapstructure:"secrets"`
	Monitor   MonitorConfig   `mapstructure:"monitor"`
	Backup    BackupConfig    `mapstructure:"backup"`
	Scheduler SchedulerConfig `mapstructure:"scheduler"`
}

type ServerConfig struct {
	Addr            string        `mapstructure:"addr"`
	Port            int           `mapstructure:"port"`
	ExternalURL     string        `mapstructure:"external_url"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	ServeSPA        bool          `mapstructure:"serve_spa"`
	SPADir          string        `mapstructure:"spa_dir"`
	CORSOrigins     []string      `mapstructure:"cors_origins"`
	TLS             TLSConfig     `mapstructure:"tls"`
}

// ListenAddr renders the host:port pair for net.Listen.
func (s ServerConfig) ListenAddr() string {
	return fmt.Sprintf("%s:%d", s.Addr, s.Port)
}

type TLSConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
}

type AuthConfig struct {
	Enabled           bool          `mapstructure:"enabled"`
	SessionTTL        time.Duration `mapstructure:"session_ttl"`
	BootstrapUser     string        `mapstructure:"bootstrap_user"`
	BootstrapPassword string        `mapstructure:"bootstrap_password"`
	APITokens         []string      `mapstructure:"api_tokens"`
}

// DatabaseConfig описывает подключение к PostgreSQL.
//
// СУБД одна. Раньше поддерживались две, и выбор задавался отдельным полем
// driver — оно же было источником самой частой ошибки настройки: указать
// параметры PostgreSQL и забыть про driver значило тихо остаться на SQLite,
// причём служба поднималась и выглядела исправной. Одного варианта такого
// состояния не создаёт.
type DatabaseConfig struct {
	// URL — подключение одной строкой, в любой из двух форм:
	//
	//	postgres://пользователь:пароль@хост:5432/база?sslmode=require
	//	host=хост port=5432 user=… password=… dbname=… sslmode=require
	//
	// Пусто — берутся поля из блока postgres ниже.
	URL string `mapstructure:"url"`

	RunMigrationsOnStartup bool           `mapstructure:"run_migrations_on_startup"`
	Postgres               PostgresConfig `mapstructure:"postgres"`
}

// applyURL раскладывает database.url в driver и параметры подключения.
//
// Вызывается до Validate, поэтому дальше весь код работает с уже разобранной
// конфигурацией и о существовании url не знает.
func (d *DatabaseConfig) applyURL() error {
	raw := strings.TrimSpace(d.URL)
	if raw == "" {
		return nil
	}

	switch {
	case strings.HasPrefix(raw, "postgres://"), strings.HasPrefix(raw, "postgresql://"):
		// Строку не разбираем на части: pgx понимает и URL, и форму
		// «ключ=значение», а своя реализация разбора — это повторение чужой
		// работы вместе с её краевыми случаями (кодирование пароля, IPv6,
		// список хостов).
		d.Postgres.URL = raw

	case looksLikeKeywordDSN(raw):
		// Форма «host=… password=…» принимается наравне с URL и существует
		// ради паролей. В URL пароль обязан быть percent-кодирован, а
		// openssl rand -base64 выдаёт / и +, которые ломают разбор адреса —
		// то есть привычный генератор пароля даёт строку, непригодную для URL.
		// Здесь экранировать не нужно ничего.
		d.Postgres.URL = raw

	case strings.HasPrefix(raw, "sqlite:"):
		return fmt.Errorf("database.url: SQLite больше не поддерживается (%q).\n"+
			"Сервис работает только с PostgreSQL — см. docs/DEPLOY.md.\n"+
			"Существующая база SQLite не конвертируется автоматически: заведите "+
			"подключения и задания заново. Сами копии при этом не теряются — они "+
			"лежат в хранилище и читаются утилитой jvbackup без базы", raw)

	default:
		return fmt.Errorf("database.url: не распознан формат %q. Ожидается одно из:\n"+
			"  postgres://пользователь:пароль@хост:5432/база?sslmode=require\n"+
			"  host=хост port=5432 user=пользователь password=пароль dbname=база sslmode=require\n"+
			"Форма host=… удобнее, когда в пароле есть / + @ или не-ASCII: "+
			"в URL их пришлось бы percent-кодировать", raw)
	}
	return nil
}

// DatabaseFromDSN собирает конфигурацию подключения из одной строки.
//
// Тот же разбор, что и у database.url, но доступный снаружи: им пользуются
// тесты, которым нужна база из JHV_TEST_POSTGRES_DSN. Повторять разбор в
// каждом тестовом пакете значило бы завести несколько слегка разных
// реализаций одного и того же.
func DatabaseFromDSN(dsn string) (DatabaseConfig, error) {
	d := DatabaseConfig{URL: dsn, RunMigrationsOnStartup: true}
	if err := d.applyURL(); err != nil {
		return DatabaseConfig{}, err
	}
	d.Postgres.MaxConns = 5
	return d, nil
}

// looksLikeKeywordDSN распознаёт libpq-строку «ключ=значение через пробел».
//
// Признак — наличие host= или dbname= в начале одного из полей. Проверять
// просто по '=' нельзя: так под определение попал бы любой мусор со знаком
// равенства, и вместо внятного «не распознан формат» пользователь получил бы
// ошибку из недр драйвера.
func looksLikeKeywordDSN(raw string) bool {
	for _, field := range strings.Fields(raw) {
		switch {
		case strings.HasPrefix(field, "host="),
			strings.HasPrefix(field, "dbname="),
			strings.HasPrefix(field, "postgres="):
			return true
		}
	}
	return false
}

// Target описывает подключение для журнала — без пароля.
//
// Печатать при старте, куда именно подключились, нужно потому, что ошибка в
// выборе СУБД не падает: сервис молча уходит на другую базу и выглядит как
// потерявший данные. Одна строка в журнале превращает это в очевидное.
func (d DatabaseConfig) Target() string {
	if d.Postgres.URL != "" {
		return redactDSN(d.Postgres.URL)
	}
	return fmt.Sprintf("%s@%s:%d/%s",
		d.Postgres.User, d.Postgres.Host, d.Postgres.Port, d.Postgres.Database)
}

// redactDSN прячет пароль в строке подключения любой из принимаемых форм.
func redactDSN(raw string) string {
	if strings.Contains(raw, "://") {
		return redactURL(raw)
	}
	// Форма «ключ=значение»: гасим только password, остальное полезно видеть.
	fields := strings.Fields(raw)
	for i, field := range fields {
		if strings.HasPrefix(field, "password=") {
			fields[i] = "password=…"
		}
	}
	return strings.Join(fields, " ")
}

// redactURL прячет пароль в строке подключения.
//
// Замена делается по строке, а не через url.Parse: разбор спотыкается на
// паролях, которые не были percent-кодированы, и тогда пришлось бы либо
// печатать строку с паролем, либо не печатать ничего. Первое недопустимо,
// второе бесполезно — а нужен как раз адрес, чтобы увидеть, куда подключились.
func redactURL(raw string) string {
	const sep = "://"
	i := strings.Index(raw, sep)
	if i < 0 {
		return raw
	}
	scheme, rest := raw[:i+len(sep)], raw[i+len(sep):]

	// Пользовательская часть — до последней @ в пределах адреса, то есть до
	// первого / после схемы: пароль сам может содержать @.
	authorityEnd := strings.IndexByte(rest, '/')
	if authorityEnd < 0 {
		authorityEnd = len(rest)
	}
	at := strings.LastIndex(rest[:authorityEnd], "@")
	if at < 0 {
		return raw // без учётных данных прятать нечего
	}

	userinfo, tail := rest[:at], rest[at:]
	if colon := strings.IndexByte(userinfo, ':'); colon >= 0 {
		userinfo = userinfo[:colon] + ":…"
	}
	return scheme + userinfo + tail
}

type PostgresConfig struct {
	// URL — строка подключения целиком. Заполняется из database.url, но может
	// быть задана и напрямую. Если непуста, поля ниже не используются.
	URL string `mapstructure:"url"`

	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	Database string `mapstructure:"database"`
	SSLMode  string `mapstructure:"sslmode"`
	MaxConns int32  `mapstructure:"max_conns"`
}

// DSN renders a connection string for pgx.
//
// pgx понимает обе формы, поэтому готовый URL отдаётся как есть: разбирать
// его на части, чтобы тут же собрать обратно, значило бы завести собственный
// разбор URL со всеми его краевыми случаями.
func (p PostgresConfig) DSN() string {
	if p.URL != "" {
		return p.URL
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		p.Host, p.Port, p.User, p.Password, p.Database, p.SSLMode)
}

type LoggingConfig struct {
	Level      string `mapstructure:"level"`
	Format     string `mapstructure:"format"`
	File       string `mapstructure:"file"`
	MaxSizeMB  int    `mapstructure:"max_size_mb"`
	MaxBackups int    `mapstructure:"max_backups"`
	MaxAgeDays int    `mapstructure:"max_age_days"`
}

type SecretsConfig struct {
	KeyBase64 string `mapstructure:"key_base64"`
	KeyFile   string `mapstructure:"key_file"`
}

type MonitorConfig struct {
	Enabled          bool          `mapstructure:"enabled"`
	Interval         time.Duration `mapstructure:"interval"`
	Timeout          time.Duration `mapstructure:"timeout"`
	HistoryRetention time.Duration `mapstructure:"history_retention"`
	FailureThreshold int           `mapstructure:"failure_threshold"`
	// CollectIOStats включает снятие метрик ввода-вывода дисков и здоровья
	// монтирований NFS/iSCSI. Требует SSH-доступа к гипервизору, поэтому
	// работает только для подключений типа kvm.
	CollectIOStats bool `mapstructure:"collect_io_stats"`
	// IORetention — сколько хранить эти метрики. Они мельче проб состояния и
	// копятся быстрее, поэтому срок отдельный.
	IORetention time.Duration     `mapstructure:"io_retention"`
	Remediation RemediationConfig `mapstructure:"remediation"`
}

// RemediationConfig gates the automatic "revive" actions. Everything that can
// disrupt a running workload is opt-in.
type RemediationConfig struct {
	Enabled            bool          `mapstructure:"enabled"`
	DryRun             bool          `mapstructure:"dry_run"`
	Cooldown           time.Duration `mapstructure:"cooldown"`
	MaxAttemptsPerHour int           `mapstructure:"max_attempts_per_hour"`
	AllowVMStart       bool          `mapstructure:"allow_vm_start"`
	AllowVMUnpause     bool          `mapstructure:"allow_vm_unpause"`
	AllowHostActivate  bool          `mapstructure:"allow_host_activate"`
	AllowHostFence     bool          `mapstructure:"allow_host_fence"`
	// ArchiveDir — куда складывать архивы периодов режима проверки.
	// Это обоснование перехода в боевой режим, поэтому хранится рядом с
	// данными сервиса, а не во временном каталоге.
	ArchiveDir string `mapstructure:"archive_dir"`
}

type BackupConfig struct {
	Workers          int    `mapstructure:"workers"`
	ChunkSize        int    `mapstructure:"chunk_size"`
	Compression      string `mapstructure:"compression"`
	CompressionLevel int    `mapstructure:"compression_level"`
	// HeavyWorkers ограничивает число одновременных проверок и восстановлений.
	// Обе операции читают цепочку целиком из хранилища, поэтому предел общий и
	// отдельный от workers: бэкапы упираются в гипервизор, а эти — в хранилище.
	HeavyWorkers int            `mapstructure:"heavy_workers"`
	TempDir      string         `mapstructure:"temp_dir"`
	QemuImgPath  string         `mapstructure:"qemu_img_path"`
	Transfer     TransferConfig `mapstructure:"transfer"`

	// RestoreDirs ограничивает каталоги, куда разрешено восстанавливать
	// образы. Каталог приходит из запроса, а восстановленный образ — это
	// десятки гигабайт: без списка любой оператор мог бы записать их в любой
	// доступный службе путь. temp_dir разрешён всегда и добавлять его сюда не
	// нужно.
	RestoreDirs []string `mapstructure:"restore_dirs"`
}

// RestoreRoots возвращает каталоги, внутри которых разрешено создавать файлы
// восстановления. temp_dir входит всегда: это каталог самой службы, и запрет
// на него сделал бы восстановление в файл невозможным из коробки.
func (b BackupConfig) RestoreRoots() []string {
	roots := make([]string, 0, len(b.RestoreDirs)+1)
	if b.TempDir != "" {
		roots = append(roots, b.TempDir)
	}
	for _, d := range b.RestoreDirs {
		if d != "" {
			roots = append(roots, d)
		}
	}
	return roots
}

type TransferConfig struct {
	PreferProxy       bool          `mapstructure:"prefer_proxy"`
	InactivityTimeout time.Duration `mapstructure:"inactivity_timeout"`
	RequestTimeout    time.Duration `mapstructure:"request_timeout"`
	MaxParallelDisks  int           `mapstructure:"max_parallel_disks"`
	RangeRetries      int           `mapstructure:"range_retries"`
}

type SchedulerConfig struct {
	Enabled       bool   `mapstructure:"enabled"`
	Timezone      string `mapstructure:"timezone"`
	CatchUpMissed bool   `mapstructure:"catch_up_missed"`
}

// Load reads the configuration from path (optional) merged over the built-in
// defaults, then applies environment overrides and validates the result.
func Load(path string) (*Config, error) {
	v := viper.New()
	setDefaults(v)

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv alone does not reach nested keys during Unmarshal, so every
	// known key is bound explicitly.
	for _, key := range v.AllKeys() {
		_ = v.BindEnv(key)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	// До Validate: url задаёт driver, и проверять драйвер имеет смысл уже
	// после того, как он окончательно определён.
	if err := cfg.Database.applyURL(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate rejects combinations that would fail later in a confusing way.
func (c *Config) Validate() error {
	if c.Database.Postgres.URL == "" && c.Database.Postgres.Host == "" {
		return fmt.Errorf("не задано подключение к базе: укажите database.url " +
			"(или JHV_DATABASE_URL) либо блок database.postgres")
	}
	switch c.Backup.Compression {
	case "none", "zstd":
	default:
		return fmt.Errorf("backup.compression must be none or zstd, got %q", c.Backup.Compression)
	}
	if c.Backup.ChunkSize < 64*1024 || c.Backup.ChunkSize%(64*1024) != 0 {
		return fmt.Errorf("backup.chunk_size must be a multiple of 64 KiB and >= 64 KiB, got %d", c.Backup.ChunkSize)
	}
	if c.Backup.Workers < 1 {
		return fmt.Errorf("backup.workers must be >= 1, got %d", c.Backup.Workers)
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port out of range: %d", c.Server.Port)
	}
	if c.Server.TLS.Enabled && (c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "") {
		return fmt.Errorf("server.tls.enabled requires cert_file and key_file")
	}
	if c.Monitor.Interval < time.Second {
		return fmt.Errorf("monitor.interval must be >= 1s, got %s", c.Monitor.Interval)
	}
	if _, err := time.LoadLocation(c.Scheduler.Timezone); err != nil {
		return fmt.Errorf("scheduler.timezone %q: %w", c.Scheduler.Timezone, err)
	}
	return nil
}

// Location resolves the scheduler timezone, falling back to UTC.
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Scheduler.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.addr", "0.0.0.0")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.external_url", "http://localhost:8080")
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "0s")
	v.SetDefault("server.shutdown_timeout", "30s")
	v.SetDefault("server.serve_spa", true)
	v.SetDefault("server.spa_dir", "./web/dist")
	v.SetDefault("server.cors_origins", []string{"http://localhost:9000"})
	v.SetDefault("server.tls.enabled", false)
	v.SetDefault("server.tls.cert_file", "")
	v.SetDefault("server.tls.key_file", "")

	v.SetDefault("auth.enabled", true)
	v.SetDefault("auth.session_ttl", "12h")
	v.SetDefault("auth.bootstrap_user", "admin")
	v.SetDefault("auth.bootstrap_password", "")
	v.SetDefault("auth.api_tokens", []string{})

	// Пустое значение по умолчанию нужно, чтобы ключ существовал: привязка
	// переменных окружения идёт по списку известных ключей, и без этой строки
	// JHV_DATABASE_URL просто не читался бы.
	v.SetDefault("database.url", "")
	v.SetDefault("database.postgres.url", "")
	v.SetDefault("database.run_migrations_on_startup", true)
	v.SetDefault("database.postgres.host", "localhost")
	v.SetDefault("database.postgres.port", 5432)
	v.SetDefault("database.postgres.user", "jhvirt")
	v.SetDefault("database.postgres.password", "")
	v.SetDefault("database.postgres.database", "jhvirt")
	v.SetDefault("database.postgres.sslmode", "disable")
	v.SetDefault("database.postgres.max_conns", 10)

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "console")
	v.SetDefault("logging.file", "")
	v.SetDefault("logging.max_size_mb", 100)
	v.SetDefault("logging.max_backups", 7)
	v.SetDefault("logging.max_age_days", 30)

	v.SetDefault("secrets.key_base64", "")
	v.SetDefault("secrets.key_file", "./data/secret.key")

	v.SetDefault("monitor.enabled", true)
	v.SetDefault("monitor.interval", "30s")
	v.SetDefault("monitor.timeout", "20s")
	v.SetDefault("monitor.history_retention", "168h")
	v.SetDefault("monitor.failure_threshold", 3)
	v.SetDefault("monitor.remediation.enabled", true)
	v.SetDefault("monitor.collect_io_stats", true)
	v.SetDefault("monitor.io_retention", "168h")
	v.SetDefault("monitor.remediation.dry_run", true)
	v.SetDefault("monitor.remediation.archive_dir", "data/remediation-archives")
	v.SetDefault("monitor.remediation.cooldown", "10m")
	v.SetDefault("monitor.remediation.max_attempts_per_hour", 3)
	v.SetDefault("monitor.remediation.allow_vm_start", true)
	v.SetDefault("monitor.remediation.allow_vm_unpause", true)
	v.SetDefault("monitor.remediation.allow_host_activate", true)
	v.SetDefault("monitor.remediation.allow_host_fence", false)

	v.SetDefault("backup.workers", 2)
	v.SetDefault("backup.chunk_size", 4*1024*1024)
	v.SetDefault("backup.compression", "zstd")
	v.SetDefault("backup.compression_level", 3)
	v.SetDefault("backup.heavy_workers", 2)
	v.SetDefault("backup.temp_dir", "./data/tmp")
	v.SetDefault("backup.restore_dirs", []string{})
	v.SetDefault("backup.qemu_img_path", "")
	v.SetDefault("backup.transfer.prefer_proxy", false)
	v.SetDefault("backup.transfer.inactivity_timeout", "60s")
	v.SetDefault("backup.transfer.request_timeout", "10m")
	v.SetDefault("backup.transfer.max_parallel_disks", 2)
	v.SetDefault("backup.transfer.range_retries", 3)

	v.SetDefault("scheduler.enabled", true)
	v.SetDefault("scheduler.timezone", "UTC")
	v.SetDefault("scheduler.catch_up_missed", true)
}
