package monitor

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Remediator performs the corrective actions — the "оживление" half of the
// service.
//
// Every decision it makes is written down, including the ones it declines, so
// that "why did nothing happen at 4am" has an answer. Three gates stand between
// a detected problem and an action: the action class must be permitted, the
// object must not have been acted on too recently (cooldown), and it must not
// have exhausted its hourly attempt budget. Anything that fails a gate is
// recorded as skipped with the reason.
type Remediator struct {
	store   *store.Store
	pool    *ovirt.Pool
	libvirt *libvirtx.Pool
	cfg     config.RemediationConfig
	bus     *events.Bus
	log     zerolog.Logger

	// dryRun is switched at runtime, so it is read on every decision rather
	// than taken from the config. Restarting the service to leave check mode
	// would mean a gap in monitoring at exactly the moment the operator has
	// decided the automation should start acting.
	dryRun atomic.Bool
	// periodID identifies the mode period the current decisions belong to; the
	// archive built when the period closes is assembled from its time window.
	periodID atomic.Value
}

// NewRemediator builds the auto-revive engine.
func NewRemediator(st *store.Store, pool *ovirt.Pool, libvirtPool *libvirtx.Pool,
	cfg config.RemediationConfig, bus *events.Bus, log zerolog.Logger) *Remediator {
	r := &Remediator{store: st, pool: pool, libvirt: libvirtPool, cfg: cfg, bus: bus, log: log}
	r.dryRun.Store(cfg.DryRun)
	r.periodID.Store("")
	return r
}

// SetMode switches between check and live mode at runtime.
func (r *Remediator) SetMode(dryRun bool, periodID string) {
	r.dryRun.Store(dryRun)
	r.periodID.Store(periodID)
}

// DryRun reports whether decisions are currently being recorded instead of
// performed.
func (r *Remediator) DryRun() bool { return r.dryRun.Load() }

// PeriodID returns the mode period decisions are currently attributed to.
func (r *Remediator) PeriodID() string {
	id, _ := r.periodID.Load().(string)
	return id
}

// Situation is a detected problem and the action proposed for it.
type Situation struct {
	ServerID   string
	Scope      model.Scope
	ObjectID   string
	ObjectName string
	Action     model.RemediationAction
	Reason     string
	// TriggeredBy отличает автоматическое решение от ручного запуска.
	TriggeredBy string
	// Force обходит cooldown и лимит попыток — только для действий,
	// запрошенных человеком.
	Force bool
}

// Consider evaluates a situation and, if all gates pass, performs the action.
// It returns the record written for this decision.
func (r *Remediator) Consider(ctx context.Context, sit Situation) (*model.RemediationRecord, error) {
	if sit.TriggeredBy == "" {
		sit.TriggeredBy = "monitor"
	}

	record := &model.RemediationRecord{
		ID:          uuid.NewString(),
		ServerID:    sit.ServerID,
		Scope:       sit.Scope,
		ObjectID:    sit.ObjectID,
		ObjectName:  sit.ObjectName,
		Action:      sit.Action,
		Reason:      sit.Reason,
		Status:      model.RemPlanned,
		Attempt:     1,
		TriggeredBy: sit.TriggeredBy,
		CreatedAt:   time.Now().UTC(),
	}

	dryRun := r.dryRun.Load()

	// Ситуация держится дольше одного опроса: лежащая ВМ через минуту лежит
	// по-прежнему, и решение по ней то же самое. В живом режиме от повторов
	// защищает пауза между попытками — она заодно задаёт, насколько часто
	// действие вообще могло бы состояться. Здесь та же пауза применяется к
	// записи: история периода наблюдения должна показывать, что происходило бы
	// в бою, а не с какой частотой опрашивается мониторинг. Без этого страница
	// оповещений набирала запись каждую минуту, пока проблема не устранена.
	//
	// Ручной запуск сюда не попадает: он приходит с Force, повторов у него нет
	// по определению, а его запись ждёт интерфейс.
	//
	// Пропущенные решения LastRemediationAt намеренно не учитывает, поэтому
	// отсчёт ведётся от последнего зафиксированного решения, а не от отказа.
	if dryRun && !sit.Force && r.cfg.Cooldown > 0 {
		last, err := r.store.LastRemediationAt(ctx, sit.ServerID, sit.ObjectID, sit.Action)
		if err != nil {
			return nil, err
		}
		if !last.IsZero() && time.Since(last) < r.cfg.Cooldown {
			r.log.Debug().
				Str("объект", sit.ObjectName).
				Str("действие", string(sit.Action)).
				Dur("прошло", time.Since(last).Round(time.Second)).
				Msg("режим наблюдения: то же решение уже записано, повтор не фиксируется")
			// Запись возвращается незаписанной: вызывающему нужен не факт
			// сохранения, а понимание, что предложено. В журнал и в историю она
			// не идёт.
			return record, nil
		}
	}

	skip, err := r.gate(ctx, sit)
	if err != nil {
		return nil, err
	}
	if skip != "" {
		record.Status = model.RemSkipped
		record.Error = skip
		ended := time.Now().UTC()
		record.EndedAt = &ended
		if err := r.store.RecordRemediation(ctx, record); err != nil {
			r.log.Warn().Err(err).Msg("не удалось записать пропущенное действие")
		}
		// In check mode a skip is as interesting as an action: the operator is
		// deciding whether to trust the automation, and "it wanted to act but
		// the policy stopped it" is exactly the kind of thing that has to be
		// visible. Outside check mode the same line would be noise at every
		// poll, so it stays at debug level there.
		if dryRun {
			r.decisionLog(sit, record).
				Str("исход", "пропущено").
				Str("что помешало", skip).
				Msg("действие предложено, но не прошло ворота")
		} else {
			r.log.Debug().Str("объект", sit.ObjectName).Str("действие", string(sit.Action)).
				Str("причина", skip).Msg("действие не выполнено")
		}
		return record, nil
	}

	attempts, err := r.store.CountRecentRemediations(ctx, sit.ServerID, sit.ObjectID, sit.Action,
		time.Now().Add(-time.Hour))
	if err == nil {
		record.Attempt = attempts + 1
	}

	if dryRun {
		record.Status = model.RemDryRun
		ended := time.Now().UTC()
		record.EndedAt = &ended
		if err := r.store.RecordRemediation(ctx, record); err != nil {
			return nil, err
		}
		r.decisionLog(sit, record).
			Str("исход", "подавлено режимом проверки").
			Str("было бы выполнено", r.describeEffect(sit)).
			Msg("действие зафиксировано, но не выполнено")
		r.publish(record)
		return record, nil
	}

	record.Status = model.RemRunning
	if err := r.store.RecordRemediation(ctx, record); err != nil {
		return nil, err
	}

	r.log.Warn().
		Str("объект", sit.ObjectName).
		Str("действие", sit.Action.Title()).
		Str("причина", sit.Reason).
		Int("попытка", record.Attempt).
		Msg("выполняется восстановительное действие")

	execErr := r.execute(ctx, sit)
	ended := time.Now().UTC()
	record.EndedAt = &ended
	if execErr != nil {
		record.Status = model.RemFailed
		record.Error = execErr.Error()
		r.log.Error().Err(execErr).Str("объект", sit.ObjectName).
			Str("действие", sit.Action.Title()).Msg("восстановительное действие не удалось")
	} else {
		record.Status = model.RemSucceeded
		r.log.Info().Str("объект", sit.ObjectName).
			Str("действие", sit.Action.Title()).Msg("восстановительное действие выполнено")
	}

	if err := r.store.UpdateRemediation(ctx, record); err != nil {
		r.log.Warn().Err(err).Msg("не удалось обновить запись о действии")
	}
	r.publish(record)
	return record, execErr
}

// decisionLog builds the log entry for one decision in check mode.
//
// Check mode exists so an operator can judge the automation before letting it
// act, and a judgement needs the whole picture: which object, on which
// connection, what the automation saw, what it proposed, and which attempt this
// would be against the hourly budget. The normal mode logs a shorter line
// because it repeats at every poll; here the verbosity is the point.
func (r *Remediator) decisionLog(sit Situation, record *model.RemediationRecord) *zerolog.Event {
	event := r.log.Debug().
		Str("режим", "проверка").
		Str("объект", sit.ObjectName).
		Str("тип объекта", string(sit.Scope)).
		Str("id объекта", sit.ObjectID).
		Str("действие", sit.Action.Title()).
		Str("почему предложено", sit.Reason).
		Str("инициатор", sit.TriggeredBy).
		Int("попытка за час", record.Attempt)

	if srv, err := r.store.GetServer(context.Background(), sit.ServerID); err == nil {
		event = event.Str("подключение", srv.Name)
	}
	if sit.Action.Disruptive() {
		event = event.Bool("прерывает работу", true)
	}
	if period := r.PeriodID(); period != "" {
		event = event.Str("период", period)
	}
	return event
}

// describeEffect spells out what the action would have done, so the log answers
// "and what would that have meant" without the reader having to know the code.
func (r *Remediator) describeEffect(sit Situation) string {
	switch sit.Action {
	case model.ActionVMStart:
		return fmt.Sprintf("ВМ %q была бы запущена", sit.ObjectName)
	case model.ActionVMUnpause:
		return fmt.Sprintf("ВМ %q была бы снята с паузы", sit.ObjectName)
	case model.ActionVMReset:
		return fmt.Sprintf("ВМ %q была бы аппаратно сброшена — гостевая ОС потеряла бы несохранённые данные", sit.ObjectName)
	case model.ActionHostActivate:
		return fmt.Sprintf("хост %q был бы выведен из обслуживания", sit.ObjectName)
	case model.ActionHostFence:
		return fmt.Sprintf("хост %q был бы перезагружен по питанию вместе со всеми его ВМ", sit.ObjectName)
	case model.ActionReconnect:
		return "соединение с движком было бы переустановлено"
	default:
		return string(sit.Action)
	}
}

// gate returns a non-empty reason when the action must not be performed.
func (r *Remediator) gate(ctx context.Context, sit Situation) (string, error) {
	if sit.TriggeredBy == "monitor" && !r.cfg.Enabled {
		return "автоматическое восстановление выключено в настройках", nil
	}

	if !r.allowed(sit.Action) {
		return fmt.Sprintf("действие «%s» не разрешено политикой", sit.Action.Title()), nil
	}

	if sit.Scope == model.ScopeVM {
		vm, err := r.store.GetVM(ctx, sit.ServerID, sit.ObjectID)
		if err == nil && vm.RemediationOptOut {
			return "для этой ВМ автоматические действия отключены вручную", nil
		}
	}

	// A bare libvirt host has no engine above it to activate or fence it
	// through. Recording that as a skip with the reason is more useful than
	// letting the attempt fail later with the same explanation.
	if srv, err := r.store.GetServer(ctx, sit.ServerID); err == nil && srv.Kind.UsesLibvirt() {
		switch sit.Action {
		case model.ActionHostActivate, model.ActionHostFence:
			return "хостом libvirt управляет его операционная система, а не движок — " +
				"действие неприменимо", nil
		}
	}

	// A human asking for an action explicitly has already made the judgement
	// call that the rate limits exist to make on their behalf.
	if sit.Force {
		return "", nil
	}

	if r.cfg.Cooldown > 0 {
		last, err := r.store.LastRemediationAt(ctx, sit.ServerID, sit.ObjectID, sit.Action)
		if err != nil {
			return "", err
		}
		if !last.IsZero() && time.Since(last) < r.cfg.Cooldown {
			return fmt.Sprintf("с предыдущей попытки прошло %s, пауза между попытками — %s",
				time.Since(last).Round(time.Second), r.cfg.Cooldown), nil
		}
	}

	if r.cfg.MaxAttemptsPerHour > 0 {
		attempts, err := r.store.CountRecentRemediations(ctx, sit.ServerID, sit.ObjectID, sit.Action,
			time.Now().Add(-time.Hour))
		if err != nil {
			return "", err
		}
		if attempts >= r.cfg.MaxAttemptsPerHour {
			// Something that needed reviving three times in an hour is broken
			// in a way restarting will not fix; escalating to a human is the
			// correct behaviour.
			return fmt.Sprintf("исчерпан лимит попыток: %d за час", attempts), nil
		}
	}
	return "", nil
}

func (r *Remediator) allowed(action model.RemediationAction) bool {
	switch action {
	case model.ActionVMStart:
		return r.cfg.AllowVMStart
	case model.ActionVMUnpause:
		return r.cfg.AllowVMUnpause
	case model.ActionHostActivate:
		return r.cfg.AllowHostActivate
	case model.ActionHostFence:
		return r.cfg.AllowHostFence
	case model.ActionVMReset:
		// A reset is as destructive to the guest as a fence is to the host and
		// is never taken automatically.
		return false
	case model.ActionReconnect:
		return true
	default:
		return false
	}
}

func (r *Remediator) execute(ctx context.Context, sit Situation) error {
	if srv, err := r.store.GetServer(ctx, sit.ServerID); err == nil && srv.Kind.UsesLibvirt() {
		return r.executeLibvirt(ctx, sit)
	}

	client, err := r.pool.Get(ctx, sit.ServerID)
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	switch sit.Action {
	case model.ActionVMStart, model.ActionVMUnpause:
		// oVirt resumes a paused VM through the same call that boots a stopped
		// one, so both situations share an implementation.
		if err := client.StartVM(opCtx, sit.ObjectID); err != nil {
			return err
		}
		status, err := client.WaitVMStatus(opCtx, sit.ObjectID,
			[]string{"up", "powering_up", "wait_for_launch", "restoring_state"}, 2*time.Minute)
		if err != nil {
			return fmt.Errorf("ВМ не поднялась (текущее состояние %s): %w", status, err)
		}
		return nil

	case model.ActionHostActivate:
		if err := client.ActivateHost(opCtx, sit.ObjectID); err != nil {
			return err
		}
		status, err := client.WaitHostStatus(opCtx, sit.ObjectID, []string{"up"}, 3*time.Minute)
		if err != nil {
			return fmt.Errorf("хост не активировался (текущее состояние %s): %w", status, err)
		}
		return nil

	case model.ActionHostFence:
		if err := client.FenceHost(opCtx, sit.ObjectID, "restart"); err != nil {
			return err
		}
		status, err := client.WaitHostStatus(opCtx, sit.ObjectID, []string{"up"}, 10*time.Minute)
		if err != nil {
			return fmt.Errorf("хост не вернулся в строй после перезагрузки (состояние %s): %w", status, err)
		}
		return nil

	case model.ActionVMReset:
		return client.ResetVM(opCtx, sit.ObjectID)

	case model.ActionReconnect:
		r.pool.Invalidate(sit.ServerID)
		_, err := r.pool.Get(opCtx, sit.ServerID)
		return err

	default:
		return fmt.Errorf("неизвестное действие: %q", sit.Action)
	}
}

func (r *Remediator) publish(record *model.RemediationRecord) {
	if r.bus == nil {
		return
	}
	r.bus.Publish(events.Event{
		Kind:     events.KindRemediation,
		ServerID: record.ServerID,
		ObjectID: record.ObjectID,
		Message: fmt.Sprintf("%s: %s (%s)", record.ObjectName, record.Action.Title(),
			record.Status),
		Payload: record,
	})
}
