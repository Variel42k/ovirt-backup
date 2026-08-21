// Package api exposes the REST interface the web UI and any integration
// speaks to.
package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/dispatch"
	drcheck "github.com/Variel42k/ovirt-backup/internal/dr"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/filebackup"
	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/logging"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/monitor"
	"github.com/Variel42k/ovirt-backup/internal/notify"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/quality"
	"github.com/Variel42k/ovirt-backup/internal/replication"
	"github.com/Variel42k/ovirt-backup/internal/scheduler"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Server holds the API dependencies.
type Server struct {
	cfg           config.Config
	baseCfg       config.Config
	store         *store.Store
	pool          *ovirt.Pool
	libvirt       *libvirtx.Pool
	engine        *dispatch.Dispatcher
	scheduler     *scheduler.Scheduler
	monitor       *monitor.Monitor
	remediator    *monitor.Remediator
	bus           *events.Bus
	log           zerolog.Logger
	logs          *logging.Manager
	quality       *quality.Service
	replicator    *replication.Replicator
	notifier      *notify.Notifier
	notifications *notify.Manager
	dr            *drcheck.Checker
	fileBackup    *filebackup.Engine
	metricsToken  []byte
	// logins притормаживает подбор пароля. В памяти, а не в базе: ограничение
	// действует на процесс, переживать перезапуск ему не нужно, а запись в
	// базу на каждую неудачную попытку сама стала бы точкой приложения силы.
	logins *loginLimiter
	// oidc пуст, когда внешний вход выключен: по этому полю обработчики и
	// страница входа узнают, есть ли вторая дверь.
	oidc *oidcClient
	// oidcLogins помнит начатые внешние входы до возврата от провайдера.
	oidcLogins *oidcPending
	// roles кеширует настраиваемые роли: проверяются они на каждом запросе,
	// а меняются редко.
	roles *roleCache
}

// Deps bundles what the API needs, so adding a dependency does not change
// every call site.
type Deps struct {
	Config config.Config
	// BaseConfig is the YAML/environment value before database overrides. It
	// is the target of the runtime-settings reset endpoints.
	BaseConfig    config.Config
	Store         *store.Store
	Pool          *ovirt.Pool
	LibvirtPool   *libvirtx.Pool
	Engine        *dispatch.Dispatcher
	Scheduler     *scheduler.Scheduler
	Monitor       *monitor.Monitor
	Remediator    *monitor.Remediator
	Bus           *events.Bus
	Logger        zerolog.Logger
	Logs          *logging.Manager
	Quality       *quality.Service
	Replicator    *replication.Replicator
	Notifier      *notify.Notifier
	Notifications *notify.Manager
	DR            *drcheck.Checker
	FileBackup    *filebackup.Engine
}

// New builds the API server.
func New(d Deps) *Server {
	base := d.BaseConfig
	if base.Backup.Compression == "" {
		base = d.Config
	}
	if d.Quality == nil {
		d.Quality = quality.New(d.Store, d.Config.Monitor.BackupQuality, d.Config.Location())
	}
	var metricsToken []byte
	if d.Config.Metrics.Enabled {
		body, _ := os.ReadFile(d.Config.Metrics.TokenFile)
		metricsToken = bytes.TrimSpace(body)
	}
	srv := &Server{
		cfg: d.Config, baseCfg: base, store: d.Store, pool: d.Pool, libvirt: d.LibvirtPool, engine: d.Engine,
		scheduler: d.Scheduler, monitor: d.Monitor, remediator: d.Remediator,
		bus: d.Bus, log: d.Logger, logs: d.Logs, quality: d.Quality, replicator: d.Replicator, notifier: d.Notifier,
		notifications: d.Notifications,
		dr:            d.DR, metricsToken: metricsToken,
		fileBackup: d.FileBackup,
		logins:     newLoginLimiter(),
		oidcLogins: newOIDCPending(),
		roles:      newRoleCache(),
	}
	// Про токены из файла настроек служба говорит при каждом старте, а не
	// один раз в документации. Забывают именно их: работают они бессрочно и с
	// правами администратора, а в журнале аудита неотличимы друг от друга.
	if n := len(d.Config.Auth.APITokens); n > 0 {
		d.Logger.Warn().Int("количество", n).Msg(
			"токены из auth.api_tokens устарели: каждый даёт права администратора, " +
				"не имеет срока и отзывается только перезапуском. Выпустите именные " +
				"токены запросом POST /api/v1/api-tokens и удалите список из настроек")
	}

	if d.Config.Auth.Enabled && d.Config.Auth.OIDC.Enabled {
		// Проверка настроек уже прошла в config.Validate; здесь остаётся
		// только запомнить их. К провайдеру обращаемся при первом входе.
		srv.oidc = newOIDCClient(d.Config.Auth.OIDC)
	}
	return srv
}

// Handler builds the complete HTTP handler: API routes, then the SPA.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health endpoints stay outside authentication so a load balancer can
	// reach them without credentials.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", s.metricsHandler())

	api := http.NewServeMux()
	s.routes(api)

	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", s.requireAuth(api)))
	// Login must be reachable without a session.
	mux.Handle("POST /api/v1/auth/login", http.HandlerFunc(s.handleLogin))
	mux.Handle("POST /api/v1/auth/logout", http.HandlerFunc(s.handleLogout))
	// Внешний вход — тоже до всякой сессии, ради неё он и затевается. Info
	// отвечает всегда: странице входа нужно знать, рисовать ли кнопку.
	mux.Handle("GET /api/v1/auth/oidc/info", http.HandlerFunc(s.handleOIDCInfo))
	mux.Handle("GET /api/v1/auth/oidc/start", http.HandlerFunc(s.handleOIDCStart))
	mux.Handle("GET /api/v1/auth/oidc/callback", http.HandlerFunc(s.handleOIDCCallback))

	if s.cfg.Server.ServeSPA {
		mux.Handle("/", s.spaHandler())
	}

	return s.recoverer(s.requestLogger(s.cors(s.authenticate(mux))))
}

// routes registers the API surface. Paths here are relative to /api/v1.
func (s *Server) routes(mux *http.ServeMux) {
	// Кто вошёл, что умеет эта сборка и справка. Без права: интерфейсу нужно
	// это, чтобы вообще отрисоваться, а роль с пустым набором прав всё равно
	// не увидит ни одного раздела.
	mux.HandleFunc("GET /auth/me", s.handleMe)
	mux.HandleFunc("GET /meta", s.handleMeta)
	mux.HandleFunc("GET /help", s.handleHelp)

	mux.HandleFunc("GET /dashboard", s.perm(model.PermMonitoringRead, s.handleDashboard))
	mux.HandleFunc("GET /events", s.perm(model.PermMonitoringRead, s.handleEvents))
	mux.HandleFunc("GET /audit", s.perm(model.PermAuditRead, s.handleAudit))

	// Подключения к движкам.
	mux.HandleFunc("GET /servers", s.perm(model.PermServersRead, s.handleListServers))
	mux.HandleFunc("POST /servers", s.perm(model.PermServersAdmin, s.handleCreateServer))
	mux.HandleFunc("GET /servers/{id}", s.perm(model.PermServersRead, s.handleGetServer))
	mux.HandleFunc("PUT /servers/{id}", s.perm(model.PermServersAdmin, s.handleUpdateServer))
	mux.HandleFunc("DELETE /servers/{id}", s.perm(model.PermServersAdmin, s.handleDeleteServer))
	mux.HandleFunc("POST /servers/probe", s.perm(model.PermServersAdmin, s.handleProbeServer))
	mux.HandleFunc("POST /servers/ca-certificate", s.perm(model.PermServersAdmin, s.handleFetchCA))
	mux.HandleFunc("POST /servers/{id}/refresh", s.perm(model.PermServersWrite, s.handleRefreshServer))
	mux.HandleFunc("GET /servers/{id}/summary", s.perm(model.PermServersRead, s.handleServerSummary))

	// Инвентарь.
	mux.HandleFunc("GET /servers/{id}/clusters", s.perm(model.PermServersRead, s.handleListClusters))
	mux.HandleFunc("GET /servers/{id}/hosts", s.perm(model.PermServersRead, s.handleListHosts))
	mux.HandleFunc("GET /servers/{id}/vms", s.perm(model.PermServersRead, s.handleListVMs))
	mux.HandleFunc("GET /servers/{id}/vms/{vmID}", s.perm(model.PermServersRead, s.handleGetVM))
	mux.HandleFunc("GET /servers/{id}/vms/{vmID}/disks", s.perm(model.PermServersRead, s.handleListVMDisks))
	mux.HandleFunc("GET /servers/{id}/disks", s.perm(model.PermServersRead, s.handleListDisks))
	mux.HandleFunc("GET /servers/{id}/storage-domains", s.perm(model.PermServersRead, s.handleListStorageDomains))
	mux.HandleFunc("GET /servers/{id}/restore-networks", s.perm(model.PermServersRead, s.handleListRestoreNetworks))

	// Управление ВМ и хостами.
	mux.HandleFunc("POST /servers/{id}/vms/{vmID}/action", s.perm(model.PermServersWrite, s.handleVMAction))
	mux.HandleFunc("PUT /servers/{id}/vms/{vmID}/policy", s.perm(model.PermServersWrite, s.handleVMPolicy))
	mux.HandleFunc("PUT /servers/{id}/vms/{vmID}/tags", s.perm(model.PermServersWrite, s.handleVMTags))
	mux.HandleFunc("POST /servers/{id}/hosts/{hostID}/action", s.perm(model.PermServersWrite, s.handleHostAction))
	mux.HandleFunc("PUT /servers/{id}/disks/{diskID}/backup-mode", s.perm(model.PermServersWrite, s.handleDiskBackupMode))

	// Планирование бэкапа для конкретной ВМ.
	mux.HandleFunc("GET /servers/{id}/vms/{vmID}/backup-options", s.perm(model.PermJobsRead, s.handleBackupOptions))

	// Хранилища бэкапов.
	mux.HandleFunc("GET /storages", s.perm(model.PermStoragesRead, s.handleListStorages))
	mux.HandleFunc("POST /storages", s.perm(model.PermStoragesAdmin, s.handleCreateStorage))
	mux.HandleFunc("GET /storages/{id}", s.perm(model.PermStoragesRead, s.handleGetStorage))
	mux.HandleFunc("PUT /storages/{id}", s.perm(model.PermStoragesAdmin, s.handleUpdateStorage))
	mux.HandleFunc("DELETE /storages/{id}", s.perm(model.PermStoragesAdmin, s.handleDeleteStorage))
	mux.HandleFunc("POST /storages/{id}/check", s.perm(model.PermStoragesWrite, s.handleCheckStorage))
	mux.HandleFunc("POST /storages/{id}/catalog-scans", s.perm(model.PermStoragesAdmin, s.handleStartCatalogScan))
	mux.HandleFunc("GET /storages/{id}/catalog-scans", s.perm(model.PermStoragesAdmin, s.handleListCatalogScans))
	mux.HandleFunc("GET /catalog-scans/{id}", s.perm(model.PermStoragesAdmin, s.handleGetCatalogScan))
	mux.HandleFunc("POST /catalog-scans/{id}/import", s.perm(model.PermStoragesAdmin, s.handleImportCatalogScan))

	// Задания бэкапа.
	mux.HandleFunc("GET /jobs", s.perm(model.PermJobsRead, s.handleListJobs))
	mux.HandleFunc("POST /jobs", s.perm(model.PermJobsWrite, s.handleCreateJob))
	mux.HandleFunc("GET /jobs/{id}", s.perm(model.PermJobsRead, s.handleGetJob))
	mux.HandleFunc("PUT /jobs/{id}", s.perm(model.PermJobsWrite, s.handleUpdateJob))
	mux.HandleFunc("DELETE /jobs/{id}", s.perm(model.PermJobsWrite, s.handleDeleteJob))
	mux.HandleFunc("POST /jobs/{id}/run", s.perm(model.PermJobsWrite, s.handleRunJob))
	mux.HandleFunc("GET /jobs/{id}/preview", s.perm(model.PermJobsRead, s.handlePreviewJob))
	mux.HandleFunc("POST /jobs/{id}/enable-replication", s.perm(model.PermJobsAdmin, s.handleEnableJobReplication))
	mux.HandleFunc("POST /jobs/{id}/change-primary", s.perm(model.PermJobsAdmin, s.handleChangeJobPrimary))

	// Запуски бэкапа.
	mux.HandleFunc("POST /backups", s.perm(model.PermBackupsWrite, s.handleAdHocBackup))
	mux.HandleFunc("GET /backups", s.perm(model.PermBackupsRead, s.handleListRuns))
	mux.HandleFunc("GET /backups/{id}", s.perm(model.PermBackupsRead, s.handleGetRun))
	mux.HandleFunc("DELETE /backups/{id}", s.perm(model.PermBackupsWrite, s.handleDeleteRun))
	mux.HandleFunc("POST /backups/{id}/cancel", s.perm(model.PermBackupsWrite, s.handleCancelRun))
	mux.HandleFunc("GET /backups/{id}/chain", s.perm(model.PermBackupsRead, s.handleRunChain))
	mux.HandleFunc("GET /backups/{id}/copies", s.perm(model.PermBackupsRead, s.handleListBackupCopies))
	mux.HandleFunc("GET /backups/{id}/artifacts", s.perm(model.PermBackupsRead, s.handleListRepositoryArtifacts))
	mux.HandleFunc("POST /backups/{id}/copies", s.perm(model.PermBackupsWrite, s.handleCreateBackupCopy))
	mux.HandleFunc("POST /backup-copies/{id}/retry", s.perm(model.PermBackupsWrite, s.handleRetryBackupCopy))
	mux.HandleFunc("POST /backup-copies/{id}/cancel", s.perm(model.PermBackupsWrite, s.handleCancelBackupCopy))
	mux.HandleFunc("GET /replications", s.perm(model.PermBackupsRead, s.handleListReplications))
	mux.HandleFunc("GET /replications/{id}", s.perm(model.PermBackupsRead, s.handleGetReplication))

	// Проверка и восстановление.
	mux.HandleFunc("POST /backups/{id}/verify", s.perm(model.PermBackupsWrite, s.handleVerifyRun))
	mux.HandleFunc("GET /verifications", s.perm(model.PermBackupsRead, s.handleListVerifications))
	mux.HandleFunc("GET /verifications/{id}", s.perm(model.PermBackupsRead, s.handleGetVerification))
	mux.HandleFunc("POST /backups/{id}/restore", s.perm(model.PermBackupsWrite, s.handleRestore))
	// Предпросмотр ничего не создаёт, но читает состав копии и место в домене
	// хранения, поэтому закрыт теми же правами, что и сама сборка.
	mux.HandleFunc("POST /backups/{id}/restore-vm/plan", s.perm(model.PermBackupsWrite, s.handlePlanRestoreVM))
	mux.HandleFunc("POST /backups/{id}/restore-vm", s.perm(model.PermBackupsWrite, s.handleRestoreVM))
	mux.HandleFunc("GET /restores", s.perm(model.PermBackupsRead, s.handleListRestores))
	mux.HandleFunc("GET /restores/{id}", s.perm(model.PermBackupsRead, s.handleGetRestore))

	// Ретенция — это удаление копий, поэтому право то же, что на их удаление.
	mux.HandleFunc("POST /retention/preview", s.perm(model.PermBackupsRead, s.handleRetentionPreview))
	mux.HandleFunc("POST /retention/apply", s.perm(model.PermBackupsWrite, s.handleRetentionApply))

	// Оповещения.
	mux.HandleFunc("GET /alerts", s.perm(model.PermAlertsRead, s.handleListAlerts))
	mux.HandleFunc("POST /alerts/{id}/ack", s.perm(model.PermAlertsWrite, s.handleAckAlert))
	mux.HandleFunc("POST /alerts/{id}/notifications", s.perm(model.PermAlertsWrite, s.handleAlertNotifications))
	mux.HandleFunc("GET /settings/notifications", s.perm(model.PermAlertsAdmin, s.handleGetNotificationSettings))
	mux.HandleFunc("PUT /settings/notifications", s.perm(model.PermAlertsAdmin, s.handleSetNotificationSettings))
	mux.HandleFunc("DELETE /settings/notifications", s.perm(model.PermAlertsAdmin, s.handleResetNotificationSettings))
	mux.HandleFunc("GET /notification-deliveries", s.perm(model.PermAlertsAdmin, s.handleListNotificationDeliveries))

	// Авто-восстановление: смена режима меняет то, что служба делает сама, и
	// остаётся администраторским правом раздела оповещений.
	mux.HandleFunc("GET /remediations", s.perm(model.PermAlertsRead, s.handleListRemediations))
	mux.HandleFunc("GET /remediation/mode", s.perm(model.PermAlertsRead, s.handleGetRemediationMode))
	mux.HandleFunc("PUT /remediation/mode", s.perm(model.PermAlertsAdmin, s.handleSetRemediationMode))
	mux.HandleFunc("GET /remediation/periods/{id}/archive", s.perm(model.PermAlertsRead, s.handleGetRemediationArchive))
	mux.HandleFunc("POST /remediations", s.perm(model.PermAlertsWrite, s.handleManualRemediation))

	// Обзор, покрытие и показатели.
	mux.HandleFunc("GET /coverage", s.perm(model.PermMonitoringRead, s.handleCoverage))
	mux.HandleFunc("GET /monitoring/backup-quality", s.perm(model.PermMonitoringRead, s.handleBackupQuality))
	mux.HandleFunc("GET /monitoring/backup-series", s.perm(model.PermMonitoringRead, s.handleBackupSeries))
	mux.HandleFunc("GET /monitoring/storage-capacity", s.perm(model.PermMonitoringRead, s.handleStorageCapacity))
	mux.HandleFunc("GET /job-runs", s.perm(model.PermJobsRead, s.handleListJobRuns))
	mux.HandleFunc("GET /health-samples", s.perm(model.PermMonitoringRead, s.handleHealthSamples))
	mux.HandleFunc("GET /disk-samples", s.perm(model.PermMonitoringRead, s.handleDiskSamples))
	mux.HandleFunc("GET /mount-samples", s.perm(model.PermMonitoringRead, s.handleMountSamples))
	mux.HandleFunc("GET /servers/{id}/storage-paths", s.perm(model.PermServersRead, s.handleStoragePaths))

	// Аварийная готовность.
	mux.HandleFunc("GET /disaster-recovery/readiness", s.perm(model.PermDRRead, s.handleDRReadiness))
	mux.HandleFunc("POST /disaster-recovery/check", s.perm(model.PermDRAdmin, s.handleDRCheck))

	// Журнал службы. Секретов он не содержит, но перечисляет все ВМ, хосты и
	// операторов установки — поэтому у него своё право, а не общее «чтение».
	mux.HandleFunc("GET /logs", s.perm(model.PermLogsRead, s.handleLogStatus))
	mux.HandleFunc("GET /logs/tail", s.perm(model.PermLogsRead, s.handleLogTail))
	mux.HandleFunc("PUT /logs/level", s.perm(model.PermLogsAdmin, s.handleSetLogLevel))
	mux.HandleFunc("POST /logs/rotate", s.perm(model.PermLogsAdmin, s.handleRotateLog))

	// Параметры, которые применяются без перезапуска и переживают его в БД.
	mux.HandleFunc("GET /settings/runtime", s.perm(model.PermSettingsRead, s.handleRuntimeSettings))
	mux.HandleFunc("PUT /settings/runtime/compression", s.perm(model.PermSettingsAdmin, s.handleSetRuntimeCompression))
	mux.HandleFunc("DELETE /settings/runtime/compression", s.perm(model.PermSettingsAdmin, s.handleResetRuntimeCompression))
	mux.HandleFunc("PUT /settings/runtime/timezone", s.perm(model.PermSettingsAdmin, s.handleSetRuntimeTimezone))
	mux.HandleFunc("DELETE /settings/runtime/timezone", s.perm(model.PermSettingsAdmin, s.handleResetRuntimeTimezone))
	mux.HandleFunc("PUT /settings/runtime/log-rotation", s.perm(model.PermSettingsAdmin, s.handleSetRuntimeLogRotation))
	mux.HandleFunc("DELETE /settings/runtime/log-rotation", s.perm(model.PermSettingsAdmin, s.handleResetRuntimeLogRotation))
	mux.HandleFunc("PUT /settings/runtime/backup-quality", s.perm(model.PermSettingsAdmin, s.handleSetRuntimeBackupQuality))
	mux.HandleFunc("DELETE /settings/runtime/backup-quality", s.perm(model.PermSettingsAdmin, s.handleResetRuntimeBackupQuality))

	// Снимки конфигурации Engine.
	mux.HandleFunc("GET /engine-config/jobs", s.perm(model.PermEngineConfigRead, s.handleListEngineConfigJobs))
	mux.HandleFunc("POST /engine-config/jobs", s.perm(model.PermEngineConfigAdmin, s.handleCreateEngineConfigJob))
	mux.HandleFunc("PUT /engine-config/jobs/{id}", s.perm(model.PermEngineConfigAdmin, s.handleUpdateEngineConfigJob))
	mux.HandleFunc("DELETE /engine-config/jobs/{id}", s.perm(model.PermEngineConfigAdmin, s.handleDeleteEngineConfigJob))
	mux.HandleFunc("POST /engine-config/jobs/{id}/run", s.perm(model.PermEngineConfigAdmin, s.handleRunEngineConfigJob))
	mux.HandleFunc("GET /engine-config/runs", s.perm(model.PermEngineConfigRead, s.handleListEngineConfigRuns))
	mux.HandleFunc("POST /engine-config/runs", s.perm(model.PermEngineConfigAdmin, s.handleRunEngineConfig))
	mux.HandleFunc("GET /engine-config/runs/{id}", s.perm(model.PermEngineConfigRead, s.handleGetEngineConfigRun))
	mux.HandleFunc("GET /engine-config/runs/{id}/download", s.perm(model.PermEngineConfigRead, s.handleDownloadEngineConfig))
	mux.HandleFunc("GET /engine-config/compare", s.perm(model.PermEngineConfigRead, s.handleCompareEngineConfig))

	// Native file backups are isolated from VM jobs and guarded by the
	// file_backup.enabled feature gate.
	mux.HandleFunc("GET /file-backup/roots", s.perm(model.PermFileBackupsRead, s.handleListFileBackupRoots))
	mux.HandleFunc("GET /file-backup/jobs", s.perm(model.PermFileBackupsRead, s.handleListFileBackupJobs))
	mux.HandleFunc("POST /file-backup/jobs", s.perm(model.PermFileBackupsAdmin, s.handleCreateFileBackupJob))
	mux.HandleFunc("GET /file-backup/jobs/{id}", s.perm(model.PermFileBackupsRead, s.handleGetFileBackupJob))
	mux.HandleFunc("PUT /file-backup/jobs/{id}", s.perm(model.PermFileBackupsAdmin, s.handleUpdateFileBackupJob))
	mux.HandleFunc("DELETE /file-backup/jobs/{id}", s.perm(model.PermFileBackupsAdmin, s.handleDeleteFileBackupJob))
	mux.HandleFunc("POST /file-backup/jobs/{id}/run", s.perm(model.PermFileBackupsWrite, s.handleRunFileBackupJob))
	mux.HandleFunc("GET /file-backup/runs", s.perm(model.PermFileBackupsRead, s.handleListFileBackupRuns))
	mux.HandleFunc("GET /file-backup/runs/{id}", s.perm(model.PermFileBackupsRead, s.handleGetFileBackupRun))
	mux.HandleFunc("DELETE /file-backup/runs/{id}", s.perm(model.PermFileBackupsAdmin, s.handleDeleteFileBackupRun))
	mux.HandleFunc("GET /file-backup/runs/{id}/tree", s.perm(model.PermFileBackupsRead, s.handleGetFileBackupTree))
	mux.HandleFunc("POST /file-backup/runs/{id}/restore", s.perm(model.PermFileBackupsWrite, s.handleRestoreFiles))

	// Права, роли, учётные записи и токены — всё под одним правом.
	//
	// Разделять их не следует: кто может править роли, тот может выдать себе
	// любое другое право, а кто может выпустить токен — выдать его с любой
	// ролью. Раздельные права создавали бы видимость ограничения там, где его
	// нет.
	mux.HandleFunc("GET /permissions", s.perm(model.PermUsersAdmin, s.handlePermissionCatalog))
	mux.HandleFunc("GET /roles", s.perm(model.PermUsersAdmin, s.handleListRoles))
	mux.HandleFunc("POST /roles", s.perm(model.PermUsersAdmin, s.handleCreateRole))
	mux.HandleFunc("PUT /roles/{id}", s.perm(model.PermUsersAdmin, s.handleUpdateRole))
	mux.HandleFunc("DELETE /roles/{id}", s.perm(model.PermUsersAdmin, s.handleDeleteRole))

	mux.HandleFunc("GET /api-tokens", s.perm(model.PermUsersAdmin, s.handleListAPITokens))
	mux.HandleFunc("POST /api-tokens", s.perm(model.PermUsersAdmin, s.handleCreateAPIToken))
	mux.HandleFunc("PUT /api-tokens/{id}", s.perm(model.PermUsersAdmin, s.handleUpdateAPIToken))
	mux.HandleFunc("DELETE /api-tokens/{id}", s.perm(model.PermUsersAdmin, s.handleDeleteAPIToken))

	mux.HandleFunc("GET /users", s.perm(model.PermUsersAdmin, s.handleListUsers))
	mux.HandleFunc("POST /users", s.perm(model.PermUsersAdmin, s.handleCreateUser))
	mux.HandleFunc("PUT /users/{id}", s.perm(model.PermUsersAdmin, s.handleUpdateUser))
	mux.HandleFunc("DELETE /users/{id}", s.perm(model.PermUsersAdmin, s.handleDeleteUser))
}

// spaHandler serves the built single-page app, falling back to index.html so
// client-side routes survive a page reload.
func (s *Server) spaHandler() http.Handler {
	dir := s.cfg.Server.SPADir
	fileServer := http.FileServer(http.Dir(dir))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
			writeJSON(w, http.StatusNotFound, errorResponse{
				Error: "веб-интерфейс не собран: соберите web/ и положите результат в " + dir,
				Code:  "spa_missing",
			})
			return
		}

		// Оболочку приложения браузер обязан перепроверять при каждой загрузке.
		// В ней записаны имена файлов сборки, а те содержат хэш содержимого:
		// после обновления сохранённая в кэше оболочка просит куски, которых в
		// новой сборке уже нет. Ломается при этом не всё сразу — открытые
		// страницы лежат в памяти и работают, — а только те разделы, куда
		// пользователь ещё не заходил: их код подгружается лениво и молча
		// падает на 404. Выглядит это как «перестала работать кнопка», и связь
		// с обновлением не очевидна совершенно.
		//
		// no-cache, а не no-store: перепроверка почти бесплатна, ответ 304
		// оставляет файл в кэше и не гоняет его заново.
		serveShell := func() {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
		}

		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "." || clean == "/" {
			serveShell()
			return
		}
		target := filepath.Join(dir, clean)
		if info, err := os.Stat(target); err == nil && !info.IsDir() {
			// Обратная сторона того же правила: имя файла сборки меняется
			// вместе с содержимым, поэтому его можно держать в кэше сколь
			// угодно долго. Дешёвая перепроверка оболочки и вечный кэш для
			// всего остального — одно решение, разнесённое на две ветки.
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// A missing static file must be a 404, not the application shell.
		//
		// The fallback exists so that /backups reloads into the SPA instead of
		// 404ing. Applying it to a missing script instead hands the browser
		// HTML with a JavaScript content type, and the page dies with
		// "Unexpected token '<'" — which says nothing about the actual problem.
		// That happens for real after an update: a cached index.html asks for a
		// chunk that the new build no longer contains.
		if isStaticAsset(clean) {
			http.NotFound(w, r)
			return
		}
		serveShell()
	})
}

// isStaticAsset reports whether a path was meant to be a file rather than a
// client-side route.
//
// Client routes of this app carry no extension (/backups, /servers/<id>), while
// every built asset does. That is a heuristic, but the failure it prevents is
// far more confusing than the one it could cause.
func isStaticAsset(clean string) bool {
	if strings.HasPrefix(clean, "assets"+string(filepath.Separator)) || strings.HasPrefix(clean, "assets/") {
		return true
	}
	return filepath.Ext(clean) != ""
}

// cors allows the dev-mode SPA, which runs on its own port, to call the API.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := s.cfg.Server.CORSOrigins

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Only echo back an origin from the allow list: a wildcard would be
		// incompatible with cookie credentials anyway.
		if origin != "" && slices.Contains(allowed, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestLogger records API calls at debug level, and slow ones at info.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		elapsed := time.Since(started)
		event := s.log.Debug()
		if elapsed > 5*time.Second || rec.status >= 500 {
			event = s.log.Info()
		}
		event.Str("метод", r.Method).Str("путь", r.URL.Path).
			Int("код", rec.status).Dur("время", elapsed).Msg("api")
	})
}

// recoverer turns a handler panic into a 500 instead of killing the process.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error().Interface("паника", p).Str("путь", r.URL.Path).
					Msg("обработчик завершился аварийно")
				writeJSON(w, http.StatusInternalServerError, errorResponse{
					Error: "внутренняя ошибка сервера", Code: "panic",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.status = code
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so server-sent events keep working
// through this wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Error: "база данных недоступна", Detail: err.Error(), Code: "db_down",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"running_backups": s.scheduler.RunningCount(),
	})
}
