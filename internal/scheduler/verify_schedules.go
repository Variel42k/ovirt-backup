package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Расписания проверки загрузкой.
//
// Расписание не снимает нового бэкапа: оно берёт последнюю успешную копию
// каждой выбранной ВМ и поднимает её на площадке проверки. Так проверку
// загрузкой можно проводить реже бэкапов — например, раз в неделю ночью, —
// и на площадке, которой нет в заданиях.

// verifyScheduleKey — ключ расписания проверок в entries и active.
func verifyScheduleKey(id string) string { return "verify:" + id }

// registerVerifySchedules добавляет расписания проверок в cron. Вызывается
// из reload под s.mu.
func (s *Scheduler) registerVerifySchedules(schedules []*model.VerifySchedule, timezone string, loc *time.Location) int {
	active := 0
	for _, v := range schedules {
		if !v.Enabled || v.Schedule == "" {
			continue
		}
		if _, err := ValidateSchedule(v.Schedule, loc); err != nil {
			s.log.Error().Err(err).Str("расписание проверок", v.Name).
				Msg("расписание пропущено: не удалось разобрать cron")
			continue
		}
		id := v.ID
		entryID, err := s.cron.AddFunc(cronSpecInTimezone(v.Schedule, timezone), func() {
			s.runScheduledVerify(id)
		})
		if err != nil {
			s.log.Error().Err(err).Str("расписание проверок", v.Name).Msg("не удалось зарегистрировать расписание")
			continue
		}
		s.entries[verifyScheduleKey(id)] = entryID
		active++
	}
	return active
}

func (s *Scheduler) runScheduledVerify(scheduleID string) {
	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := s.RunVerifySchedule(ctx, scheduleID); err != nil {
		s.log.Warn().Err(err).Str("расписание проверок", scheduleID).Msg("прогон расписания проверок не выполнен")
	}
}

// VerifyScheduleActive сообщает, идёт ли прогон расписания проверок.
func (s *Scheduler) VerifyScheduleActive(scheduleID string) bool {
	return s.JobActive(verifyScheduleKey(scheduleID))
}

// ErrVerifyScheduleBusy — предыдущий прогон расписания ещё идёт.
var ErrVerifyScheduleBusy = fmt.Errorf("предыдущий прогон этого расписания проверок ещё идёт")

// StartVerifySchedule запускает прогон расписания в фоне — кнопка
// «проверить сейчас».
func (s *Scheduler) StartVerifySchedule(scheduleID string) error {
	if s.VerifyScheduleActive(scheduleID) {
		return ErrVerifyScheduleBusy
	}
	go s.runScheduledVerify(scheduleID)
	return nil
}

// VerifyScheduleOutcome — итог прогона расписания проверок.
type VerifyScheduleOutcome struct {
	Status  string
	Detail  string
	Passed  []string
	Failed  []string
	Skipped []string
}

// RunVerifySchedule проверяет загрузкой последние копии ВМ расписания.
func (s *Scheduler) RunVerifySchedule(ctx context.Context, scheduleID string) (*VerifyScheduleOutcome, error) {
	key := verifyScheduleKey(scheduleID)
	if !s.claimJob(key) {
		s.log.Warn().Str("расписание проверок", scheduleID).
			Msg("точка расписания пропущена: предыдущий прогон проверок ещё идёт")
		return nil, ErrVerifyScheduleBusy
	}
	defer s.releaseJob(key)

	sched, err := s.store.GetVerifySchedule(ctx, scheduleID)
	if err != nil {
		return nil, fmt.Errorf("расписание проверок: %w", err)
	}
	log := s.log.With().Str("расписание проверок", sched.Name).Logger()
	started := time.Now().UTC()

	outcome := &VerifyScheduleOutcome{}
	finish := func() (*VerifyScheduleOutcome, error) {
		outcome.Status, outcome.Detail = summariseVerifyOutcome(outcome)
		if err := s.store.SetVerifyScheduleResult(context.WithoutCancel(ctx), sched.ID, started,
			outcome.Status, outcome.Detail); err != nil {
			log.Warn().Err(err).Msg("итог прогона расписания проверок не сохранён")
		}
		log.Info().Str("итог", outcome.Status).Str("подробности", outcome.Detail).Msg("прогон расписания проверок завершён")
		return outcome, nil
	}

	target, err := s.store.GetVerifyTarget(ctx, sched.TargetID)
	if err != nil {
		outcome.Skipped = append(outcome.Skipped, "площадка проверки недоступна: "+err.Error())
		return finish()
	}
	runs, err := s.store.LatestVerifiableRuns(ctx, sched.ServerID, sched.VMIDs, sched.StorageTargetID)
	if err != nil {
		outcome.Skipped = append(outcome.Skipped, err.Error())
		return finish()
	}
	plan := planScheduledVerify(sched, runs, time.Now())
	outcome.Skipped = append(outcome.Skipped, plan.skipped...)
	log.Info().Int("ВМ", len(plan.runs)).Str("площадка", target.Name).Msg("прогон расписания проверок начат")

	// Проверки идут не больше, чем площадка ведёт одновременно: очередь
	// площадки всё равно не пустит больше, а лишние записи «ожидает» в
	// журнале только путают.
	parallel := max(target.MaxParallel, 1)
	slots := make(chan struct{}, parallel)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, run := range plan.runs {
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(run *model.BackupRun) {
			defer wg.Done()
			defer func() { <-slots }()
			err := s.verifyScheduledRun(ctx, sched, run)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				outcome.Failed = append(outcome.Failed, fmt.Sprintf("%s: %v", run.VMName, err))
				s.raiseVerifyAlert(ctx, run, err)
				return
			}
			outcome.Passed = append(outcome.Passed, run.VMName)
		}(run)
	}
	wg.Wait()
	return finish()
}

func (s *Scheduler) verifyScheduledRun(ctx context.Context, sched *model.VerifySchedule, run *model.BackupRun) error {
	copyID := ""
	if sched.StorageTargetID != "" {
		c, err := s.store.GetBackupCopyForTarget(ctx, run.ID, sched.StorageTargetID)
		if err != nil {
			return fmt.Errorf("копия в выбранном хранилище: %w", err)
		}
		copyID = c.ID
	}
	opts := model.VerifyOptions{TargetID: sched.TargetID, TriggeredBy: model.VerifyTriggerSchedule + ":" + sched.ID}
	_, err := s.engine.VerifyCopy(ctx, run.ID, copyID, model.VerifyBoot, opts)
	return err
}

type scheduledVerifyPlan struct {
	runs    []*model.BackupRun
	skipped []string
}

// planScheduledVerify решает, какие копии проверять: ВМ без успешных бэкапов
// и слишком старые копии пропускаются с объяснением.
func planScheduledVerify(sched *model.VerifySchedule, runs []*model.BackupRun, now time.Time) scheduledVerifyPlan {
	var plan scheduledVerifyPlan
	found := map[string]bool{}
	for _, run := range runs {
		found[run.VMID] = true
		if sched.MaxAgeHours > 0 {
			if age := now.Sub(run.CreatedAt); age > time.Duration(sched.MaxAgeHours)*time.Hour {
				plan.skipped = append(plan.skipped, fmt.Sprintf("%s: последняя копия старше %d ч (%s)",
					run.VMName, sched.MaxAgeHours, age.Round(time.Hour)))
				continue
			}
		}
		plan.runs = append(plan.runs, run)
	}
	for _, vmID := range sched.VMIDs {
		if !found[vmID] {
			plan.skipped = append(plan.skipped, fmt.Sprintf("ВМ %s: нет успешных бэкапов для проверки", vmID))
		}
	}
	if len(sched.VMIDs) == 0 && len(runs) == 0 {
		plan.skipped = append(plan.skipped, "у ВМ подключения нет успешных бэкапов для проверки")
	}
	return plan
}

// summariseVerifyOutcome сводит прогон к статусу и короткому описанию для
// списка расписаний.
func summariseVerifyOutcome(o *VerifyScheduleOutcome) (string, string) {
	status := string(model.RunSucceeded)
	switch {
	case len(o.Passed) == 0:
		status = string(model.RunFailed)
	case len(o.Failed) > 0 || len(o.Skipped) > 0:
		status = string(model.RunPartial)
	}
	parts := []string{fmt.Sprintf("загрузились: %d", len(o.Passed))}
	if len(o.Failed) > 0 {
		parts = append(parts, fmt.Sprintf("не прошли: %d (%s)", len(o.Failed), strings.Join(o.Failed, "; ")))
	}
	if len(o.Skipped) > 0 {
		parts = append(parts, fmt.Sprintf("пропущены: %d (%s)", len(o.Skipped), strings.Join(o.Skipped, "; ")))
	}
	detail := strings.Join(parts, ", ")
	if r := []rune(detail); len(r) > 2000 {
		detail = string(r[:2000]) + "…"
	}
	return status, detail
}

// jobVerifyOptions помечает проверку после бэкапа для журнала.
func jobVerifyOptions(opts model.VerifyOptions) model.VerifyOptions {
	opts.TriggeredBy = model.VerifyTriggerJob
	return opts
}
