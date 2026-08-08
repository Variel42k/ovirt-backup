// Package scheduler runs backup jobs on their cron schedules and performs the
// periodic housekeeping: retention, health checks and cleanup.
package scheduler

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/dispatch"
	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/store"
)

// cronParser accepts the standard five-field syntax plus the @daily-style
// descriptors, which is what operators expect from a crontab field.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// ValidateSchedule checks a cron expression and reports the next firing time.
func ValidateSchedule(spec string, loc *time.Location) (time.Time, error) {
	if spec == "" {
		return time.Time{}, fmt.Errorf("расписание не задано")
	}
	sched, err := cronParser.Parse(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("некорректное расписание %q: %w", spec, err)
	}
	return sched.Next(time.Now().In(loc)), nil
}

// Scheduler owns the cron engine and the backup worker pool.
type Scheduler struct {
	store  *store.Store
	engine *dispatch.Dispatcher
	cfg    config.Config
	bus    *events.Bus
	log    zerolog.Logger

	cron *cron.Cron

	mu      sync.Mutex
	entries map[string]cron.EntryID
	running map[string]context.CancelFunc

	// workers ограничивает число одновременно выполняющихся бэкапов.
	workers chan struct{}
	baseCtx context.Context
}

// New builds the scheduler.
func New(st *store.Store, engine *dispatch.Dispatcher, cfg config.Config, bus *events.Bus, log zerolog.Logger) *Scheduler {
	workers := cfg.Backup.Workers
	if workers < 1 {
		workers = 1
	}
	return &Scheduler{
		store:   st,
		engine:  engine,
		cfg:     cfg,
		bus:     bus,
		log:     log,
		cron:    cron.New(cron.WithLocation(cfg.Location()), cron.WithParser(cronParser)),
		entries: map[string]cron.EntryID{},
		running: map[string]context.CancelFunc{},
		workers: make(chan struct{}, workers),
	}
}

// Start loads the jobs, registers the housekeeping tasks and begins ticking.
func (s *Scheduler) Start(ctx context.Context) error {
	s.baseCtx = ctx

	if err := s.registerMaintenance(); err != nil {
		return err
	}
	if err := s.Reload(ctx); err != nil {
		return err
	}

	if !s.cfg.Scheduler.Enabled {
		s.log.Warn().Msg("планировщик выключен в конфигурации: задания по расписанию выполняться не будут")
		return nil
	}
	s.cron.Start()
	s.log.Info().
		Str("часовой пояс", s.cfg.Scheduler.Timezone).
		Int("рабочих потоков", cap(s.workers)).
		Msg("планировщик запущен")
	return nil
}

// Stop halts the cron engine and waits for in-flight ticks to finish.
func (s *Scheduler) Stop() {
	stopCtx := s.cron.Stop()
	<-stopCtx.Done()

	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.running))
	for _, cancel := range s.running {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()

	// Cancelling lets each run record its own interruption; the engine's
	// deferred cleanup still releases engine-side locks.
	for _, cancel := range cancels {
		cancel()
	}
	s.log.Info().Msg("планировщик остановлен")
}

// Reload rebuilds the cron entries from the stored job definitions. It is
// called at startup and whenever a job is created, edited or deleted.
func (s *Scheduler) Reload(ctx context.Context) error {
	jobs, err := s.store.ListBackupJobs(ctx, "")
	if err != nil {
		return fmt.Errorf("загрузка заданий: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for jobID, entryID := range s.entries {
		s.cron.Remove(entryID)
		delete(s.entries, jobID)
	}

	loc := s.cfg.Location()
	active := 0
	for _, job := range jobs {
		if !job.Enabled || job.Schedule == "" {
			// A job without a schedule is still usable — it just only runs
			// when somebody presses the button.
			continue
		}
		next, err := ValidateSchedule(job.Schedule, loc)
		if err != nil {
			s.log.Error().Err(err).Str("задание", job.Name).
				Msg("задание пропущено: не удалось разобрать расписание")
			continue
		}

		jobID := job.ID
		entryID, err := s.cron.AddFunc(job.Schedule, func() {
			s.runScheduled(jobID)
		})
		if err != nil {
			s.log.Error().Err(err).Str("задание", job.Name).Msg("не удалось зарегистрировать задание")
			continue
		}
		s.entries[jobID] = entryID
		active++

		nextRun := next
		if err := s.store.SetJobSchedulingState(ctx, job.ID, job.LastRunAt, job.LastStatus, &nextRun); err != nil {
			s.log.Debug().Err(err).Msg("не удалось сохранить время следующего запуска")
		}
	}

	s.log.Info().Int("активных заданий", active).Int("всего", len(jobs)).Msg("расписание перечитано")
	return nil
}

// registerMaintenance wires the periodic housekeeping.
func (s *Scheduler) registerMaintenance() error {
	tasks := []struct {
		spec string
		name string
		fn   func(context.Context)
	}{
		{"@every 1h", "ретенция", s.runRetention},
		{"@every 1h", "просроченные бэкапы", s.pruneExpired},
		{"@every 15m", "проверка хранилищ", s.checkStorageTargets},
		{"@every 1h", "устаревшие бэкапы", s.checkBackupFreshness},
		{"@every 6h", "очистка истории", s.purgeHistory},
	}

	for _, task := range tasks {
		name, fn := task.name, task.fn
		if _, err := s.cron.AddFunc(task.spec, func() {
			ctx, cancel := context.WithTimeout(s.baseCtx, 2*time.Hour)
			defer cancel()
			defer func() {
				if p := recover(); p != nil {
					s.log.Error().Interface("паника", p).Str("задача", name).
						Msg("служебная задача завершилась аварийно")
				}
			}()
			fn(ctx)
		}); err != nil {
			return fmt.Errorf("регистрация служебной задачи %q: %w", name, err)
		}
	}
	return nil
}

// runScheduled is the cron callback for one job.
func (s *Scheduler) runScheduled(jobID string) {
	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := s.TriggerJob(ctx, jobID, "scheduler"); err != nil {
		s.log.Error().Err(err).Str("задание", jobID).Msg("запуск задания по расписанию не удался")
	}
}

// TriggerJob executes a job now: it resolves the VMs it covers and queues a
// backup for each. It returns as soon as the runs are queued.
func (s *Scheduler) TriggerJob(ctx context.Context, jobID, triggeredBy string) ([]string, error) {
	job, err := s.store.GetBackupJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if len(job.StorageTargetIDs) == 0 {
		return nil, fmt.Errorf("у задания %q не выбрано ни одного хранилища", job.Name)
	}

	vms, err := s.resolveVMs(ctx, job)
	if err != nil {
		return nil, err
	}
	if len(vms) == 0 {
		s.log.Warn().Str("задание", job.Name).Msg("задание не выбрало ни одной ВМ")
		return nil, nil
	}

	now := time.Now().UTC()
	var nextRun *time.Time
	if job.Schedule != "" {
		if next, err := ValidateSchedule(job.Schedule, s.cfg.Location()); err == nil {
			nextRun = &next
		}
	}
	if err := s.store.SetJobSchedulingState(ctx, job.ID, &now, model.RunRunning, nextRun); err != nil {
		s.log.Debug().Err(err).Msg("не удалось отметить запуск задания")
	}

	s.log.Info().
		Str("задание", job.Name).
		Int("ВМ", len(vms)).
		Int("хранилищ", len(job.StorageTargetIDs)).
		Str("инициатор", triggeredBy).
		Msg("задание запущено")

	var queued []string
	var wg sync.WaitGroup

	for _, vm := range vms {
		for _, targetID := range job.StorageTargetIDs {
			req := backup.RunRequest{
				ServerID:        job.ServerID,
				VMID:            vm.ID,
				Type:            job.Type,
				JobID:           job.ID,
				JobName:         job.Name,
				FullEvery:       job.FullEvery,
				FallbackType:    job.FallbackType,
				StorageTargetID: targetID,
				ExcludeDiskIDs:  job.ExcludeDiskIDs,
				Quiesce:         job.Quiesce,
				Encrypt:         job.Encrypt,
				VerifyAfter:     job.VerifyAfter,
				Retention:       job.Retention,
				TriggeredBy:     triggeredBy,
			}
			queued = append(queued, vm.ID)

			wg.Add(1)
			go func(req backup.RunRequest, job *model.BackupJob) {
				defer wg.Done()
				s.executeOne(ctx, req, job)
			}(req, job)
		}
	}

	// Wait in the background so an operator pressing "run now" gets an
	// immediate answer while the work continues.
	go func() {
		wg.Wait()
		s.finishJob(ctx, job)
	}()

	return queued, nil
}

// executeOne runs a single backup, respecting the worker limit.
func (s *Scheduler) executeOne(ctx context.Context, req backup.RunRequest, job *model.BackupJob) {
	select {
	case s.workers <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-s.workers }()

	defer func() {
		if p := recover(); p != nil {
			s.log.Error().Interface("паника", p).Str("вм", req.VMID).Msg("бэкап завершился аварийно")
		}
	}()

	runCtx := ctx
	var cancel context.CancelFunc
	if job != nil && job.MaxDuration > 0 {
		runCtx, cancel = context.WithTimeout(ctx, job.MaxDuration)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	run, err := s.engine.Execute(runCtx, req)
	if run != nil {
		s.mu.Lock()
		s.running[run.ID] = cancel
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.running, run.ID)
			s.mu.Unlock()
		}()

		s.bus.Publish(events.Event{
			Kind: events.KindBackupRun, ServerID: run.ServerID, ObjectID: run.ID,
			Message: fmt.Sprintf("%s: бэкап %s — %s", run.VMName, run.Type.Title(), run.Status),
			Payload: run,
		})
	}
	if err != nil {
		s.log.Error().Err(err).Str("вм", req.VMID).Str("задание", req.JobName).Msg("бэкап не выполнен")
		if run != nil {
			s.raiseBackupAlert(ctx, run, err)
		}
		return
	}

	_ = s.store.ResolveAlert(ctx, run.ServerID, model.ScopeVM, run.VMID, model.AlertBackupFailed)

	if req.VerifyAfter != "" && run.Status != model.RunFailed {
		// No options: a scheduled boot test runs on the hypervisor the backup
		// came from, which only works when that is a libvirt connection. For an
		// oVirt backup the verifier says so instead of picking a host nobody
		// asked for.
		if _, err := s.engine.Verify(ctx, run.ID, req.VerifyAfter, model.VerifyOptions{}); err != nil {
			s.log.Warn().Err(err).Str("run", run.ID).Msg("проверка после бэкапа не пройдена")
			s.raiseVerifyAlert(ctx, run, err)
		}
	}

	if job != nil && !job.Retention.Empty() {
		if _, err := s.engine.ApplyRetention(ctx, run.ServerID, run.VMID, run.StorageTargetID,
			job.Retention, false); err != nil {
			s.log.Warn().Err(err).Str("вм", run.VMName).Msg("ретенция после бэкапа не отработала")
		}
	}
}

func (s *Scheduler) finishJob(ctx context.Context, job *model.BackupJob) {
	runs, err := s.store.ListBackupRuns(ctx, store.RunFilter{
		JobID: job.ID,
		Since: ptr(time.Now().Add(-24 * time.Hour)),
		Limit: 200,
	})
	if err != nil {
		return
	}

	status := model.RunSucceeded
	for _, r := range runs {
		if r.Status == model.RunFailed {
			status = model.RunFailed
			break
		}
		if r.Status == model.RunPartial {
			status = model.RunPartial
		}
	}

	now := time.Now().UTC()
	var nextRun *time.Time
	if job.Schedule != "" {
		if next, err := ValidateSchedule(job.Schedule, s.cfg.Location()); err == nil {
			nextRun = &next
		}
	}
	if err := s.store.SetJobSchedulingState(ctx, job.ID, &now, status, nextRun); err != nil {
		s.log.Debug().Err(err).Msg("не удалось сохранить итог задания")
	}
	s.bus.Publish(events.Event{
		Kind: events.KindJob, ObjectID: job.ID,
		Message: fmt.Sprintf("задание «%s» завершено: %s", job.Name, status),
	})
}

// resolveVMs turns a job's selector into a concrete list of VMs.
//
// An empty selector means "every VM on the server", which is the setting most
// installations actually want and the one people forget to configure.
func (s *Scheduler) resolveVMs(ctx context.Context, job *model.BackupJob) ([]*model.VM, error) {
	all, err := s.store.ListVMs(ctx, job.ServerID)
	if err != nil {
		return nil, err
	}

	var nameRe *regexp.Regexp
	if job.VMNameRegex != "" {
		nameRe, err = regexp.Compile(job.VMNameRegex)
		if err != nil {
			return nil, fmt.Errorf("некорректное выражение отбора по имени %q: %w", job.VMNameRegex, err)
		}
	}

	selectorEmpty := len(job.VMIDs) == 0 && nameRe == nil && len(job.ClusterIDs) == 0

	out := make([]*model.VM, 0, len(all))
	for _, vm := range all {
		if slices.Contains(job.ExcludeVMIDs, vm.ID) {
			continue
		}
		match := selectorEmpty ||
			slices.Contains(job.VMIDs, vm.ID) ||
			slices.Contains(job.ClusterIDs, vm.ClusterID) ||
			(nameRe != nil && nameRe.MatchString(vm.Name))
		if match {
			out = append(out, vm)
		}
	}
	return out, nil
}

// CancelRun stops a backup that is currently executing.
func (s *Scheduler) CancelRun(runID string) bool {
	s.mu.Lock()
	cancel, ok := s.running[runID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	s.log.Info().Str("run", runID).Msg("бэкап отменён оператором")
	return true
}

// RunningCount reports how many backups are executing right now.
func (s *Scheduler) RunningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// Housekeeping.

func (s *Scheduler) runRetention(ctx context.Context) {
	jobs, err := s.store.ListBackupJobs(ctx, "")
	if err != nil {
		s.log.Error().Err(err).Msg("ретенция: не удалось получить задания")
		return
	}

	for _, job := range jobs {
		if job.Retention.Empty() {
			continue
		}
		vms, err := s.resolveVMs(ctx, job)
		if err != nil {
			continue
		}
		for _, vm := range vms {
			for _, targetID := range job.StorageTargetIDs {
				plan, err := s.engine.ApplyRetention(ctx, job.ServerID, vm.ID, targetID, job.Retention, false)
				if err != nil {
					s.log.Warn().Err(err).Str("вм", vm.Name).Msg("ретенция не отработала")
					continue
				}
				if len(plan.Delete) > 0 {
					s.log.Info().Str("вм", vm.Name).Int("удалено", len(plan.Delete)).
						Int64("освобождено", plan.FreedBytes).Msg("ретенция выполнена")
				}
			}
		}
	}
}

func (s *Scheduler) pruneExpired(ctx context.Context) {
	n, err := s.engine.PruneExpired(ctx)
	if err != nil {
		s.log.Warn().Err(err).Msg("не удалось удалить просроченные бэкапы")
		return
	}
	if n > 0 {
		s.log.Info().Int("удалено", n).Msg("просроченные бэкапы удалены")
	}
}

// checkStorageTargets verifies each repository is reachable and writable.
// Discovering that the backup target died only when a backup fails is too late.
func (s *Scheduler) checkStorageTargets(ctx context.Context) {
	targets, err := s.store.ListStorageTargets(ctx)
	if err != nil {
		return
	}

	for _, target := range targets {
		if !target.Enabled {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		backend, err := repo.Open(checkCtx, target)
		var free, used int64
		var checkErr error
		if err != nil {
			checkErr = err
		} else {
			checkErr = backend.Check(checkCtx)
			if checkErr == nil {
				free, used, _ = backend.Usage(checkCtx)
			}
			_ = backend.Close()
		}
		cancel()

		msg := ""
		if checkErr != nil {
			msg = checkErr.Error()
		}
		if err := s.store.UpdateStorageTargetHealth(ctx, target.ID, checkErr == nil, msg, free, used); err != nil {
			s.log.Debug().Err(err).Msg("не удалось сохранить состояние хранилища")
		}

		if checkErr != nil {
			_ = s.store.RaiseAlert(ctx, &model.Alert{
				Scope: model.ScopeStorageTarget, ObjectID: target.ID, ObjectName: target.Name,
				Kind: model.AlertStorageTargetDown, Severity: model.SeverityCritical,
				Message: fmt.Sprintf("хранилище бэкапов %q недоступно: %v", target.Name, checkErr),
			})
			s.log.Error().Err(checkErr).Str("хранилище", target.Name).Msg("хранилище недоступно")
		} else {
			_ = s.store.ResolveAlert(ctx, "", model.ScopeStorageTarget, target.ID, model.AlertStorageTargetDown)
		}
	}
}

// checkBackupFreshness alerts on VMs a job covers but has not backed up
// recently. A schedule that silently stopped firing is otherwise invisible
// until somebody needs a restore.
func (s *Scheduler) checkBackupFreshness(ctx context.Context) {
	jobs, err := s.store.ListBackupJobs(ctx, "")
	if err != nil {
		return
	}

	for _, job := range jobs {
		if !job.Enabled || job.Schedule == "" {
			continue
		}
		// Two missed firings is the threshold: one can be a slow night, two is
		// a pattern.
		interval, err := scheduleInterval(job.Schedule, s.cfg.Location())
		if err != nil {
			continue
		}
		deadline := time.Now().Add(-2 * interval)

		vms, err := s.resolveVMs(ctx, job)
		if err != nil {
			continue
		}
		for _, vm := range vms {
			runs, err := s.store.ListBackupRuns(ctx, store.RunFilter{
				ServerID: job.ServerID,
				VMID:     vm.ID,
				Statuses: []model.RunStatus{model.RunSucceeded, model.RunPartial},
				Limit:    1,
			})
			if err != nil {
				continue
			}
			if len(runs) > 0 && runs[0].CreatedAt.After(deadline) {
				_ = s.store.ResolveAlert(ctx, job.ServerID, model.ScopeVM, vm.ID, model.AlertBackupStale)
				continue
			}

			message := fmt.Sprintf("ВМ %s не бэкапилась дольше двух интервалов расписания задания «%s»",
				vm.Name, job.Name)
			if len(runs) == 0 {
				message = fmt.Sprintf("ВМ %s ни разу не бэкапилась заданием «%s»", vm.Name, job.Name)
			}
			_ = s.store.RaiseAlert(ctx, &model.Alert{
				ServerID: job.ServerID, Scope: model.ScopeVM, ObjectID: vm.ID, ObjectName: vm.Name,
				Kind: model.AlertBackupStale, Severity: model.SeverityWarning, Message: message,
			})
		}
	}
}

func (s *Scheduler) purgeHistory(ctx context.Context) {
	if s.cfg.Monitor.HistoryRetention > 0 {
		cutoff := time.Now().Add(-s.cfg.Monitor.HistoryRetention)
		if n, err := s.store.PurgeHealthSamples(ctx, cutoff); err == nil && n > 0 {
			s.log.Debug().Int64("удалено", n).Msg("история проб состояния очищена")
		}
		if n, err := s.store.PurgeResolvedAlerts(ctx, cutoff); err == nil && n > 0 {
			s.log.Debug().Int64("удалено", n).Msg("закрытые оповещения очищены")
		}
	}
	// Метрики ввода-вывода живут по своему сроку: точка на каждый диск каждой
	// ВМ при каждом опросе накапливается на порядок быстрее проб состояния.
	if s.cfg.Monitor.IORetention > 0 {
		cutoff := time.Now().Add(-s.cfg.Monitor.IORetention)
		if n, err := s.store.PruneIOSamples(ctx, cutoff); err == nil && n > 0 {
			s.log.Debug().Int64("удалено", n).Msg("метрики ввода-вывода очищены")
		}
	}
	if n, err := s.store.PurgeExpiredSessions(ctx); err == nil && n > 0 {
		s.log.Debug().Int64("удалено", n).Msg("истёкшие сессии очищены")
	}
}

func (s *Scheduler) raiseBackupAlert(ctx context.Context, run *model.BackupRun, cause error) {
	_ = s.store.RaiseAlert(ctx, &model.Alert{
		ServerID: run.ServerID, Scope: model.ScopeVM, ObjectID: run.VMID, ObjectName: run.VMName,
		Kind: model.AlertBackupFailed, Severity: model.SeverityCritical,
		Message: fmt.Sprintf("бэкап ВМ %s не выполнен: %v", run.VMName, cause),
		Details: run.Error,
	})
}

func (s *Scheduler) raiseVerifyAlert(ctx context.Context, run *model.BackupRun, cause error) {
	_ = s.store.RaiseAlert(ctx, &model.Alert{
		ServerID: run.ServerID, Scope: model.ScopeBackup, ObjectID: run.ID, ObjectName: run.VMName,
		Kind: model.AlertVerifyFailed, Severity: model.SeverityCritical,
		Message: fmt.Sprintf("проверка бэкапа ВМ %s не пройдена: %v", run.VMName, cause),
	})
}

// scheduleInterval estimates how often a cron expression fires, by measuring
// the gap between the next two firings.
func scheduleInterval(spec string, loc *time.Location) (time.Duration, error) {
	sched, err := cronParser.Parse(spec)
	if err != nil {
		return 0, err
	}
	now := time.Now().In(loc)
	first := sched.Next(now)
	second := sched.Next(first)
	if second.IsZero() || !second.After(first) {
		return 24 * time.Hour, nil
	}
	return second.Sub(first), nil
}

func ptr[T any](v T) *T { return &v }
