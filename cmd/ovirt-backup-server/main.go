// Command ovirt-backup-server manages oVirt engines and their forks: it polls
// their state, revives what it is allowed to revive, and runs the backups.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/api"
	"github.com/Variel42k/ovirt-backup/internal/auditlog"
	"github.com/Variel42k/ovirt-backup/internal/backup"
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
	"github.com/Variel42k/ovirt-backup/internal/secret"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "критическая ошибка: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config/ovirt-backup.yaml", "путь к файлу конфигурации")
	showVersion := flag.Bool("version", false, "показать версию и выйти")
	checkConfig := flag.Bool("check-config", false, "проверить конфигурацию и выйти")
	resetUser := flag.String("reset-password", "",
		"задать новый пароль учётной записи (укажите имя пользователя) и выйти; "+
			"новый пароль берётся из JHV_NEW_PASSWORD либо генерируется и печатается")
	recoveryTokenFile := flag.String("recovery-token-file", "",
		"защищённый recovery-токен для -reset-password; '-' читает токен из stdin")
	revokeAllAccess := flag.Bool("revoke-all-access", false,
		"при -reset-password закрыть все сессии и отозвать API-токены и делегирования из БД")
	flag.Parse()

	if *showVersion {
		fmt.Printf("ovirt-backup-server %s\n", version)
		return nil
	}

	// A missing config file is not fatal: the built-in defaults plus JHV_
	// environment variables are enough to start, which is what a container
	// deployment wants.
	if _, err := os.Stat(*configPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("доступ к конфигурации %s: %w", *configPath, err)
		}
		fmt.Fprintf(os.Stderr, "конфигурация %s не найдена, используются значения по умолчанию и переменные окружения JHV_*\n", *configPath)
		*configPath = ""
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *resetUser == "" {
		if *recoveryTokenFile != "" || *revokeAllAccess {
			return fmt.Errorf("-recovery-token-file и -revoke-all-access допустимы только вместе с -reset-password")
		}
	} else {
		if *recoveryTokenFile == "" {
			return fmt.Errorf("-reset-password требует -recovery-token-file; запускайте восстановление с хоста")
		}
		if *revokeAllAccess && len(cfg.Auth.APITokens) > 0 {
			return fmt.Errorf("auth.api_tokens содержит статические токены, которые нельзя отозвать в БД; " +
				"удалите их из конфигурации и повторите восстановление")
		}
		reader, closeReader, err := openRecoveryToken(*recoveryTokenFile)
		if err != nil {
			return err
		}
		if closeReader != nil {
			defer closeReader()
		}
		if err := api.VerifyRecoveryToken(cfg.Auth.RecoveryTokenHash, reader); err != nil {
			return fmt.Errorf("проверка права на восстановление: %w", err)
		}
	}
	if *checkConfig {
		fmt.Println("конфигурация корректна")
		return nil
	}
	// Keep the startup values separately: DELETE runtime-setting returns to
	// YAML/environment even after the effective config is overlaid from DB.
	baseCfg := *cfg

	log, logs, err := logging.Setup(cfg.Logging)
	if err != nil {
		return err
	}
	if err := logs.SetTimezone(cfg.Scheduler.Timezone); err != nil {
		return err
	}
	// Суточная смена файла поверх ротации по размеру: на тихой установке файл
	// может никогда не дорасти до предела, а значит и не смениться — и тогда
	// max_age_days к нему не применяется, потому что чистит он архивы.
	rotateDone := make(chan struct{})
	defer close(rotateDone)
	// Закрываем файл последним: до этого момента в него ещё пишут строки о
	// завершении работы.
	defer func() { _ = logs.Close() }()
	log.Info().Str("версия", version).Str("конфигурация", *configPath).Msg("ovirt-backup-server запускается")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	warnLeftoverSQLite(cfg, log)

	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("подключение к базе данных: %w", err)
	}
	defer db.Close()
	log.Info().
		Str("подключение", cfg.Database.Target()).
		Msg("база данных подключена")

	if cfg.Database.RunMigrationsOnStartup {
		if err := db.Migrate(ctx); err != nil {
			return fmt.Errorf("применение миграций: %w", err)
		}
		log.Info().Msg("миграции применены")
	}

	cipher, err := secret.NewFromConfig(cfg.Secrets)
	if err != nil {
		return fmt.Errorf("ключ шифрования секретов: %w", err)
	}
	st := store.New(db, cipher)

	// Recovery is deliberately completed before any scheduler, notification,
	// DR or replication work starts. The one-off process has one job and exits.
	if *resetUser != "" {
		password, revoked, err := api.RecoverLocalAccess(ctx, st, *resetUser,
			os.Getenv("JHV_NEW_PASSWORD"), *revokeAllAccess)
		if err != nil {
			return err
		}
		printCredentials(*resetUser, password, "ПАРОЛЬ ИЗМЕНЁН")
		if *revokeAllAccess {
			fmt.Fprintf(os.Stderr,
				"Отозвано: сессий %d, API-токенов %d, делегирований %d.\n",
				revoked.Sessions, revoked.APITokens, revoked.Delegations)
		}
		return nil
	}

	// Оповещения наружу. Подписка ставится до запуска монитора и планировщика:
	// иначе первые же беды, найденные при старте, останутся без сообщения.
	notifier := notify.NewSender(cfg.Notifications, log)
	defer notifier.Close()
	if notifier != nil {
		if err := notifier.SetTimezone(cfg.Scheduler.Timezone); err != nil {
			return err
		}
	}
	drChecker := drcheck.New(cfg.DisasterRecovery, cfg.Secrets.KeyFile, st)
	drCtx, stopDR := context.WithCancel(ctx)
	drDone := make(chan struct{})
	go func() {
		defer close(drDone)
		drChecker.Run(drCtx)
	}()
	defer func() {
		stopDR()
		<-drDone
	}()
	runtimeSettings, err := st.RuntimeSettings(ctx)
	if err != nil {
		return fmt.Errorf("загрузка настроек времени выполнения: %w", err)
	}
	if runtimeSettings.SchedulerTimezone != nil {
		timezone := strings.TrimSpace(*runtimeSettings.SchedulerTimezone)
		if _, err := time.LoadLocation(timezone); err != nil {
			return fmt.Errorf("часовой пояс в runtime_settings %q: %w", timezone, err)
		}
		cfg.Scheduler.Timezone = timezone
		if err := logs.SetTimezone(timezone); err != nil {
			return err
		}
		if notifier != nil {
			if err := notifier.SetTimezone(timezone); err != nil {
				return err
			}
		}
		log.Info().Str("часовой пояс", timezone).
			Msg("системный часовой пояс загружен из базы данных")
	}
	if runtimeSettings.BackupCompression != nil {
		if !backup.KnownCompression(*runtimeSettings.BackupCompression) {
			return fmt.Errorf("неизвестное сжатие в runtime_settings: %q", *runtimeSettings.BackupCompression)
		}
		cfg.Backup.Compression = *runtimeSettings.BackupCompression
		log.Info().Str("алгоритм", cfg.Backup.Compression).
			Msg("сжатие новых бэкапов загружено из базы данных")
	}
	if runtimeSettings.HasLogRotation() {
		if err := logs.UpdateRotation(*runtimeSettings.LogMaxSizeMB,
			*runtimeSettings.LogMaxBackups, *runtimeSettings.LogMaxAgeDays); err != nil {
			return fmt.Errorf("ротация журнала из базы данных: %w", err)
		}
		log.Info().Int("размер_МиБ", *runtimeSettings.LogMaxSizeMB).
			Int("архивов", *runtimeSettings.LogMaxBackups).
			Int("дней", *runtimeSettings.LogMaxAgeDays).
			Msg("ротация журнала загружена из базы данных")
	} else if runtimeSettings.LogMaxSizeMB != nil || runtimeSettings.LogMaxBackups != nil ||
		runtimeSettings.LogMaxAgeDays != nil {
		log.Warn().Msg("неполная настройка ротации в базе проигнорирована; используются значения конфигурации")
	}
	if runtimeSettings.HasBackupQuality() {
		cfg.Monitor.BackupQuality = runtimeSettings.BackupQuality()
		if err := cfg.Monitor.BackupQuality.Validate(); err != nil {
			return fmt.Errorf("настройки качества бэкапов из базы данных: %w", err)
		}
		log.Info().Msg("пороги качества бэкапов загружены из базы данных")
	} else if runtimeSettings.QualityStaleIntervals != nil || runtimeSettings.QualityVerifyMaxAgeDays != nil {
		log.Warn().Msg("неполные настройки качества в базе проигнорированы; используются значения конфигурации")
	}

	if cfg.Auth.Enabled {
		bootstrapPassword := cfg.Auth.BootstrapPassword
		cfg.Auth.BootstrapPassword = ""
		baseCfg.Auth.BootstrapPassword = ""
		generated, err := api.EnsureBootstrapUser(ctx, st, cfg.Auth.BootstrapUser, bootstrapPassword)
		bootstrapPassword = ""
		if err != nil {
			return fmt.Errorf("создание первой учётной записи: %w", err)
		}
		if generated != "" {
			// Printed once and never stored in clear text anywhere.
			//
			// It goes to stderr as its own block rather than into a log field:
			// as a field it landed at the end of a long sentence, and a terminal
			// narrower than that sentence wraps the line, so copying the
			// password picks up a newline or a stray space and the first login
			// fails for a reason nobody can see.
			printCredentials(cfg.Auth.BootstrapUser, generated, "СОЗДАНА УЧЁТНАЯ ЗАПИСЬ АДМИНИСТРАТОРА")
			log.Warn().Str("пользователь", cfg.Auth.BootstrapUser).
				Msg("создана учётная запись администратора, пароль напечатан отдельным блоком")
		}
	} else {
		log.Warn().Msg("аутентификация выключена: API доступен без входа — используйте только за внешним периметром")
	}
	go logs.RotateDaily(rotateDone, log)

	loadServer := func(ctx context.Context, serverID string) (*model.Server, error) {
		return st.GetServer(ctx, serverID)
	}

	pool := ovirt.NewPool(loadServer, cfg.Monitor.Timeout, log)
	defer pool.Close()

	// Bare libvirt hosts are reached over SSH; the pool keeps one session per
	// hypervisor and redials when it dies.
	libvirtPool := libvirtx.NewPool(loadServer, cfg.Monitor.Timeout, log)
	defer libvirtPool.Close()

	bus := events.NewBus(128)
	notificationManager := notify.NewManager(st, cfg.Notifications, notifier, bus, log)
	st.OnAlertRaised(func(model.Alert) { notificationManager.Wake() })
	go notificationManager.Run(ctx)
	engine := backup.NewEngine(st, pool, cfg.Backup, cipher, log)

	// Карантин удаления: копии, помеченные удалёнными, лежат нетронутыми до
	// срока, и только потом их данные стираются. Без этого сборщика они
	// остались бы в хранилище навсегда, занимая место.
	if cfg.Backup.PurgeDelay > 0 {
		purgeCtx, stopPurge := context.WithCancel(ctx)
		purgeDone := make(chan struct{})
		go func() {
			defer close(purgeDone)
			engine.RunPurgeCollector(purgeCtx)
		}()
		defer func() {
			stopPurge()
			<-purgeDone
		}()
		log.Info().Dur("карантин", cfg.Backup.PurgeDelay).
			Msg("удалённые копии стираются не сразу: до срока их можно вернуть")
	}
	// The dispatcher adds the libvirt path on top of the oVirt engine; it
	// cannot live inside the backup package because the KVM driver builds on
	// that package's storage format.
	dispatcher := dispatch.New(engine, st, libvirtPool, cfg.Backup, cipher, log)
	fileBackupEngine := filebackup.New(st, *cfg, cipher, log)

	if backup.QemuImgAvailable(cfg.Backup.QemuImgPath) {
		log.Info().Msg("qemu-img найден: доступны экспорт в qcow2 и проверка qemu-img check")
	} else {
		log.Info().Msg("qemu-img не найден: бэкапы и восстановление работают, недоступны только экспорт в qcow2 и qemu-img check")
	}

	// Runs that were executing when the previous process died still hold locks
	// on the engine side; clearing them first prevents every later backup of
	// those VMs from failing.
	if err := engine.ReconcileStaleRuns(ctx); err != nil {
		log.Warn().Err(err).Msg("не удалось разобрать незавершённые бэкапы предыдущего запуска")
	}

	// Выключенное управление гасит и робота восстановления. Иначе выключатель
	// закрывал бы только людей: маршрутов нет, а служба сама продолжает
	// перезапускать ВМ и обесточивать хосты — ровно те действия, ради запрета
	// которых его и передвинули.
	if !cfg.Management.Enabled {
		if cfg.Monitor.Remediation.Enabled {
			log.Warn().Msg("управление виртуализацией выключено (management.enabled=false) — " +
				"авто-восстановление ВМ и хостов не запускается")
		} else {
			log.Info().Msg("управление виртуализацией выключено (management.enabled=false) — " +
				"служба выполняет только резервное копирование")
		}
		// Гасится в самом cfg, а не в копии: тот же флаг читают монитор и
		// ответ /meta, и разойдись они — интерфейс показывал бы включённое
		// авто-восстановление, которого нет.
		cfg.Monitor.Remediation.Enabled = false
	}

	remediator := monitor.NewRemediator(st, pool, libvirtPool, cfg.Monitor.Remediation, bus, log)
	mon := monitor.New(st, pool, libvirtPool, remediator, cfg.Monitor, bus, log)
	qualityService := quality.New(st, cfg.Monitor.BackupQuality, cfg.Location())
	replicator := replication.New(st, cfg.Backup.ReplicationWorkers, bus, log)
	replicator.SetVerifier(func(ctx context.Context, runID, copyID string, mode model.VerifyMode, opts model.VerifyOptions) error {
		_, err := dispatcher.VerifyCopy(ctx, runID, copyID, mode, opts)
		return err
	})
	replicator.Start()
	defer replicator.Close()

	sched := scheduler.New(st, dispatcher, *cfg, bus, log)
	sched.SetQualityService(qualityService)
	sched.SetReplicator(replicator)
	sched.SetFileBackupEngine(fileBackupEngine)

	// The mode is stored, not configured: an operator halfway through observing
	// the automation must not be dropped into live mode by a restart.
	if err := remediator.RestoreMode(ctx); err != nil {
		return fmt.Errorf("восстановление режима авто-восстановления: %w", err)
	}

	// Планировщик и монитор — то, что нельзя выполнять дважды: задания
	// запустились бы по два раза, а авто-восстановление подралось бы за
	// действия над ВМ. Переносы в очереди этого не боятся, они разбираются
	// арендой, и потому идут на всех экземплярах.
	background := newBackgroundParts(sched, mon, log)

	if cfg.Cluster.LeaderElection {
		leaderDone := make(chan struct{})
		go func() {
			defer close(leaderDone)
			superviseLeadership(ctx, st, cfg.Cluster.PollInterval, background, log)
		}()
		defer func() { <-leaderDone }()
	} else {
		if err := background.start(ctx); err != nil {
			return fmt.Errorf("запуск планировщика: %w", err)
		}
	}
	defer background.stop()

	// Журнал аудита для внешнего сборщика. Отказ открыть файл — причина не
	// стартовать: настроенный и не работающий вывод аудита хуже отсутствующего,
	// потому что на него рассчитывают.
	auditFile, err := auditlog.Open(cfg.Audit.File)
	if err != nil {
		log.Fatal().Err(err).Msg("журнал аудита не открывается")
	}
	if auditFile != nil {
		defer auditFile.Close()
		log.Info().Str("файл", cfg.Audit.File).Msg("журнал аудита дублируется наружу")
	}

	apiServer := api.New(api.Deps{
		Config: *cfg, BaseConfig: baseCfg, Store: st, Pool: pool, LibvirtPool: libvirtPool, Engine: dispatcher,
		Scheduler: sched, Monitor: mon, Remediator: remediator, Bus: bus, Logger: log,
		Logs: logs, Quality: qualityService, Replicator: replicator, Notifier: notifier,
		Notifications: notificationManager, DR: drChecker,
		FileBackup: fileBackupEngine, AuditFile: auditFile,
	})

	// Сроки согласования: эскалация на резервную группу и закрытие заявок,
	// которые никто не подтвердил. Без этого просроченная заявка висела бы
	// вечно, а резервная группа не узнала бы о ней никогда.
	approvalCtx, stopApprovals := context.WithCancel(ctx)
	approvalsDone := make(chan struct{})
	go func() {
		defer close(approvalsDone)
		apiServer.RunApprovalSweeper(approvalCtx)
	}()
	defer func() {
		stopApprovals()
		<-approvalsDone
	}()

	httpServer := &http.Server{
		Addr:        cfg.Server.ListenAddr(),
		Handler:     apiServer.Handler(),
		ReadTimeout: cfg.Server.ReadTimeout,
		// WriteTimeout stays unset by default: restoring a disk streams for
		// hours through this handler, and a write deadline would cut it off.
		WriteTimeout:      cfg.Server.WriteTimeout,
		ReadHeaderTimeout: 15 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		scheme := "http"
		if cfg.Server.TLS.Enabled {
			scheme = "https"
		}
		log.Info().Str("адрес", scheme+"://"+cfg.Server.ListenAddr()).Msg("веб-интерфейс и API доступны")

		var err error
		if cfg.Server.TLS.Enabled {
			err = httpServer.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		} else {
			err = httpServer.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("HTTP-сервер: %w", err)
		}
	case <-ctx.Done():
		log.Info().Msg("получен сигнал остановки, завершаем работу")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn().Err(err).Msg("HTTP-сервер не завершился штатно")
	}
	background.stop()
	log.Info().Msg("остановлено")
	return nil
}

func openRecoveryToken(path string) (io.Reader, func() error, error) {
	if path == "-" {
		return os.Stdin, nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("открытие recovery-токена %s: %w", path, err)
	}
	return file, file.Close, nil
}

// backgroundParts — то, что должно работать ровно в одном экземпляре.
//
// Планировщик запустил бы каждое задание дважды, а авто-восстановление
// подралось бы за действия над одной ВМ. Переносы в очереди сюда не входят:
// они разбираются арендой и безопасны на всех экземплярах.
type backgroundParts struct {
	sched *scheduler.Scheduler
	mon   *monitor.Monitor
	log   zerolog.Logger

	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped chan struct{}
}

func newBackgroundParts(sched *scheduler.Scheduler, mon *monitor.Monitor, log zerolog.Logger) *backgroundParts {
	return &backgroundParts{sched: sched, mon: mon, log: log}
}

func (b *backgroundParts) start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	if err := b.sched.Start(runCtx); err != nil {
		cancel()
		return err
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		b.mon.Run(runCtx)
	}()
	b.cancel, b.stopped = cancel, stopped
	return nil
}

// stop останавливает всё и ждёт остановки. Повторный вызов безопасен: путей к
// завершению службы несколько.
func (b *backgroundParts) stop() {
	b.mu.Lock()
	cancel, stopped := b.cancel, b.stopped
	b.cancel, b.stopped = nil, nil
	b.mu.Unlock()

	if cancel == nil {
		return
	}
	b.sched.Stop()
	cancel()
	<-stopped
}

// superviseLeadership держит место ведущего и включает по нему фоновые части.
//
// Место занимается консультативной блокировкой PostgreSQL: она живёт, пока
// живо соединение, поэтому упавший экземпляр отпускает её сам — без сроков и
// ожидания, пока истечёт аренда. Проверять её всё равно нужно: наполовину
// закрытое TCP-соединение может выглядеть живым часами, и всё это время
// ведущими считали бы себя оба.
func superviseLeadership(ctx context.Context, st *store.Store, poll time.Duration,
	parts *backgroundParts, log zerolog.Logger) {

	if poll <= 0 {
		poll = 15 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var leadership *store.Leadership
	defer func() {
		if leadership != nil {
			leadership.Release(ctx)
		}
	}()

	for {
		switch {
		case leadership == nil:
			held, err := st.TryBecomeLeader(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("не удалось проверить место ведущего")
				break
			}
			if held == nil {
				break // место занято другим экземпляром — это норма для ведомого
			}
			leadership = held
			if err := parts.start(ctx); err != nil {
				log.Error().Err(err).Msg("не удалось запустить планировщик, место отдаю")
				leadership.Release(ctx)
				leadership = nil
				break
			}
			log.Info().Msg("экземпляр стал ведущим: планировщик и монитор работают здесь")

		case !leadership.Alive(ctx):
			// Продолжать нельзя: место, скорее всего, уже занял другой
			// экземпляр, и задания пошли бы по второму разу.
			log.Error().Msg("место ведущего потеряно, останавливаю планировщик и монитор")
			parts.stop()
			leadership.Release(ctx)
			leadership = nil
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// printCredentials writes an account's credentials to stderr as a block that
// survives being copied.
//
// The password used to be a field on a log line, after a sentence long enough
// to wrap on a normal terminal. A wrapped line copies with a newline in the
// middle of the value, and the login then fails with "неверный пароль" — the
// one error message that gives the operator nothing to work with. On its own
// line, delimited, it cannot pick up neighbouring text.
func printCredentials(username, password, title string) {
	const rule = "════════════════════════════════════════════════════════════"
	fmt.Fprintf(os.Stderr, "\n%s\n  %s\n\n  пользователь: %s\n  пароль:       %s\n\n"+
		"  Запишите пароль: он больше не будет показан.\n"+
		"  Забыли — используйте host-side ovirt-backup-recover-admin.\n%s\n\n",
		rule, title, username, password, rule)
}

// warnLeftoverSQLite сообщает, что в каталоге данных остался файл базы от
// прежних версий.
//
// До перехода на PostgreSQL сервис умел работать с SQLite, и у обновившихся
// установок файл остаётся лежать. Он больше не читается, но выглядит как
// действующая база: без этой строки «а где мои подключения» превращается в
// поиск вслепую.
//
// Предупреждение, а не отказ: файл может быть намеренно оставлен до тех пор,
// пока оператор не убедится, что всё перенесено.
func warnLeftoverSQLite(cfg *config.Config, log zerolog.Logger) {
	dir := filepath.Dir(cfg.Secrets.KeyFile)
	if dir == "" || dir == "." {
		return
	}
	path := filepath.Join(dir, "jhvirt.db")
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return
	}
	log.Warn().
		Str("файл", path).
		Msg("остался файл базы SQLite от прежней версии — он не используется, " +
			"сервис работает только с PostgreSQL; подключения и задания в нём не видны")
}
