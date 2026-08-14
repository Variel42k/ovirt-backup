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

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/dispatch"
	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/logging"
	"adveng/jh_virt/internal/monitor"
	"adveng/jh_virt/internal/ovirt"
	"adveng/jh_virt/internal/quality"
	"adveng/jh_virt/internal/scheduler"
	"adveng/jh_virt/internal/store"
)

// Server holds the API dependencies.
type Server struct {
	cfg          config.Config
	baseCfg      config.Config
	store        *store.Store
	pool         *ovirt.Pool
	libvirt      *libvirtx.Pool
	engine       *dispatch.Dispatcher
	scheduler    *scheduler.Scheduler
	monitor      *monitor.Monitor
	remediator   *monitor.Remediator
	bus          *events.Bus
	log          zerolog.Logger
	logs         *logging.Manager
	quality      *quality.Service
	metricsToken []byte
	// logins притормаживает подбор пароля. В памяти, а не в базе: ограничение
	// действует на процесс, переживать перезапуск ему не нужно, а запись в
	// базу на каждую неудачную попытку сама стала бы точкой приложения силы.
	logins *loginLimiter
}

// Deps bundles what the API needs, so adding a dependency does not change
// every call site.
type Deps struct {
	Config config.Config
	// BaseConfig is the YAML/environment value before database overrides. It
	// is the target of the runtime-settings reset endpoints.
	BaseConfig  config.Config
	Store       *store.Store
	Pool        *ovirt.Pool
	LibvirtPool *libvirtx.Pool
	Engine      *dispatch.Dispatcher
	Scheduler   *scheduler.Scheduler
	Monitor     *monitor.Monitor
	Remediator  *monitor.Remediator
	Bus         *events.Bus
	Logger      zerolog.Logger
	Logs        *logging.Manager
	Quality     *quality.Service
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
	return &Server{
		cfg: d.Config, baseCfg: base, store: d.Store, pool: d.Pool, libvirt: d.LibvirtPool, engine: d.Engine,
		scheduler: d.Scheduler, monitor: d.Monitor, remediator: d.Remediator,
		bus: d.Bus, log: d.Logger, logs: d.Logs, quality: d.Quality, metricsToken: metricsToken,
		logins: newLoginLimiter(),
	}
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

	if s.cfg.Server.ServeSPA {
		mux.Handle("/", s.spaHandler())
	}

	return s.recoverer(s.requestLogger(s.cors(s.authenticate(mux))))
}

// routes registers the API surface. Paths here are relative to /api/v1.
func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/me", s.handleMe)
	mux.HandleFunc("GET /meta", s.handleMeta)
	mux.HandleFunc("GET /help", s.handleHelp)
	mux.HandleFunc("GET /dashboard", s.handleDashboard)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /audit", s.admin(s.handleAudit))

	// Подключения к движкам.
	mux.HandleFunc("GET /servers", s.handleListServers)
	mux.HandleFunc("POST /servers", s.admin(s.handleCreateServer))
	mux.HandleFunc("GET /servers/{id}", s.handleGetServer)
	mux.HandleFunc("PUT /servers/{id}", s.admin(s.handleUpdateServer))
	mux.HandleFunc("DELETE /servers/{id}", s.admin(s.handleDeleteServer))
	mux.HandleFunc("POST /servers/probe", s.admin(s.handleProbeServer))
	mux.HandleFunc("POST /servers/ca-certificate", s.admin(s.handleFetchCA))
	mux.HandleFunc("POST /servers/{id}/refresh", s.writer(s.handleRefreshServer))
	mux.HandleFunc("GET /servers/{id}/summary", s.handleServerSummary)

	// Инвентарь.
	mux.HandleFunc("GET /servers/{id}/clusters", s.handleListClusters)
	mux.HandleFunc("GET /servers/{id}/hosts", s.handleListHosts)
	mux.HandleFunc("GET /servers/{id}/vms", s.handleListVMs)
	mux.HandleFunc("GET /servers/{id}/vms/{vmID}", s.handleGetVM)
	mux.HandleFunc("GET /servers/{id}/vms/{vmID}/disks", s.handleListVMDisks)
	mux.HandleFunc("GET /servers/{id}/disks", s.handleListDisks)
	mux.HandleFunc("GET /servers/{id}/storage-domains", s.handleListStorageDomains)

	// Управление ВМ и хостами.
	mux.HandleFunc("POST /servers/{id}/vms/{vmID}/action", s.writer(s.handleVMAction))
	mux.HandleFunc("PUT /servers/{id}/vms/{vmID}/policy", s.writer(s.handleVMPolicy))
	mux.HandleFunc("POST /servers/{id}/hosts/{hostID}/action", s.writer(s.handleHostAction))
	mux.HandleFunc("PUT /servers/{id}/disks/{diskID}/backup-mode", s.writer(s.handleDiskBackupMode))

	// Планирование бэкапа для конкретной ВМ.
	mux.HandleFunc("GET /servers/{id}/vms/{vmID}/backup-options", s.handleBackupOptions)

	// Хранилища бэкапов.
	mux.HandleFunc("GET /storages", s.handleListStorages)
	mux.HandleFunc("POST /storages", s.admin(s.handleCreateStorage))
	mux.HandleFunc("GET /storages/{id}", s.handleGetStorage)
	mux.HandleFunc("PUT /storages/{id}", s.admin(s.handleUpdateStorage))
	mux.HandleFunc("DELETE /storages/{id}", s.admin(s.handleDeleteStorage))
	mux.HandleFunc("POST /storages/{id}/check", s.writer(s.handleCheckStorage))

	// Задания бэкапа.
	mux.HandleFunc("GET /jobs", s.handleListJobs)
	mux.HandleFunc("POST /jobs", s.writer(s.handleCreateJob))
	mux.HandleFunc("GET /jobs/{id}", s.handleGetJob)
	mux.HandleFunc("PUT /jobs/{id}", s.writer(s.handleUpdateJob))
	mux.HandleFunc("DELETE /jobs/{id}", s.writer(s.handleDeleteJob))
	mux.HandleFunc("POST /jobs/{id}/run", s.writer(s.handleRunJob))
	mux.HandleFunc("GET /jobs/{id}/preview", s.handlePreviewJob)

	// Запуски бэкапа.
	mux.HandleFunc("POST /backups", s.writer(s.handleAdHocBackup))
	mux.HandleFunc("GET /backups", s.handleListRuns)
	mux.HandleFunc("GET /backups/{id}", s.handleGetRun)
	mux.HandleFunc("DELETE /backups/{id}", s.writer(s.handleDeleteRun))
	mux.HandleFunc("POST /backups/{id}/cancel", s.writer(s.handleCancelRun))
	mux.HandleFunc("GET /backups/{id}/chain", s.handleRunChain)

	// Проверка и восстановление.
	mux.HandleFunc("POST /backups/{id}/verify", s.writer(s.handleVerifyRun))
	mux.HandleFunc("GET /verifications", s.handleListVerifications)
	mux.HandleFunc("GET /verifications/{id}", s.handleGetVerification)
	mux.HandleFunc("POST /backups/{id}/restore", s.writer(s.handleRestore))
	mux.HandleFunc("GET /restores", s.handleListRestores)
	mux.HandleFunc("GET /restores/{id}", s.handleGetRestore)

	// Ретенция.
	mux.HandleFunc("POST /retention/preview", s.handleRetentionPreview)
	mux.HandleFunc("POST /retention/apply", s.writer(s.handleRetentionApply))

	// Мониторинг.
	mux.HandleFunc("GET /alerts", s.handleListAlerts)
	mux.HandleFunc("POST /alerts/{id}/ack", s.writer(s.handleAckAlert))
	mux.HandleFunc("GET /remediations", s.handleListRemediations)
	mux.HandleFunc("GET /remediation/mode", s.handleGetRemediationMode)
	mux.HandleFunc("PUT /remediation/mode", s.admin(s.handleSetRemediationMode))
	mux.HandleFunc("GET /remediation/periods/{id}/archive", s.handleGetRemediationArchive)
	mux.HandleFunc("POST /remediations", s.writer(s.handleManualRemediation))
	mux.HandleFunc("GET /coverage", s.handleCoverage)
	mux.HandleFunc("GET /monitoring/backup-quality", s.handleBackupQuality)
	mux.HandleFunc("GET /monitoring/backup-series", s.handleBackupSeries)
	mux.HandleFunc("GET /monitoring/storage-capacity", s.handleStorageCapacity)
	mux.HandleFunc("GET /job-runs", s.handleListJobRuns)
	mux.HandleFunc("GET /health-samples", s.handleHealthSamples)
	mux.HandleFunc("GET /disk-samples", s.handleDiskSamples)
	mux.HandleFunc("GET /mount-samples", s.handleMountSamples)
	mux.HandleFunc("GET /servers/{id}/storage-paths", s.handleStoragePaths)

	// Журнал службы. Только администратор: журнал не содержит секретов, но
	// перечисляет все ВМ, хосты и операторов установки.
	mux.HandleFunc("GET /logs", s.admin(s.handleLogStatus))
	mux.HandleFunc("GET /logs/tail", s.admin(s.handleLogTail))
	mux.HandleFunc("PUT /logs/level", s.admin(s.handleSetLogLevel))
	mux.HandleFunc("POST /logs/rotate", s.admin(s.handleRotateLog))

	// Параметры, которые применяются без перезапуска и переживают его в БД.
	mux.HandleFunc("GET /settings/runtime", s.admin(s.handleRuntimeSettings))
	mux.HandleFunc("PUT /settings/runtime/compression", s.admin(s.handleSetRuntimeCompression))
	mux.HandleFunc("DELETE /settings/runtime/compression", s.admin(s.handleResetRuntimeCompression))
	mux.HandleFunc("PUT /settings/runtime/timezone", s.admin(s.handleSetRuntimeTimezone))
	mux.HandleFunc("DELETE /settings/runtime/timezone", s.admin(s.handleResetRuntimeTimezone))
	mux.HandleFunc("PUT /settings/runtime/log-rotation", s.admin(s.handleSetRuntimeLogRotation))
	mux.HandleFunc("DELETE /settings/runtime/log-rotation", s.admin(s.handleResetRuntimeLogRotation))
	mux.HandleFunc("PUT /settings/runtime/backup-quality", s.admin(s.handleSetRuntimeBackupQuality))
	mux.HandleFunc("DELETE /settings/runtime/backup-quality", s.admin(s.handleResetRuntimeBackupQuality))

	// Пользователи.
	mux.HandleFunc("GET /users", s.admin(s.handleListUsers))
	mux.HandleFunc("POST /users", s.admin(s.handleCreateUser))
	mux.HandleFunc("PUT /users/{id}", s.admin(s.handleUpdateUser))
	mux.HandleFunc("DELETE /users/{id}", s.admin(s.handleDeleteUser))
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
