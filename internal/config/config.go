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

type DatabaseConfig struct {
	Driver                 string         `mapstructure:"driver"`
	RunMigrationsOnStartup bool           `mapstructure:"run_migrations_on_startup"`
	SQLite                 SQLiteConfig   `mapstructure:"sqlite"`
	Postgres               PostgresConfig `mapstructure:"postgres"`
}

type SQLiteConfig struct {
	Path        string        `mapstructure:"path"`
	BusyTimeout time.Duration `mapstructure:"busy_timeout"`
}

type PostgresConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	Database string `mapstructure:"database"`
	SSLMode  string `mapstructure:"sslmode"`
	MaxConns int32  `mapstructure:"max_conns"`
}

// DSN renders a libpq-style connection string for pgx.
func (p PostgresConfig) DSN() string {
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
	Workers          int            `mapstructure:"workers"`
	ChunkSize        int            `mapstructure:"chunk_size"`
	Compression      string         `mapstructure:"compression"`
	CompressionLevel int            `mapstructure:"compression_level"`
	TempDir          string         `mapstructure:"temp_dir"`
	QemuImgPath      string         `mapstructure:"qemu_img_path"`
	Transfer         TransferConfig `mapstructure:"transfer"`
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
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate rejects combinations that would fail later in a confusing way.
func (c *Config) Validate() error {
	switch c.Database.Driver {
	case "sqlite", "postgres":
	default:
		return fmt.Errorf("database.driver must be sqlite or postgres, got %q", c.Database.Driver)
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

	v.SetDefault("database.driver", "sqlite")
	v.SetDefault("database.run_migrations_on_startup", true)
	v.SetDefault("database.sqlite.path", "./data/jhvirt.db")
	v.SetDefault("database.sqlite.busy_timeout", "10s")
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
	v.SetDefault("backup.temp_dir", "./data/tmp")
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
