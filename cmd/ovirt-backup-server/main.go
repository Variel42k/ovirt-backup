// Command ovirt-backup-server manages oVirt engines and their forks: it polls
// their state, revives what it is allowed to revive, and runs the backups.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/api"
	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/dispatch"
	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/logging"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/monitor"
	"adveng/jh_virt/internal/ovirt"
	"adveng/jh_virt/internal/scheduler"
	"adveng/jh_virt/internal/secret"
	"adveng/jh_virt/internal/store"
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
	if *checkConfig {
		fmt.Println("конфигурация корректна")
		return nil
	}

	log, logs, err := logging.Setup(cfg.Logging)
	if err != nil {
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
	go logs.RotateDaily(rotateDone, log)
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

	// Password recovery runs before anything else starts. Without it an operator
	// who lost the bootstrap password has no way back in except deleting the
	// database, which would take every connection and backup record with it.
	if *resetUser != "" {
		password, err := api.ResetPassword(ctx, st, *resetUser, os.Getenv("JHV_NEW_PASSWORD"))
		if err != nil {
			return err
		}
		printCredentials(*resetUser, password, "ПАРОЛЬ ИЗМЕНЁН")
		return nil
	}

	if cfg.Auth.Enabled {
		generated, err := api.EnsureBootstrapUser(ctx, st, cfg.Auth.BootstrapUser, cfg.Auth.BootstrapPassword)
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
	engine := backup.NewEngine(st, pool, cfg.Backup, cipher, log)
	// The dispatcher adds the libvirt path on top of the oVirt engine; it
	// cannot live inside the backup package because the KVM driver builds on
	// that package's storage format.
	dispatcher := dispatch.New(engine, st, libvirtPool, cfg.Backup, cipher, log)

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

	remediator := monitor.NewRemediator(st, pool, libvirtPool, cfg.Monitor.Remediation, bus, log)
	mon := monitor.New(st, pool, libvirtPool, remediator, cfg.Monitor, bus, log)

	sched := scheduler.New(st, dispatcher, *cfg, bus, log)
	if err := sched.Start(ctx); err != nil {
		return fmt.Errorf("запуск планировщика: %w", err)
	}
	defer sched.Stop()

	// The mode is stored, not configured: an operator halfway through observing
	// the automation must not be dropped into live mode by a restart.
	if err := remediator.RestoreMode(ctx); err != nil {
		return fmt.Errorf("восстановление режима авто-восстановления: %w", err)
	}

	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		mon.Run(ctx)
	}()

	apiServer := api.New(api.Deps{
		Config: *cfg, Store: st, Pool: pool, LibvirtPool: libvirtPool, Engine: dispatcher,
		Scheduler: sched, Monitor: mon, Remediator: remediator, Bus: bus, Logger: log,
		Logs: logs,
	})

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
	<-monitorDone
	log.Info().Msg("остановлено")
	return nil
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
		"  Забыли — задайте новый: ovirt-backup-server -reset-password %s\n%s\n\n",
		rule, title, username, password, username, rule)
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
