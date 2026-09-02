// Package scheduler runs backup jobs on their cron schedules and performs the
// periodic housekeeping: retention, health checks and cleanup.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/dispatch"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/filebackup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/quality"
	"github.com/Variel42k/ovirt-backup/internal/replication"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// ErrJobBusy сообщает, что задание уже выполняется и повторный запуск
// отклонён. Отдельная ошибка, а не просто текст: по ней API отвечает 409, а
// планировщик отличает штатный пропуск от настоящего сбоя.
var ErrJobBusy = errors.New("задание уже выполняется")

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
	store      *store.Store
	engine     *dispatch.Dispatcher
	cfg        config.Config
	bus        *events.Bus
	log        zerolog.Logger
	quality    *quality.Service
	replicator *replication.Replicator
	fileBackup *filebackup.Engine

	cron *cron.Cron
	// scheduleMu serializes job reloads with a timezone change. robfig/cron can
	// accept updates while running, but a reload must use one timezone snapshot.
	scheduleMu sync.Mutex
	timezoneMu sync.RWMutex
	timezone   string
	location   *time.Location

	mu      sync.Mutex
	entries map[string]cron.EntryID
	running map[string]context.CancelFunc
	// active содержит задания, которые выполняются прямо сейчас. Нужен
	// потому, что running считает отдельные бэкапы, а перекрываться могут
	// именно задания: задание длиннее своего интервала иначе запустится
	// вторым экземпляром поверх первого.
	active map[string]struct{}

	// workers ограничивает число одновременно выполняющихся бэкапов.
	workers     chan struct{}
	taskWake    chan struct{}
	taskSlotsMu sync.Mutex
	taskSlots   map[string]chan struct{}
	baseCtx     context.Context
}

func (s *Scheduler) SetReplicator(replicator *replication.Replicator) {
	s.replicator = replicator
}

func (s *Scheduler) SetFileBackupEngine(engine *filebackup.Engine) {
	s.fileBackup = engine
}

// SetQualityService connects the schedule-aware health evaluator. It is kept
// separate from New so scheduler tests and small command-line tools can run
// without constructing the monitoring subsystem.
func (s *Scheduler) SetQualityService(service *quality.Service) {
	s.quality = service
	if service != nil {
		service.SetLocation(s.Location())
	}
}

// New builds the scheduler.
func New(st *store.Store, engine *dispatch.Dispatcher, cfg config.Config, bus *events.Bus, log zerolog.Logger) *Scheduler {
	workers := cfg.Backup.Workers
	if workers < 1 {
		workers = 1
	}
	loc := cfg.Location()
	return &Scheduler{
		store:     st,
		engine:    engine,
		cfg:       cfg,
		bus:       bus,
		log:       log,
		cron:      cron.New(cron.WithLocation(loc), cron.WithParser(cronParser)),
		timezone:  cfg.Scheduler.Timezone,
		location:  loc,
		entries:   map[string]cron.EntryID{},
		running:   map[string]context.CancelFunc{},
		active:    map[string]struct{}{},
		workers:   make(chan struct{}, workers),
		taskWake:  make(chan struct{}, 1),
		taskSlots: map[string]chan struct{}{},
	}
}

// Timezone returns the effective IANA timezone used for cron schedules.
func (s *Scheduler) Timezone() string {
	s.timezoneMu.RLock()
	defer s.timezoneMu.RUnlock()
	return s.timezone
}

// Location returns the effective scheduler location.
func (s *Scheduler) Location() *time.Location {
	s.timezoneMu.RLock()
	defer s.timezoneMu.RUnlock()
	return s.location
}

func (s *Scheduler) timezoneSnapshot() (string, *time.Location) {
	s.timezoneMu.RLock()
	defer s.timezoneMu.RUnlock()
	return s.timezone, s.location
}

func (s *Scheduler) storeTimezone(name string, loc *time.Location) {
	s.timezoneMu.Lock()
	s.timezone, s.location = name, loc
	s.timezoneMu.Unlock()
}

// SetTimezone applies an IANA timezone and recalculates every future job
// occurrence. In-flight jobs are independent from cron entries and continue.
func (s *Scheduler) SetTimezone(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("часовой пояс не задан")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return fmt.Errorf("часовой пояс %q: %w", name, err)
	}

	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	previousName, previousLoc := s.timezoneSnapshot()
	if name == previousName {
		return nil
	}
	s.storeTimezone(name, loc)
	if err := s.reload(ctx); err != nil {
		s.storeTimezone(previousName, previousLoc)
		if rollbackErr := s.reload(ctx); rollbackErr != nil {
			return fmt.Errorf("применение часового пояса: %w; откат расписаний: %v", err, rollbackErr)
		}
		return err
	}
	if s.quality != nil {
		s.quality.SetLocation(loc)
	}
	return nil
}

func cronSpecInTimezone(spec, timezone string) string {
	return "CRON_TZ=" + timezone + " " + spec
}

// Start loads the jobs, registers the housekeeping tasks and begins ticking.
func (s *Scheduler) Start(ctx context.Context) error {
	s.baseCtx = ctx
	if err := s.recoverTaskFinalizations(ctx); err != nil {
		return fmt.Errorf("восстановление итогов очереди: %w", err)
	}
	for i := 0; i < cap(s.workers); i++ {
		go s.taskWorker(ctx, fmt.Sprintf("%s-%d", uuid.NewString(), i))
	}

	if err := s.registerMaintenance(); err != nil {
		return err
	}
	var catchUps []missedSchedule
	if s.cfg.Scheduler.Enabled {
		var err error
		catchUps, err = s.recoverMissedSchedules(ctx)
		if err != nil {
			return err
		}
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
		Str("часовой пояс", s.Timezone()).
		Int("рабочих потоков", cap(s.workers)).
		Msg("планировщик запущен")
	if s.cfg.Scheduler.CatchUpMissed {
		for _, missed := range catchUps {
			missed := missed
			go func() {
				if _, err := s.TriggerJob(ctx, missed.jobID, "catch_up", &missed.latest); err != nil {
					s.log.Warn().Err(err).Str("задание", missed.jobID).
						Msg("не удалось выполнить последний пропущенный запуск")
				}
			}()
		}
	}
	return nil
}

type missedSchedule struct {
	jobID  string
	latest time.Time
}

// recoverMissedSchedules records every schedule point lost while the service
// was stopped as one aggregate row. Catch-up, when enabled, starts only the
// latest point; replaying every old point can overload both the engine and the
// backup repository after a long outage.
func (s *Scheduler) recoverMissedSchedules(ctx context.Context) ([]missedSchedule, error) {
	jobs, err := s.store.ListBackupJobs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("загрузка заданий для восстановления расписания: %w", err)
	}
	now := time.Now().UTC()
	var out []missedSchedule
	for _, job := range jobs {
		if !job.Enabled || job.Schedule == "" || job.NextRunAt == nil || job.NextRunAt.After(now) {
			continue
		}
		schedule, err := cronParser.Parse(job.Schedule)
		if err != nil {
			continue
		}
		loc := s.Location()
		count, latest := 0, job.NextRunAt.In(loc)
		for point := latest; !point.After(now.In(loc)); point = schedule.Next(point) {
			count++
			latest = point
			if count >= 100000 {
				s.log.Warn().Str("задание", job.Name).Msg("число пропущенных точек ограничено 100000")
				break
			}
		}
		if count == 0 {
			continue
		}
		latestUTC := latest.UTC()
		ended := now
		vms, _ := s.resolveVMs(ctx, job)
		run := &model.BackupJobRun{
			JobID: job.ID, JobName: job.Name, ServerID: job.ServerID, TriggeredBy: "scheduler",
			ScheduledAt: &latestUTC, MissedIntervals: count, Status: model.RunMissed,
			VMCount: len(vms), ReplicaCount: len(vms) * len(job.StorageTargetIDs),
			Error: "служба не работала в запланированное время", EndedAt: &ended,
		}
		if err := s.store.CreateBackupJobRun(ctx, run); err != nil {
			return nil, err
		}
		_ = s.store.RaiseAlert(ctx, &model.Alert{
			ServerID: job.ServerID, Scope: model.ScopeBackup, ObjectID: job.ID, ObjectName: job.Name,
			Kind: model.AlertBackupScheduleMissed, Severity: model.SeverityWarning,
			Message: fmt.Sprintf("задание «%s» пропустило точек расписания: %d", job.Name, count),
		})
		out = append(out, missedSchedule{jobID: job.ID, latest: latestUTC})
	}
	return out, nil
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
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	return s.reload(ctx)
}

func (s *Scheduler) reload(ctx context.Context) error {
	jobs, err := s.store.ListBackupJobs(ctx, "")
	if err != nil {
		return fmt.Errorf("загрузка заданий: %w", err)
	}
	fileJobs, err := s.store.ListFileBackupJobs(ctx)
	if err != nil {
		return fmt.Errorf("загрузка заданий файлового бекапа: %w", err)
	}
	engineJobs, err := s.store.ListEngineConfigJobs(ctx)
	if err != nil {
		return fmt.Errorf("загрузка заданий снимков Engine: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for jobID, entryID := range s.entries {
		s.cron.Remove(entryID)
		delete(s.entries, jobID)
	}

	timezone, loc := s.timezoneSnapshot()
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
		entryID, err := s.cron.AddFunc(cronSpecInTimezone(job.Schedule, timezone), func() {
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
	for _, job := range fileJobs {
		if !job.Enabled || job.Schedule == "" || s.fileBackup == nil {
			continue
		}
		if _, err := ValidateSchedule(job.Schedule, loc); err != nil {
			s.log.Error().Err(err).Str("файловое задание", job.Name).
				Msg("задание пропущено: не удалось разобрать расписание")
			continue
		}
		jobID := job.ID
		entryID, err := s.cron.AddFunc(cronSpecInTimezone(job.Schedule, timezone), func() {
			s.runScheduledFile(jobID)
		})
		if err != nil {
			s.log.Error().Err(err).Str("файловое задание", job.Name).Msg("не удалось зарегистрировать задание")
			continue
		}
		s.entries["file:"+jobID] = entryID
		active++
	}
	for _, job := range engineJobs {
		if !job.Enabled || job.Schedule == "" {
			continue
		}
		if _, err := ValidateSchedule(job.Schedule, loc); err != nil {
			s.log.Error().Err(err).Str("задание Engine", job.Name).
				Msg("задание пропущено: не удалось разобрать расписание")
			continue
		}
		jobID := job.ID
		entryID, err := s.cron.AddFunc(cronSpecInTimezone(job.Schedule, timezone), func() {
			s.runScheduledEngineConfig(jobID)
		})
		if err != nil {
			s.log.Error().Err(err).Str("задание Engine", job.Name).Msg("не удалось зарегистрировать задание")
			continue
		}
		s.entries["engine:"+jobID] = entryID
		active++
	}

	s.log.Info().Int("активных заданий", active).Int("всего", len(jobs)+len(fileJobs)+len(engineJobs)).Msg("расписание перечитано")
	return nil
}

func (s *Scheduler) runScheduledEngineConfig(jobID string) {
	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	job, err := s.store.GetEngineConfigJob(ctx, jobID)
	if err != nil {
		s.log.Error().Err(err).Str("задание Engine", jobID).Msg("задание не найдено")
		return
	}
	busy, err := s.store.HasActiveEngineConfigRun(ctx, job.ID)
	if err != nil {
		s.log.Error().Err(err).Str("задание Engine", job.Name).Msg("не удалось проверить активный запуск")
		return
	}
	if busy {
		s.log.Warn().Str("задание Engine", job.Name).Msg("точка расписания пропущена: предыдущий снимок ещё выполняется")
		return
	}
	if _, err := s.engine.SnapshotEngineConfigJob(ctx, job); err != nil {
		s.log.Error().Err(err).Str("задание Engine", job.Name).Msg("снимок Engine по расписанию не создан")
		return
	}
	if err := s.engine.ApplyEngineConfigRetention(ctx, job); err != nil {
		s.log.Warn().Err(err).Str("задание Engine", job.Name).Msg("ретенция снимков Engine не отработала")
	}
}

func (s *Scheduler) runScheduledFile(jobID string) {
	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	busy, err := s.store.HasActiveFileBackupRun(ctx, jobID)
	if err != nil {
		s.log.Error().Err(err).Str("файловое задание", jobID).Msg("не удалось проверить активные запуски")
		return
	}
	if busy {
		s.log.Warn().Str("файловое задание", jobID).
			Msg("точка расписания пропущена: предыдущий файловый бекап ещё выполняется")
		return
	}
	if _, err := s.fileBackup.Start(ctx, jobID); err != nil {
		s.log.Error().Err(err).Str("файловое задание", jobID).Msg("не удалось запустить файловый бекап по расписанию")
	}
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
		{"@every 15m", "качество бэкапов", s.checkBackupQuality},
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
	scheduledAt := time.Now().UTC()
	jobRun, err := s.TriggerJob(ctx, jobID, "scheduler", &scheduledAt)
	switch {
	case err == nil:
	case errors.Is(err, ErrJobBusy):
		// Не ошибка, а сообщение о том, что задание не укладывается в свой
		// интервал. По уровню «ошибка» оно бы повторялось каждый тик и
		// утопило бы журнал в шуме, скрыв настоящие сбои.
		s.log.Warn().Str("задание", jobID).
			Msg("пропуск запуска: предыдущий ещё выполняется — задание не укладывается в интервал расписания")
		if jobRun != nil {
			_ = s.store.RaiseAlert(ctx, &model.Alert{ServerID: jobRun.ServerID, Scope: model.ScopeBackup,
				ObjectID: jobRun.JobID, ObjectName: jobRun.JobName, Kind: model.AlertBackupScheduleMissed,
				Severity: model.SeverityWarning,
				Message:  fmt.Sprintf("задание «%s» пропустило точку расписания: предыдущий запуск ещё выполняется", jobRun.JobName)})
		}
	default:
		s.log.Error().Err(err).Str("задание", jobID).Msg("запуск задания по расписанию не удался")
	}
}

// TriggerJob executes a job now: it resolves the VMs it covers and queues a
// backup for each. It returns as soon as the runs are queued.
func (s *Scheduler) TriggerJob(ctx context.Context, jobID, triggeredBy string, scheduledAt *time.Time) (*model.BackupJobRun, error) {
	job, err := s.store.GetBackupJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.Type != model.BackupOVA && len(job.StorageTargetIDs) == 0 {
		return nil, fmt.Errorf("у задания %q не выбрано ни одного хранилища", job.Name)
	}

	vms, err := s.resolveVMs(ctx, job)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	jobRun := &model.BackupJobRun{JobID: job.ID, JobName: job.Name, ServerID: job.ServerID,
		TriggeredBy: triggeredBy, ScheduledAt: scheduledAt, Status: model.RunRunning,
		VMCount: len(vms), ReplicaCount: len(vms) * len(job.StorageTargetIDs), StartedAt: &now}
	if job.Type == model.BackupOVA {
		jobRun.ReplicaCount = len(vms)
	}
	if len(vms) == 0 {
		s.log.Warn().Str("задание", job.Name).Msg("задание не выбрало ни одной ВМ")
		jobRun.Status, jobRun.Error, jobRun.EndedAt = model.RunFailed, "задание не выбрало ни одной ВМ", &now
		if err := s.store.CreateBackupJobRun(ctx, jobRun); err != nil {
			return nil, err
		}
		return jobRun, nil
	}

	// Заявка на задание — после всех проверок и до первого бэкапа. Если
	// предыдущий запуск ещё идёт, второй не начинаем: для инкрементальных
	// цепочек два одновременных бэкапа одной ВМ вдобавок соревнуются за
	// родительский чекпоинт и оставляют цепочку в неопределённом состоянии.
	openTasks, openErr := s.store.HasOpenBackupTasks(ctx, job.ID, "")
	if openErr != nil {
		return nil, openErr
	}
	if openTasks || !s.claimJob(job.ID) {
		jobRun.EndedAt = &now
		if scheduledAt != nil {
			jobRun.Status, jobRun.MissedIntervals = model.RunMissed, 1
		} else {
			jobRun.Status = model.RunFailed
		}
		jobRun.Error = "предыдущий запуск ещё выполняется"
		if createErr := s.store.CreateBackupJobRun(ctx, jobRun); createErr != nil {
			return nil, createErr
		}
		return jobRun, fmt.Errorf("%w: задание %q ещё выполняется с прошлого раза; "+
			"дождитесь окончания или отмените текущие бэкапы", ErrJobBusy, job.Name)
	}
	if err := s.store.CreateBackupJobRun(ctx, jobRun); err != nil {
		s.releaseJob(job.ID)
		return nil, err
	}

	s.scheduleMu.Lock()
	var nextRun *time.Time
	if job.Schedule != "" {
		if next, err := ValidateSchedule(job.Schedule, s.Location()); err == nil {
			nextRun = &next
		}
	}
	scheduleStateErr := s.store.SetJobSchedulingState(ctx, job.ID, &now, model.RunRunning, nextRun)
	s.scheduleMu.Unlock()
	if scheduleStateErr != nil {
		s.log.Debug().Err(scheduleStateErr).Msg("не удалось отметить запуск задания")
	}

	s.log.Info().
		Str("задание", job.Name).
		Int("ВМ", len(vms)).
		Int("хранилищ", len(job.StorageTargetIDs)).
		Str("инициатор", triggeredBy).
		Msg("задание запущено")

	var requests []backup.RunRequest

	for _, vm := range vms {
		// Какие хранилища получают данные напрямую от гипервизора:
		//   copy      — только основное, остальные догоняет репликация;
		//   parallel  — тоже только один запуск, но пишет он сразу во все;
		//   separate  — по запуску на каждое, то есть диск читается заново.
		targetIDs := job.StorageTargetIDs
		var mirrorIDs []string
		if job.Type == model.BackupOVA {
			targetIDs = []string{""}
		} else {
			switch job.StorageMode {
			case model.StorageModeCopy:
				targetIDs = job.StorageTargetIDs[:1]
			case model.StorageModeParallel:
				targetIDs = job.StorageTargetIDs[:1]
				mirrorIDs = job.StorageTargetIDs[1:]
			}
		}
		for _, targetID := range targetIDs {
			runType := job.Type
			if job.ForceFullNext {
				runType = model.BackupFull
			}
			req := backup.RunRequest{
				JobRunID:        jobRun.ID,
				ServerID:        job.ServerID,
				VMID:            vm.ID,
				Type:            runType,
				JobID:           job.ID,
				JobName:         job.Name,
				FullEvery:       job.FullEvery,
				FallbackType:    job.FallbackType,
				StorageTargetID: targetID,
				MirrorTargetIDs: mirrorIDs,
				ExcludeDiskIDs:  job.ExcludeDiskIDs,
				Quiesce:         job.Quiesce,
				Encrypt:         job.Encrypt,
				ExportQcow2:     job.ExportQcow2,
				VerifyAfter:     job.VerifyAfter,
				VerifyOptions:   job.VerifyOptions,
				Retention:       job.Retention,
				OVAHostID:       job.OVAHostID,
				OVADirectory:    job.OVADirectory,
				TriggeredBy:     triggeredBy,
			}
			requests = append(requests, req)
		}
	}

	tasks := make([]*model.BackupTask, 0, len(requests))
	for _, req := range requests {
		payload, marshalErr := json.Marshal(queuedBackupTask{Request: req, Job: *job})
		if marshalErr != nil {
			s.releaseJob(job.ID)
			return nil, fmt.Errorf("сериализация задачи бэкапа: %w", marshalErr)
		}
		tasks = append(tasks, &model.BackupTask{
			JobRunID: jobRun.ID, JobID: job.ID, ServerID: req.ServerID, VMID: req.VMID,
			Priority: job.Priority, Concurrency: max(1, job.Concurrency), Payload: payload,
		})
	}
	if err := s.store.EnqueueBackupTasks(ctx, tasks); err != nil {
		s.releaseJob(job.ID)
		jobRun.Status, jobRun.Error = model.RunFailed, err.Error()
		ended := time.Now().UTC()
		jobRun.EndedAt = &ended
		_ = s.store.UpdateBackupJobRun(ctx, jobRun)
		return nil, err
	}
	select {
	case s.taskWake <- struct{}{}:
	default:
	}

	return jobRun, nil
}

type queuedBackupTask struct {
	Request backup.RunRequest `json:"request"`
	Job     model.BackupJob   `json:"job"`
}

// claimJob помечает задание выполняющимся. Возвращает false, если оно уже
// помечено — то есть предыдущий запуск не закончился.
func (s *Scheduler) claimJob(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.active[jobID]; busy {
		return false
	}
	s.active[jobID] = struct{}{}
	return true
}

func (s *Scheduler) releaseJob(jobID string) {
	s.mu.Lock()
	delete(s.active, jobID)
	s.mu.Unlock()
}

// JobActive сообщает, выполняется ли задание прямо сейчас.
func (s *Scheduler) JobActive(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, busy := s.active[jobID]
	return busy
}

// RunOnce ставит в очередь один бэкап вне задания — то, что запускают кнопкой
// «сделать бэкап сейчас».
//
// Идёт тем же путём, что и запуск по расписанию, намеренно: иначе разовые
// бэкапы обходили бы предел backup.workers, не попадали бы в список
// выполняющихся и их нельзя было бы отменить.
func (s *Scheduler) RunOnce(ctx context.Context, req backup.RunRequest) {
	go s.executeOne(ctx, req, nil)
}

const backupTaskLease = 2 * time.Minute

func (s *Scheduler) taskWorker(ctx context.Context, owner string) {
	for {
		tasks, err := s.store.ClaimBackupTasks(ctx, owner, 1, backupTaskLease)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Warn().Err(err).Msg("не удалось получить задачу бэкапа из очереди")
		}
		if len(tasks) == 0 {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.taskWake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		s.executeBackupTask(ctx, owner, tasks[0])
	}
}

func (s *Scheduler) executeBackupTask(ctx context.Context, owner string, task *model.BackupTask) {
	var payload queuedBackupTask
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = s.store.FinishBackupTask(finishCtx, task.ID, owner, model.BackupTaskFailed, err.Error())
		s.maybeFinalizeBackupTaskGroup(finishCtx, task, nil)
		return
	}
	s.mu.Lock()
	s.active[task.JobID] = struct{}{}
	s.mu.Unlock()

	release, err := s.acquireTaskSlot(ctx, task.JobID, task.Concurrency)
	if err != nil {
		return // the lease expires and another leader can resume the work
	}
	defer release()

	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(backupTaskLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.store.RenewBackupTaskLease(context.WithoutCancel(ctx), task.ID, owner, backupTaskLease)
			}
		}
	}()
	execErr := s.executeOne(ctx, payload.Request, &payload.Job)
	close(heartbeatDone)
	status, message := model.BackupTaskSucceeded, ""
	if execErr != nil {
		status, message = model.BackupTaskFailed, execErr.Error()
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.store.FinishBackupTask(finishCtx, task.ID, owner, status, message); err != nil {
		s.log.Warn().Err(err).Str("task", task.ID).Msg("не удалось завершить задачу очереди")
		return
	}
	s.maybeFinalizeBackupTaskGroup(finishCtx, task, &payload.Job)
}

func (s *Scheduler) maybeFinalizeBackupTaskGroup(ctx context.Context, task *model.BackupTask, job *model.BackupJob) {
	open, err := s.store.HasOpenBackupTasks(ctx, "", task.JobRunID)
	if err != nil || open {
		return
	}
	claimed, err := s.store.ClaimBackupJobRunFinalization(ctx, task.JobRunID)
	if err != nil || !claimed {
		return
	}
	s.releaseJob(task.JobID)
	if job == nil {
		job, err = s.store.GetBackupJob(ctx, task.JobID)
		if err != nil {
			s.log.Error().Err(err).Str("запуск", task.JobRunID).Msg("не удалось завершить запуск задания")
			return
		}
	}
	jobRun, err := s.store.GetBackupJobRun(ctx, task.JobRunID)
	if err == nil {
		s.finishJob(ctx, job, jobRun)
	}
}

// recoverTaskFinalizations completes task groups whose last worker finished
// just before a crash. It also handles older queued groups created before the
// durable finalization marker existed.
func (s *Scheduler) recoverTaskFinalizations(ctx context.Context) error {
	runs, err := s.store.ListBackupJobRuns(ctx, store.JobRunFilter{
		Statuses: []model.RunStatus{model.RunRunning, model.RunWaitingCopies}, Limit: 1000,
	})
	if err != nil {
		return err
	}
	for _, run := range runs {
		open, openErr := s.store.HasOpenBackupTasks(ctx, "", run.ID)
		if openErr != nil {
			return openErr
		}
		if open {
			continue
		}
		if run.Status == model.RunRunning {
			claimed, claimErr := s.store.ClaimBackupJobRunFinalization(ctx, run.ID)
			if claimErr != nil {
				return claimErr
			}
			if !claimed {
				continue
			}
		}
		task, taskErr := s.store.GetBackupTaskForJobRun(ctx, run.ID)
		if taskErr != nil {
			s.log.Warn().Err(taskErr).Str("запуск", run.ID).Msg("нет payload для восстановления итога")
			continue
		}
		var payload queuedBackupTask
		if err := json.Unmarshal(task.Payload, &payload); err != nil {
			job, jobErr := s.store.GetBackupJob(ctx, run.JobID)
			if jobErr != nil {
				return fmt.Errorf("запуск %s: повреждён payload: %w; задание: %v", run.ID, err, jobErr)
			}
			payload.Job = *job
		}
		s.releaseJob(run.JobID)
		s.finishJob(ctx, &payload.Job, run)
	}
	return nil
}

func (s *Scheduler) acquireTaskSlot(ctx context.Context, jobID string, concurrency int) (func(), error) {
	if concurrency <= 0 {
		concurrency = 1
	}
	s.taskSlotsMu.Lock()
	sem := s.taskSlots[jobID]
	if sem == nil {
		sem = make(chan struct{}, concurrency)
		s.taskSlots[jobID] = sem
	}
	s.taskSlotsMu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// executeOne runs a single backup, respecting the worker limit.
func (s *Scheduler) executeOne(ctx context.Context, req backup.RunRequest, job *model.BackupJob) (execErr error) {
	select {
	case s.workers <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.workers }()

	defer func() {
		if p := recover(); p != nil {
			s.log.Error().Interface("паника", p).Str("вм", req.VMID).Msg("бэкап завершился аварийно")
			execErr = fmt.Errorf("паника выполнения бэкапа: %v", p)
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

	// Отмену регистрируем по колбэку, а не по результату Execute: результат
	// возвращается, когда бэкап уже закончился, и отменять там нечего.
	// Идентификатор запоминаем, чтобы снять регистрацию даже если Execute
	// вернёт ошибку и никакой записи.
	var runID string
	req.OnRunCreated = func(r *model.BackupRun) {
		runID = r.ID
		s.mu.Lock()
		s.running[r.ID] = cancel
		s.mu.Unlock()
	}
	defer func() {
		if runID == "" {
			return
		}
		s.mu.Lock()
		delete(s.running, runID)
		s.mu.Unlock()
	}()

	run, err := s.engine.Execute(runCtx, req)
	if run != nil {
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
		return err
	}

	_ = s.store.ResolveAlert(ctx, run.ServerID, model.ScopeBackup,
		backupAlertObjectID(run), model.AlertBackupFailed)

	if job != nil && job.ReplicationEnabled && s.replicator != nil && run.Status != model.RunFailed {
		if _, err := s.replicator.QueueRun(context.WithoutCancel(ctx), run.ID, job.StorageTargetIDs); err != nil {
			s.log.Error().Err(err).Str("run", run.ID).Msg("реплики не поставлены в очередь")
			s.raiseBackupAlert(ctx, run, fmt.Errorf("постановка реплик в очередь: %w", err))
		}
	}

	if req.VerifyAfter != "" && run.Status != model.RunFailed {
		primary, copyErr := s.store.GetBackupCopyForTarget(ctx, run.ID, run.StorageTargetID)
		if copyErr != nil {
			s.log.Warn().Err(copyErr).Str("run", run.ID).Msg("не удалось определить основную копию для проверки")
			s.raiseVerifyAlert(ctx, run, copyErr)
		} else if _, err := s.engine.VerifyCopy(ctx, run.ID, primary.ID, req.VerifyAfter, req.VerifyOptions); err != nil {
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
	return nil
}

func (s *Scheduler) finishJob(ctx context.Context, job *model.BackupJob, jobRun *model.BackupJobRun) {
	runs, err := s.store.ListBackupRuns(ctx, store.RunFilter{
		JobRunID:       jobRun.ID,
		IncludeDeleted: true,
	})
	if err != nil {
		return
	}

	jobRun.SucceededCount, jobRun.PartialCount, jobRun.FailedCount, jobRun.CanceledCount = 0, 0, 0, 0
	status := model.RunSucceeded
	primaryFailed := false
	waitingCopies := 0
	targets := make(map[string]struct{}, len(job.StorageTargetIDs))
	for _, targetID := range job.StorageTargetIDs {
		targets[targetID] = struct{}{}
	}
	for _, r := range runs {
		if job.StorageMode == model.StorageModeCopy || job.StorageMode == model.StorageModeParallel {
			copies, copyErr := s.store.ListBackupCopies(ctx, r.ID)
			if copyErr != nil {
				jobRun.FailedCount += len(job.StorageTargetIDs)
				continue
			}
			for _, copy := range copies {
				if !copy.Required {
					continue
				}
				if _, expected := targets[copy.StorageTargetID]; !expected {
					continue
				}
				if copy.Healthy() {
					jobRun.SucceededCount++
				} else {
					switch copy.Status {
					case model.CopyCanceled:
						jobRun.CanceledCount++
					case model.CopyPending, model.CopyCopying, model.CopyVerifying:
						waitingCopies++
					case model.CopyFailed:
						if copy.NextRetryAt != nil {
							waitingCopies++
						} else {
							jobRun.FailedCount++
						}
					default:
						jobRun.FailedCount++
					}
				}
				if copy.Role == model.CopyPrimary && !copy.Healthy() {
					primaryFailed = true
				}
			}
			continue
		}
		switch r.Status {
		case model.RunSucceeded:
			jobRun.SucceededCount++
		case model.RunPartial:
			jobRun.PartialCount++
		case model.RunCanceled:
			jobRun.CanceledCount++
		default:
			jobRun.FailedCount++
		}
	}
	accounted := jobRun.SucceededCount + jobRun.PartialCount + jobRun.FailedCount + jobRun.CanceledCount + waitingCopies
	if accounted < jobRun.ReplicaCount {
		jobRun.FailedCount += jobRun.ReplicaCount - accounted
	}
	switch {
	case primaryFailed:
		status = model.RunFailed
	case waitingCopies > 0:
		status = model.RunWaitingCopies
	case jobRun.FailedCount > 0 && jobRun.SucceededCount == 0 && jobRun.PartialCount == 0:
		// If no VM/copy produced a usable result, the whole job failed.  This
		// also covers a worker that failed before it could create a BackupRun:
		// the missing replica is accounted as failed just above.
		status = model.RunFailed
	case jobRun.FailedCount > 0 || jobRun.PartialCount > 0:
		status = model.RunPartial
	case jobRun.CanceledCount == jobRun.ReplicaCount:
		status = model.RunCanceled
	case jobRun.CanceledCount > 0:
		status = model.RunPartial
	}

	now := time.Now().UTC()
	jobRun.Status = status
	if status.Terminal() {
		jobRun.EndedAt = &now
	} else {
		jobRun.EndedAt = nil
	}
	if err := s.store.UpdateBackupJobRun(ctx, jobRun); err != nil {
		s.log.Warn().Err(err).Str("запуск", jobRun.ID).Msg("не удалось сохранить итог пакетного запуска")
	}
	if job.ForceFullNext && !primaryFailed {
		_ = s.store.ClearForceFullNext(ctx, job.ID)
	}
	s.scheduleMu.Lock()
	var nextRun *time.Time
	if job.Schedule != "" {
		if next, err := ValidateSchedule(job.Schedule, s.Location()); err == nil {
			nextRun = &next
		}
	}
	scheduleStateErr := s.store.SetJobSchedulingState(ctx, job.ID, &now, status, nextRun)
	s.scheduleMu.Unlock()
	if scheduleStateErr != nil {
		s.log.Debug().Err(scheduleStateErr).Msg("не удалось сохранить итог задания")
	}
	if status == model.RunSucceeded {
		_ = s.store.ResolveAlert(ctx, job.ServerID, model.ScopeBackup, job.ID, model.AlertBackupScheduleMissed)
	}
	s.bus.Publish(events.Event{
		Kind: events.KindJob, ServerID: job.ServerID, ObjectID: jobRun.ID, Payload: jobRun,
		Message: fmt.Sprintf("задание «%s»: %s", job.Name, status),
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

	selector, err := model.NewVMSelector(job)
	if err != nil {
		return nil, err
	}

	out := make([]*model.VM, 0, len(all))
	for _, vm := range all {
		if match, _ := selector.Match(vm); match {
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
	if s.fileBackup != nil {
		if err := s.fileBackup.ApplyRetention(ctx); err != nil {
			s.log.Warn().Err(err).Msg("ретенция файловых бекапов не отработала")
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
		if err := s.store.AddStorageUsageSample(ctx, &model.StorageUsageSample{
			StorageTargetID: target.ID, CheckOK: checkErr == nil,
			CapacityKnown: target.Kind != model.StorageS3 && free+used > 0,
			FreeBytes:     free, UsedBytes: used,
		}); err != nil {
			s.log.Debug().Err(err).Str("хранилище", target.Name).
				Msg("не удалось сохранить пробу ёмкости хранилища")
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

func (s *Scheduler) checkBackupQuality(ctx context.Context) {
	if s.quality == nil {
		return
	}
	if err := s.quality.EvaluateAlerts(ctx); err != nil {
		s.log.Warn().Err(err).Msg("не удалось пересчитать качество бэкапов")
	}
}

func (s *Scheduler) purgeHistory(ctx context.Context) {
	if s.quality != nil {
		cutoff := time.Now().Add(-time.Duration(s.quality.Settings().HistoryRetentionDays) * 24 * time.Hour)
		if n, err := s.store.PurgeStorageUsageSamples(ctx, cutoff); err == nil && n > 0 {
			s.log.Debug().Int64("удалено", n).Msg("история ёмкости хранилищ очищена")
		}
	}
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
		ServerID: run.ServerID, Scope: model.ScopeBackup, ObjectID: backupAlertObjectID(run), ObjectName: run.VMName,
		Kind: model.AlertBackupFailed, Severity: model.SeverityCritical,
		Message: fmt.Sprintf("бэкап ВМ %s не выполнен: %v", run.VMName, cause),
		Details: run.Error,
	})
}

func backupAlertObjectID(run *model.BackupRun) string {
	return strings.Join([]string{run.JobID, run.VMID, run.StorageTargetID}, "/")
}

func (s *Scheduler) raiseVerifyAlert(ctx context.Context, run *model.BackupRun, cause error) {
	_ = s.store.RaiseAlert(ctx, &model.Alert{
		ServerID: run.ServerID, Scope: model.ScopeBackup, ObjectID: run.ID, ObjectName: run.VMName,
		Kind: model.AlertVerifyFailed, Severity: model.SeverityCritical,
		Message: fmt.Sprintf("проверка бэкапа ВМ %s не пройдена: %v", run.VMName, cause),
	})
}
