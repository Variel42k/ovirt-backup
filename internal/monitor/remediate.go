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
	"github.com/Variel42k/ovirt-backup/internal/proxmox"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Remediator выполняет действия по восстановлению.
//
// Записываем каждое решение, в том числе пропущенные действия. Класс действия должен быть
// разрешён, минимальная пауза после предыдущего действия должна истечь, а часовой лимит
// попыток не должен быть исчерпан. При отказе указываем точную причину.
type Remediator struct {
	store   *store.Store
	pool    *ovirt.Pool
	libvirt *libvirtx.Pool
	proxmox *proxmox.Pool
	cfg     config.RemediationConfig
	bus     *events.Bus
	log     zerolog.Logger

	// dryRun меняется без перезапуска и считывается при каждом решении. Перезапуск при
	// смене режима прервал бы мониторинг именно в момент включения реальных действий.
	dryRun atomic.Bool
	// periodID идентифицирует период режима, к которому относятся текущие решения; архив,
	// построенный при закрытии периода, собирается из его временного окна.
	periodID atomic.Value
}

// NewRemediator постройте автоматический движок.
func NewRemediator(st *store.Store, pool *ovirt.Pool, libvirtPool *libvirtx.Pool,
	cfg config.RemediationConfig, bus *events.Bus, log zerolog.Logger) *Remediator {
	r := &Remediator{store: st, pool: pool, libvirt: libvirtPool, cfg: cfg, bus: bus, log: log}
	r.dryRun.Store(cfg.DryRun)
	r.periodID.Store("")
	return r
}

// SetProxmoxPool добавляет драйвер Proxmox управления, сохраняя конструктор, используемый
// существующими интеграциями.
func (r *Remediator) SetProxmoxPool(pool *proxmox.Pool) { r.proxmox = pool }

// SetMode переключается между проверочным и живым режимом во время выполнения.
func (r *Remediator) SetMode(dryRun bool, periodID string) {
	r.dryRun.Store(dryRun)
	r.periodID.Store(periodID)
}

// DryRun показывает, включён ли режим записи решений без выполнения действий.
func (r *Remediator) DryRun() bool { return r.dryRun.Load() }

// PeriodID возвращает время, в течение которого принимаются решения.
func (r *Remediator) PeriodID() string {
	id, _ := r.periodID.Load().(string)
	return id
}

// Situation — обнаруженная проблема и предлагаемое для нее действие.
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

// Рассматривает оценивает ситуацию и, если все ворота проходят, выполняет действие.
// Он возвращает запись, написанную для этого решения.
func (r *Remediator) Consider(ctx context.Context, sit Situation) (*model.RemediationRecord, error) {
	if sit.TriggeredBy == "" {
		sit.TriggeredBy = "monitor"
	}
	if sit.TriggeredBy == "cluster-recovery" {
		lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lock, err := r.store.TryVMRecoveryLock(lockCtx, sit.ServerID, sit.ObjectID)
		cancel()
		if err != nil {
			return nil, err
		}
		if lock == nil {
			return nil, nil
		}
		defer lock.Release()
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
		// В режиме проверки оператору важно видеть, какие действия остановила политика. В
		// рабочем режиме такое решение пишется на уровне debug, чтобы не засорять журнал
		// при каждом опросе.
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

// decisionLog строит запись журнала для одного решения в режиме проверки.
//
// Режим проверки записывает объект, подключение, наблюдаемое состояние, предложенное
// действие и номер попытки относительно часового лимита. В рабочем режиме запись короче,
// поскольку она появляется при каждом опросе.
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

// describeEffect поясняет, что сделал бы монитор, чтобы оператор мог понять запись в журнале
// без чтения кода.
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

// gate возвращает непустую причину, когда действие не должно быть исполнено.
func (r *Remediator) gate(ctx context.Context, sit Situation) (string, error) {
	if (sit.TriggeredBy == "monitor" || sit.TriggeredBy == "cluster-recovery") && !r.cfg.Enabled {
		return "автоматическое восстановление выключено в настройках", nil
	}

	// Восстановление кластера имеет свою собственную явную, проверенную разрешением опцию
	// сброса.
	if sit.TriggeredBy != "cluster-recovery" && !r.actionAllowed(sit) {
		return fmt.Sprintf("действие «%s» не разрешено политикой", sit.Action.Title()), nil
	}

	if sit.Scope == model.ScopeVM {
		owner, err := r.store.VMTransferOwner(ctx, sit.ServerID, sit.ObjectID)
		if err != nil {
			return "", err
		}
		if owner != "" {
			return "ВМ участвует в миграции " + owner, nil
		}
		vm, err := r.store.GetVM(ctx, sit.ServerID, sit.ObjectID)
		if err != nil {
			return "", err
		}
		if err == nil && vm.RemediationOptOut {
			return "для этой ВМ автоматические действия отключены вручную", nil
		}
		p, err := r.store.GetClusterRecoveryPolicy(ctx, sit.ServerID, vm.ClusterID)
		if err != nil {
			return "", err
		}
		if sit.TriggeredBy == "cluster-recovery" {
			if !p.Configured || !p.VM.AppliesTo(vm) {
				return "нет политики восстановления кластера", nil
			}
			if reason, e := r.clusterRecoveryGate(ctx, sit, p.VM); reason != "" || e != nil {
				return reason, e
			}
			return r.confirmClusterRecovery(ctx, sit)
		}
		if sit.TriggeredBy == "monitor" && p.Configured && p.VM.AppliesTo(vm) {
			return "для ВМ действует отдельная политика восстановления кластера", nil
		}
	}

	// Некоторые разъемы не реализуют режим обслуживания хоста или ограждения. Запись этого
	// в качестве пропуска более полезна, чем позволить попытке потерпеть неудачу позже.
	if srv, err := r.store.GetServer(ctx, sit.ServerID); err == nil && !srv.Kind.SupportsHostManagement() {
		switch sit.Action {
		case model.ActionHostActivate, model.ActionHostFence:
			return fmt.Sprintf("управление узлами для %s этим драйвером не поддерживается", srv.Kind.Title()), nil
		}
	}

	// Человек, который просит о действии, уже сделал суждение о том, что существуют пределы
	// ставок, чтобы сделать от их имени.
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
			// Если объект пришлось восстанавливать трижды за час, перезапуск не решает
			// проблему. Дальше нужно вмешательство оператора.
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
		// Сброс прерывает работу гостя так же, как фенсинг прерывает работу хоста.
		// Автоматически его не выполняем.
		return false
	case model.ActionReconnect:
		return true
	default:
		return false
	}
}

func (r *Remediator) actionAllowed(sit Situation) bool {
	return r.allowed(sit.Action) || (sit.Force && sit.Action == model.ActionVMReset)
}

func (r *Remediator) execute(ctx context.Context, sit Situation) error {
	if srv, err := r.store.GetServer(ctx, sit.ServerID); err == nil {
		switch {
		case srv.Kind.UsesLibvirt():
			return r.executeLibvirt(ctx, sit)
		case srv.Kind.UsesProxmoxAPI():
			return r.executeProxmox(ctx, sit)
		case !srv.Kind.UsesOVirtAPI():
			return fmt.Errorf("действия для драйвера %q не поддерживаются", srv.Kind)
		}
	}

	client, err := r.pool.Get(ctx, sit.ServerID)
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	switch sit.Action {
	case model.ActionVMStart, model.ActionVMUnpause:
		// oVirt возобновляет приостановленную ВМ через тот же вызов, который загружает
		// остановленную машину, так что обе ситуации разделяют реализацию.
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
	case model.ActionVMReboot:
		return client.RebootVM(opCtx, sit.ObjectID)

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
