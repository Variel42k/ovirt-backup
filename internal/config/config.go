// Package config loads the service configuration from a YAML file and the
// environment. Every key can be overridden with a JHV_-prefixed variable where
// dots in the key path become underscores (JHV_DATABASE_POSTGRES_PASSWORD).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"

	"adveng/jh_virt/internal/model"
)

const envPrefix = "JHV"

// Config is the fully resolved runtime configuration.
type Config struct {
	Server           ServerConfig           `mapstructure:"server"`
	Auth             AuthConfig             `mapstructure:"auth"`
	Database         DatabaseConfig         `mapstructure:"database"`
	Logging          LoggingConfig          `mapstructure:"logging"`
	Secrets          SecretsConfig          `mapstructure:"secrets"`
	Monitor          MonitorConfig          `mapstructure:"monitor"`
	Metrics          MetricsConfig          `mapstructure:"metrics"`
	Notifications    NotificationsConfig    `mapstructure:"notifications"`
	Cluster          ClusterConfig          `mapstructure:"cluster"`
	Backup           BackupConfig           `mapstructure:"backup"`
	FileBackup       FileBackupConfig       `mapstructure:"file_backup"`
	Scheduler        SchedulerConfig        `mapstructure:"scheduler"`
	DisasterRecovery DisasterRecoveryConfig `mapstructure:"disaster_recovery"`
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
	OIDC              OIDCConfig    `mapstructure:"oidc"`
}

// OIDCConfig describes the external identity provider.
//
// Пароль пользователя при таком входе через службу не проходит вовсе: она
// получает от провайдера подписанный токен и по нему заводит сессию. Отсюда и
// 2FA, и единый вход, и мгновенный отзыв доступа — всё это остаётся заботой
// провайдера, а не воспроизводится здесь заново и хуже.
type OIDCConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Issuer — адрес провайдера; остальные точки берутся из его discovery.
	Issuer       string `mapstructure:"issuer"`
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`
	// RedirectURL должен совпадать с зарегистрированным у провайдера точно,
	// вплоть до схемы и завершающего пути.
	RedirectURL string   `mapstructure:"redirect_url"`
	Scopes      []string `mapstructure:"scopes"`
	// Заголовок кнопки на странице входа: «Войти через …».
	ButtonLabel string `mapstructure:"button_label"`
	// GroupsClaim — где в токене лежат группы, RoleMapping — во что они
	// превращаются. Роль назначается по первому совпадению в порядке
	// admin, operator, auditor, viewer: у пользователя может быть несколько
	// групп, и старшая должна побеждать.
	GroupsClaim string            `mapstructure:"groups_claim"`
	RoleMapping map[string]string `mapstructure:"role_mapping"`
	// DefaultRole получают те, чьи группы ни во что не отобразились. Пусто —
	// вход запрещён: молча выдавать права тому, кого не ждали, нельзя.
	DefaultRole string `mapstructure:"default_role"`
	// PostLogoutRedirectURL — куда провайдер вернёт браузер после выхода.
	//
	// Пусто — параметр не передаётся вовсе, и человек остаётся на странице
	// провайдера. Это умолчание намеренное: адрес возврата провайдер обязан
	// иметь в списке разрешённых, и незарегистрированный превращает выход в
	// страницу ошибки.
	PostLogoutRedirectURL string `mapstructure:"post_logout_redirect_url"`
	// SessionTTL — срок сессии, заведённой через провайдера. Пусто — общий
	// auth.session_ttl.
	//
	// Смысл в отзыве доступа: роль пересчитывается при входе, но уже выданная
	// сессия живёт своим сроком, и сотрудник, у которого отобрали группу,
	// остаётся администратором до её конца. Час вместо полусуток — компромисс:
	// мгновенного отзыва он не даёт, но и не растягивает права на смену.
	SessionTTL time.Duration `mapstructure:"session_ttl"`
	// AllowLocalLogin оставляет вход по паролю рядом с внешним.
	//
	// Выключать не рекомендуется: локальная запись администратора — это путь
	// внутрь, когда провайдер недоступен, а недоступен он бывает ровно в той
	// аварии, ради которой и разворачивают систему восстановления.
	AllowLocalLogin bool `mapstructure:"allow_local_login"`
}

// NotificationsConfig — доставка оповещений наружу.
//
// Внутри системы оповещения и так видны, но узнаёт о них только тот, кто в эту
// минуту смотрит в интерфейс. Бэкапы идут ночью, и ночью же ломаются.
type NotificationsConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// MinSeverity — порог: critical (по умолчанию), warning либо info.
	//
	// Умолчание выбрано так, чтобы наружу уходило только то, ради чего стоит
	// будить человека. Предупреждений бывает много, и канал, по которому идёт
	// поток «в целом всё в порядке», перестают читать целиком — вместе с тем
	// единственным сообщением, ради которого он заводился.
	MinSeverity string         `mapstructure:"min_severity"`
	Webhook     WebhookConfig  `mapstructure:"webhook"`
	Telegram    TelegramConfig `mapstructure:"telegram"`
	Email       EmailConfig    `mapstructure:"email"`
}

// WebhookConfig — произвольный приёмник HTTP.
type WebhookConfig struct {
	URL string `mapstructure:"url"`
	// Token уходит в заголовке Authorization: Bearer.
	Token string `mapstructure:"token"`
}

// TelegramConfig — бот и получатель.
type TelegramConfig struct {
	BotToken string `mapstructure:"bot_token"`
	ChatID   string `mapstructure:"chat_id"`
}

// EmailConfig — отправка через SMTP.
type EmailConfig struct {
	SMTPHost string   `mapstructure:"smtp_host"`
	SMTPPort int      `mapstructure:"smtp_port"`
	Username string   `mapstructure:"username"`
	Password string   `mapstructure:"password"`
	From     string   `mapstructure:"from"`
	To       []string `mapstructure:"to"`
}

// ClusterConfig — запуск нескольких экземпляров службы.
//
// Очередь переносов безопасна для нескольких процессов сама по себе: задачи
// разбираются арендой. А вот планировщик, монитор и авто-восстановление — нет:
// два экземпляра выполнили бы каждое задание дважды и подрались бы за действия
// над ВМ. Поэтому они работают только у ведущего.
type ClusterConfig struct {
	// LeaderElection по умолчанию выключен: при одном экземпляре он ничего не
	// добавляет, а поведение установки менять на ровном месте незачем.
	// Включать обязательно до запуска второго экземпляра.
	LeaderElection bool `mapstructure:"leader_election"`
	// PollInterval — как часто ведомый проверяет, не освободилось ли место, а
	// ведущий — что оно всё ещё за ним.
	PollInterval time.Duration `mapstructure:"poll_interval"`
}

type MetricsConfig struct {
	Enabled   bool   `mapstructure:"enabled"`
	TokenFile string `mapstructure:"token_file"`
}

type DisasterRecoveryConfig struct {
	Enabled             bool          `mapstructure:"enabled"`
	PostgresDumpPath    string        `mapstructure:"postgres_dump_path"`
	PostgresDumpMaxAge  time.Duration `mapstructure:"postgres_dump_max_age"`
	SecretKeyBackupPath string        `mapstructure:"secret_key_backup_path"`
	CheckInterval       time.Duration `mapstructure:"check_interval"`
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
	IORetention   time.Duration               `mapstructure:"io_retention"`
	BackupQuality model.BackupQualitySettings `mapstructure:"backup_quality"`
	Remediation   RemediationConfig           `mapstructure:"remediation"`
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
	Workers            int    `mapstructure:"workers"`
	ReplicationWorkers int    `mapstructure:"replication_workers"`
	ChunkSize          int    `mapstructure:"chunk_size"`
	Compression        string `mapstructure:"compression"`
	CompressionLevel   int    `mapstructure:"compression_level"`
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

type FileBackupRoot struct {
	ID           string   `mapstructure:"id" json:"id"`
	Name         string   `mapstructure:"name" json:"name"`
	Path         string   `mapstructure:"path" json:"-"`
	RestoreRoots []string `mapstructure:"restore_roots" json:"-"`
}

type FileBackupConfig struct {
	Enabled bool             `mapstructure:"enabled"`
	Roots   []FileBackupRoot `mapstructure:"roots"`
}

func (f FileBackupConfig) Root(id string) (FileBackupRoot, bool) {
	for _, root := range f.Roots {
		if root.ID == id {
			return root, true
		}
	}
	return FileBackupRoot{}, false
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

// validRole reports whether a role name is one the authorization layer knows.
//
// Список повторён строкой, а не взят из internal/model: конфигурация лежит
// ниже модели и импорт замкнул бы пакеты друг на друга. Расхождение ловится
// тестом в internal/api, где обе стороны уже видны.
func validRole(role string) bool {
	switch role {
	case "admin", "operator", "viewer":
		return true
	}
	return false
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
	// Список повторён здесь строкой, а не взят из internal/backup: движок
	// бэкапа настраивается этой структурой, и импорт в обратную сторону замкнул
	// бы пакеты друг на друга. При добавлении алгоритма правятся оба места, и
	// тест на согласованность есть в internal/backup.
	switch c.Backup.Compression {
	case "none", "zstd", "gzip", "s2":
	default:
		return fmt.Errorf("backup.compression must be none, zstd, gzip or s2, got %q", c.Backup.Compression)
	}
	if c.Backup.CompressionLevel < 1 || c.Backup.CompressionLevel > 9 {
		return fmt.Errorf("backup.compression_level must be between 1 and 9, got %d", c.Backup.CompressionLevel)
	}
	if c.Logging.MaxSizeMB < 1 || c.Logging.MaxSizeMB > 10240 {
		return fmt.Errorf("logging.max_size_mb must be between 1 and 10240, got %d", c.Logging.MaxSizeMB)
	}
	if c.Logging.MaxBackups < 1 || c.Logging.MaxBackups > 1000 {
		return fmt.Errorf("logging.max_backups must be between 1 and 1000, got %d", c.Logging.MaxBackups)
	}
	if c.Logging.MaxAgeDays < 1 || c.Logging.MaxAgeDays > 3650 {
		return fmt.Errorf("logging.max_age_days must be between 1 and 3650, got %d", c.Logging.MaxAgeDays)
	}
	if c.Auth.OIDC.Enabled {
		for key, value := range map[string]string{
			"auth.oidc.issuer":       c.Auth.OIDC.Issuer,
			"auth.oidc.client_id":    c.Auth.OIDC.ClientID,
			"auth.oidc.redirect_url": c.Auth.OIDC.RedirectURL,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s обязателен при auth.oidc.enabled", key)
			}
		}
		// Роль, взятая из воздуха, — это выданные права. Если группы ни во что
		// не отображаются и умолчания нет, вход запрещается, и это правильный
		// исход: неизвестному пользователю не место в системе, которая
		// управляет чужими виртуальными машинами.
		if len(c.Auth.OIDC.RoleMapping) == 0 && c.Auth.OIDC.DefaultRole == "" {
			return fmt.Errorf("задайте auth.oidc.role_mapping либо auth.oidc.default_role: " +
				"иначе вошедшему через провайдера не из чего назначить роль")
		}
		for group, role := range c.Auth.OIDC.RoleMapping {
			if !validRole(role) {
				return fmt.Errorf("auth.oidc.role_mapping[%q]: неизвестная роль %q", group, role)
			}
		}
		if c.Auth.OIDC.DefaultRole != "" && !validRole(c.Auth.OIDC.DefaultRole) {
			return fmt.Errorf("auth.oidc.default_role: неизвестная роль %q", c.Auth.OIDC.DefaultRole)
		}
	}
	if c.Notifications.Enabled {
		switch c.Notifications.MinSeverity {
		case "", "critical", "warning", "info":
		default:
			return fmt.Errorf("notifications.min_severity: допустимы critical, warning и info, задано %q",
				c.Notifications.MinSeverity)
		}
		// Включённые оповещения без единого канала — это молчание, которое
		// выглядит как настроенная доставка. Лучше отказаться на старте.
		if strings.TrimSpace(c.Notifications.Webhook.URL) == "" &&
			strings.TrimSpace(c.Notifications.Telegram.BotToken) == "" &&
			strings.TrimSpace(c.Notifications.Email.SMTPHost) == "" {
			return fmt.Errorf("notifications.enabled задан, но не настроен ни один канал: " +
				"webhook.url, telegram.bot_token либо email.smtp_host")
		}
		if strings.TrimSpace(c.Notifications.Email.SMTPHost) != "" && len(c.Notifications.Email.To) == 0 {
			return fmt.Errorf("notifications.email.to: некому отправлять")
		}
		if strings.TrimSpace(c.Notifications.Telegram.BotToken) != "" &&
			strings.TrimSpace(c.Notifications.Telegram.ChatID) == "" {
			return fmt.Errorf("notifications.telegram.chat_id обязателен вместе с bot_token")
		}
	}
	if c.Backup.ChunkSize < 64*1024 || c.Backup.ChunkSize%(64*1024) != 0 {
		return fmt.Errorf("backup.chunk_size must be a multiple of 64 KiB and >= 64 KiB, got %d", c.Backup.ChunkSize)
	}
	if c.Backup.Workers < 1 {
		return fmt.Errorf("backup.workers must be >= 1, got %d", c.Backup.Workers)
	}
	if c.Backup.ReplicationWorkers < 1 {
		return fmt.Errorf("backup.replication_workers must be >= 1, got %d", c.Backup.ReplicationWorkers)
	}
	seenFileRoots := map[string]bool{}
	for _, root := range c.FileBackup.Roots {
		if strings.TrimSpace(root.ID) == "" || strings.TrimSpace(root.Path) == "" {
			return fmt.Errorf("file_backup.roots require id and path")
		}
		if seenFileRoots[root.ID] {
			return fmt.Errorf("duplicate file_backup root id %q", root.ID)
		}
		seenFileRoots[root.ID] = true
		if !filepath.IsAbs(root.Path) {
			return fmt.Errorf("file_backup root %q must be absolute", root.ID)
		}
		for _, restoreRoot := range root.RestoreRoots {
			if !filepath.IsAbs(restoreRoot) {
				return fmt.Errorf("file backup restore root %q must be absolute", restoreRoot)
			}
		}
	}
	if c.DisasterRecovery.Enabled {
		if strings.TrimSpace(c.DisasterRecovery.PostgresDumpPath) == "" ||
			strings.TrimSpace(c.DisasterRecovery.SecretKeyBackupPath) == "" {
			return fmt.Errorf("disaster_recovery.enabled requires postgres_dump_path and secret_key_backup_path")
		}
		if c.DisasterRecovery.PostgresDumpMaxAge <= 0 || c.DisasterRecovery.CheckInterval < time.Minute {
			return fmt.Errorf("disaster_recovery: max age must be positive and check_interval at least 1m")
		}
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
	if err := c.Monitor.BackupQuality.Validate(); err != nil {
		return fmt.Errorf("monitor.backup_quality: %w", err)
	}
	if c.Metrics.Enabled {
		if strings.TrimSpace(c.Metrics.TokenFile) == "" {
			return fmt.Errorf("metrics.enabled requires metrics.token_file")
		}
		body, err := os.ReadFile(c.Metrics.TokenFile)
		if err != nil {
			return fmt.Errorf("metrics.token_file %q: %w", c.Metrics.TokenFile, err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return fmt.Errorf("metrics.token_file %q is empty", c.Metrics.TokenFile)
		}
		info, err := os.Stat(c.Metrics.TokenFile)
		if err != nil {
			return fmt.Errorf("metrics.token_file %q: %w", c.Metrics.TokenFile, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("metrics.token_file %q is not a regular file", c.Metrics.TokenFile)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("metrics.token_file %q must not be accessible by group or others (expected mode 0600)", c.Metrics.TokenFile)
		}
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
	v.SetDefault("auth.oidc.enabled", false)
	v.SetDefault("auth.oidc.issuer", "")
	v.SetDefault("auth.oidc.client_id", "")
	v.SetDefault("auth.oidc.client_secret", "")
	v.SetDefault("auth.oidc.redirect_url", "")
	v.SetDefault("auth.oidc.scopes", []string{"openid", "profile", "email", "groups"})
	v.SetDefault("auth.oidc.button_label", "")
	v.SetDefault("auth.oidc.groups_claim", "groups")
	v.SetDefault("auth.oidc.role_mapping", map[string]string{})
	v.SetDefault("auth.oidc.default_role", "")
	v.SetDefault("auth.oidc.post_logout_redirect_url", "")
	// Час: отобранная у провайдера группа перестаёт действовать здесь не позже
	// чем через час, а не через полсуток общего срока сессии.
	v.SetDefault("auth.oidc.session_ttl", time.Hour)
	v.SetDefault("auth.oidc.allow_local_login", true)
	v.SetDefault("metrics.enabled", false)
	v.SetDefault("metrics.token_file", "")
	v.SetDefault("cluster.leader_election", false)
	v.SetDefault("cluster.poll_interval", 15*time.Second)
	v.SetDefault("notifications.enabled", false)
	v.SetDefault("notifications.min_severity", "critical")
	v.SetDefault("notifications.webhook.url", "")
	v.SetDefault("notifications.webhook.token", "")
	v.SetDefault("notifications.telegram.bot_token", "")
	v.SetDefault("notifications.telegram.chat_id", "")
	v.SetDefault("notifications.email.smtp_host", "")
	v.SetDefault("notifications.email.smtp_port", 587)
	v.SetDefault("notifications.email.username", "")
	v.SetDefault("notifications.email.password", "")
	v.SetDefault("notifications.email.from", "")
	v.SetDefault("notifications.email.to", []string{})

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
	v.SetDefault("monitor.backup_quality.stale_intervals", 2)
	v.SetDefault("monitor.backup_quality.verify_max_age_days", 7)
	v.SetDefault("monitor.backup_quality.performance_window_runs", 10)
	v.SetDefault("monitor.backup_quality.performance_degradation_percent", 50)
	v.SetDefault("monitor.backup_quality.performance_consecutive_runs", 3)
	v.SetDefault("monitor.backup_quality.storage_warning_free_percent", 15)
	v.SetDefault("monitor.backup_quality.storage_critical_free_percent", 5)
	v.SetDefault("monitor.backup_quality.storage_warning_forecast_days", 30)
	v.SetDefault("monitor.backup_quality.storage_critical_forecast_days", 7)
	v.SetDefault("monitor.backup_quality.history_retention_days", 90)
	v.SetDefault("monitor.remediation.dry_run", true)
	v.SetDefault("monitor.remediation.archive_dir", "data/remediation-archives")
	v.SetDefault("monitor.remediation.cooldown", "10m")
	v.SetDefault("monitor.remediation.max_attempts_per_hour", 3)
	v.SetDefault("monitor.remediation.allow_vm_start", true)
	v.SetDefault("monitor.remediation.allow_vm_unpause", true)
	v.SetDefault("monitor.remediation.allow_host_activate", true)
	v.SetDefault("monitor.remediation.allow_host_fence", false)

	v.SetDefault("backup.workers", 2)
	v.SetDefault("backup.replication_workers", 2)
	v.SetDefault("backup.chunk_size", 4*1024*1024)
	v.SetDefault("backup.compression", "zstd")
	v.SetDefault("backup.compression_level", 3)
	v.SetDefault("backup.heavy_workers", 2)
	v.SetDefault("backup.temp_dir", "./data/tmp")
	v.SetDefault("backup.restore_dirs", []string{})
	v.SetDefault("backup.qemu_img_path", "")

	v.SetDefault("disaster_recovery.enabled", false)
	v.SetDefault("disaster_recovery.postgres_dump_path", "")
	v.SetDefault("disaster_recovery.postgres_dump_max_age", "24h")
	v.SetDefault("disaster_recovery.secret_key_backup_path", "")
	v.SetDefault("disaster_recovery.check_interval", "1h")
	v.SetDefault("backup.transfer.prefer_proxy", false)
	v.SetDefault("backup.transfer.inactivity_timeout", "60s")
	v.SetDefault("backup.transfer.request_timeout", "10m")
	v.SetDefault("backup.transfer.max_parallel_disks", 2)
	v.SetDefault("backup.transfer.range_retries", 3)

	v.SetDefault("scheduler.enabled", true)
	v.SetDefault("scheduler.timezone", "UTC")
	v.SetDefault("file_backup.enabled", false)
	v.SetDefault("file_backup.roots", []map[string]any{})
	v.SetDefault("scheduler.catch_up_missed", true)
}
