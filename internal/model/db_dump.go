package model

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// DBEngine — СУБД, дампы которой умеет снимать хелпер jhvirt-db-dump.
type DBEngine string

const (
	DBEnginePostgreSQL DBEngine = "postgresql"
	// DBEngineMySQL покрывает и MySQL, и MariaDB: клиент и формат дампа общие.
	DBEngineMySQL DBEngine = "mysql"
)

// Valid сообщает, известна ли СУБД.
func (e DBEngine) Valid() bool {
	return e == DBEnginePostgreSQL || e == DBEngineMySQL
}

// Title — название для сообщений и интерфейса.
func (e DBEngine) Title() string {
	switch e {
	case DBEnginePostgreSQL:
		return "PostgreSQL"
	case DBEngineMySQL:
		return "MySQL / MariaDB"
	}
	return string(e)
}

// dbNamePattern — алфавит имён баз, которые служба передаёт хелперу.
//
// Он уже, чем допускают сами СУБД, намеренно: имя уходит в командную строку
// forced command, и хелпер сверяет его с тем же выражением. Базу с пробелом
// или кавычкой в имени проще переименовать, чем безопасно экранировать на
// обоих концах.
var dbNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,62}$`)

// ValidDBName сообщает, можно ли передать имя базы хелперу.
func ValidDBName(name string) bool {
	return dbNamePattern.MatchString(name)
}

// DBHost — подключение к хосту СУБД через SSH и хелпер jhvirt-db-dump.
//
// Паролей СУБД здесь нет и не будет: хелпер работает на самом хосте от
// отдельного системного пользователя и входит в СУБД через Unix-сокет.
// Секрет один — SSH-ключ, и он хранится зашифрованным ключом службы.
type DBHost struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	// PrivateKey наружу не отдаётся: браузер видит только PrivateKeyStored.
	PrivateKey       string `json:"-"`
	PrivateKeyStored bool   `json:"private_key_stored"`
	// HostKey — закреплённый ключ SSH-сервера. Пусто и TrustAnyHostKey=false
	// означает «подключения не будет», как у гипервизоров и SFTP.
	HostKey         string `json:"host_key,omitempty"`
	TrustAnyHostKey bool   `json:"trust_any_host_key"`
	// ServerID/VMID связывают статистику этой СУБД с бэкапом ВМ. Пустые поля
	// означают, что хост используется только для логических дампов.
	ServerID      string   `json:"server_id,omitempty"`
	VMID          string   `json:"vm_id,omitempty"`
	MonitorEngine DBEngine `json:"monitor_engine,omitempty"`
	// Engines — что ответил хелпер на probe: СУБД и версии клиентов.
	Engines   []DBEngineInfo `json:"engines,omitempty"`
	ProbedAt  *time.Time     `json:"probed_at,omitempty"`
	ProbeErr  string         `json:"probe_error,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// DBEngineInfo — одна СУБД на хосте по данным probe.
type DBEngineInfo struct {
	Engine  DBEngine `json:"engine"`
	Version string   `json:"version"`
}

// Validate проверяет подключение перед сохранением.
func (h *DBHost) Validate() error {
	if strings.TrimSpace(h.Name) == "" {
		return fmt.Errorf("укажите имя хоста СУБД")
	}
	if strings.TrimSpace(h.Address) == "" {
		return fmt.Errorf("укажите адрес хоста СУБД")
	}
	if strings.ContainsAny(h.Address, " /\\") {
		return fmt.Errorf("адрес хоста СУБД содержит недопустимые символы")
	}
	if h.Port < 0 || h.Port > 65535 {
		return fmt.Errorf("неверный порт SSH: %d", h.Port)
	}
	if strings.TrimSpace(h.Username) == "" {
		return fmt.Errorf("укажите SSH-пользователя хелпера")
	}
	if strings.TrimSpace(h.PrivateKey) == "" && !h.PrivateKeyStored {
		return fmt.Errorf("нужен приватный SSH-ключ: вход по паролю для дампов не поддерживается")
	}
	if strings.TrimSpace(h.HostKey) == "" && !h.TrustAnyHostKey {
		return fmt.Errorf("закрепите ключ SSH-сервера или явно разрешите подключение без проверки")
	}
	monitoringConfigured := h.ServerID != "" || h.VMID != "" || h.MonitorEngine != ""
	if monitoringConfigured && (h.ServerID == "" || h.VMID == "" || !h.MonitorEngine.Valid()) {
		return fmt.Errorf("для мониторинга выберите вместе виртуализацию, ВМ и СУБД")
	}
	return nil
}

// DBStatsSample — накопительные счётчики СУБД во время бэкапа ВМ.
// Скорость вычисляется между пробами; запись в пользовательскую базу не нужна.
type DBStatsSample struct {
	ID       string    `json:"id"`
	RunID    string    `json:"run_id"`
	HostID   string    `json:"host_id"`
	HostName string    `json:"host_name"`
	Engine   DBEngine  `json:"engine"`
	At       time.Time `json:"at"`
	// Cumulative counters are encoded as decimal strings: WAL/redo can exceed
	// JavaScript's exact integer range on a long-lived database host.
	Commits   int64  `json:"commits,string"`
	Rollbacks int64  `json:"rollbacks,string"`
	Active    int64  `json:"active"`
	LogBytes  int64  `json:"log_bytes,string"`
	Error     string `json:"error,omitempty"`
}

// DBDumpJob — задание логических дампов одной СУБД на одном хосте.
type DBDumpJob struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Enabled bool     `json:"enabled"`
	HostID  string   `json:"host_id"`
	Engine  DBEngine `json:"engine"`
	// Databases — какие базы снимать; пусто — все, что вернёт list.
	Databases []string `json:"databases"`
	// IncludeGlobals добавляет роли и табличные пространства PostgreSQL
	// (pg_dumpall --globals-only, без хешей паролей).
	IncludeGlobals   bool     `json:"include_globals"`
	StorageTargetIDs []string `json:"storage_target_ids"`
	Encrypt          bool     `json:"encrypt"`
	// VerifyAfter перечитывает точку сразу после дампа: каждый чанк
	// расшифровывается и сверяется по SHA-256. Дамп, который нельзя прочитать,
	// должен обнаружиться в ночь съёмки, а не в день восстановления.
	VerifyAfter bool            `json:"verify_after"`
	Schedule    string          `json:"schedule,omitempty"`
	Retention   RetentionPolicy `json:"retention"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Validate проверяет задание перед сохранением.
func (j *DBDumpJob) Validate() error {
	if strings.TrimSpace(j.Name) == "" {
		return fmt.Errorf("укажите имя задания")
	}
	if strings.TrimSpace(j.HostID) == "" {
		return fmt.Errorf("выберите хост СУБД")
	}
	if !j.Engine.Valid() {
		return fmt.Errorf("неизвестная СУБД: %q", j.Engine)
	}
	for _, name := range j.Databases {
		if !ValidDBName(name) {
			return fmt.Errorf("имя базы %q вне допустимого алфавита: латиница, цифры, «_», «.», «-»", name)
		}
	}
	if j.IncludeGlobals && j.Engine != DBEnginePostgreSQL {
		return fmt.Errorf("глобальные объекты (роли) снимаются только для PostgreSQL")
	}
	if len(j.StorageTargetIDs) == 0 {
		return fmt.Errorf("выберите хотя бы одно хранилище")
	}
	return j.Retention.Validate()
}

// DBDumpEntry — одна база (или глобальные объекты) внутри запуска.
type DBDumpEntry struct {
	Database string `json:"database"`
	// Kind — database или globals.
	Kind string `json:"kind"`
	// Format — custom (pg_dump -Fc) или sql (mysqldump, pg_dumpall).
	Format       string `json:"format"`
	LogicalBytes int64  `json:"logical_bytes"`
	StoredBytes  int64  `json:"stored_bytes"`
	Error        string `json:"error,omitempty"`
}

// DBDumpRun — один запуск задания в одном хранилище.
type DBDumpRun struct {
	ID              string        `json:"id"`
	JobID           string        `json:"job_id"`
	JobName         string        `json:"job_name,omitempty"`
	HostID          string        `json:"host_id"`
	Engine          DBEngine      `json:"engine"`
	StorageTargetID string        `json:"storage_target_id"`
	Status          RunStatus     `json:"status"`
	ManifestKey     string        `json:"manifest_key,omitempty"`
	ServerVersion   string        `json:"server_version,omitempty"`
	Entries         []DBDumpEntry `json:"entries,omitempty"`
	LogicalBytes    int64         `json:"logical_bytes"`
	StoredBytes     int64         `json:"stored_bytes"`
	Encrypted       bool          `json:"encrypted"`
	Error           string        `json:"error,omitempty"`
	// VerifyStatus — итог последней проверки точки; пусто — не проверялась.
	VerifyStatus RunStatus  `json:"verify_status,omitempty"`
	VerifyError  string     `json:"verify_error,omitempty"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}
