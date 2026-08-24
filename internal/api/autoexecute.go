package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Автоматическое выполнение по истечении окна вето.
//
// Уровень veto устроен так: одно подтверждение, затем окно, в течение которого
// любой из группы может отменить. Пока действие после этого окна выполнял
// человек повторным запросом, окно вето работало наполовину: если про заявку
// забывали, ретенция не применялась и копии копились, а согласующие были
// уверены, что всё прошло.
//
// Выполняется не воспроизведением HTTP-запроса, а вызовом того же кода, что и
// обработчик. Хранить тело запроса ради повтора значило бы складывать в базу и
// секреты из него; подделывать запрос с чужой сессией — заводить внутри службы
// способ действовать от чужого имени. Здесь ни того, ни другого нет: действие
// выполняется от лица службы, а в журнале аудита стоит ссылка на заявку.
//
// Действия, которым для повтора нужен секрет, автоматически не выполняются —
// их доводит человек, и заявка честно ждёт его.

// retryAfterFailure — через сколько повторить неудавшееся выполнение.
//
// Причина неудачи обычно внешняя: хранилище не отвечает, движок перезапускают.
// Повторять каждую минуту значит завалить журнал одинаковыми ошибками, бросить
// после первой — потерять действие, которое согласовали.
const retryAfterFailure = 15 * time.Minute

// approvalExecutor выполняет согласованное действие без участия человека.
type approvalExecutor func(ctx context.Context, req model.ApprovalRequest) error

// executors — что служба умеет доводить сама.
//
// Здесь только уровень veto. Кворумные действия в этот список не входят
// намеренно: там подтверждение собирает человек, он же и выполняет — а
// автоматическое выполнение удаления хранилища означало бы, что достаточно
// собрать подписи и уйти.
func (s *Server) executors() map[model.GuardedAction]approvalExecutor {
	return map[model.GuardedAction]approvalExecutor{
		model.GuardJobDelete:      s.executeJobDelete,
		model.GuardServerDelete:   s.executeServerDelete,
		model.GuardRetentionApply: s.executeRetentionApply,
	}
}

func (s *Server) executeJobDelete(ctx context.Context, req model.ApprovalRequest) error {
	if err := s.store.DeleteBackupJob(ctx, req.ObjectID); err != nil {
		return err
	}
	// Проверка на nil не формальность: этот код выполняется в фоне, а не в
	// обработчике запроса, и паника здесь роняет службу целиком — вместе с
	// идущими бэкапами. Обработчик рядом падал бы только своим запросом.
	if s.scheduler != nil {
		if err := s.scheduler.Reload(ctx); err != nil {
			// Задание удалено, а расписание не перечитано: следующая
			// перезагрузка службы это исправит, и объявлять действие
			// неудавшимся нельзя — оно уже произошло.
			s.log.Warn().Err(err).Msg("не удалось перечитать расписание после удаления задания")
		}
	}
	return nil
}

func (s *Server) executeServerDelete(ctx context.Context, req model.ApprovalRequest) error {
	if err := s.store.DeleteServer(ctx, req.ObjectID); err != nil {
		return err
	}
	if s.pool != nil {
		s.pool.Invalidate(req.ObjectID)
	}
	return nil
}

func (s *Server) executeRetentionApply(ctx context.Context, req model.ApprovalRequest) error {
	if len(req.Payload) == 0 {
		return errors.New("в заявке нет параметров: повторите запрос вручную со ссылкой на неё")
	}
	var payload retentionRequest
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return fmt.Errorf("параметры заявки не разбираются: %w", err)
	}
	if err := payload.validate(); err != nil {
		return err
	}
	if s.engine == nil {
		return errors.New("движок выполнения недоступен")
	}
	_, err := s.applyRetention(ctx, payload)
	return err
}

// sweepDueApprovals доводит заявки, у которых вышло окно отмены.
//
// Вызывается из того же обхода, что и проверка сроков: минуты точности
// достаточно, когда окно измеряется часами.
func (s *Server) sweepDueApprovals(ctx context.Context) {
	due, err := s.store.DueApprovalRequests(ctx, time.Now().UTC())
	if err != nil {
		s.log.Error().Err(err).Msg("не удалось перебрать заявки, готовые к выполнению")
		return
	}

	executors := s.executors()
	for _, req := range due {
		exec, ok := executors[req.Action]
		if !ok {
			// Исполнителя нет — заявка ждёт человека. Это не ошибка и не повод
			// шуметь в журнале каждую минуту: состояние видно в списке заявок.
			continue
		}
		s.executeDueApproval(ctx, req, exec)
	}
}

// executeDueApproval выполняет одну заявку и разбирается с исходом.
func (s *Server) executeDueApproval(ctx context.Context, req model.ApprovalRequest,
	exec approvalExecutor) {

	now := time.Now().UTC()
	err := exec(ctx, req)

	// Объекта уже нет — значит действие, ради которого заводилась заявка, кто-то
	// довёл руками. Это исход, а не отказ: заявка закрывается выполненной.
	if errors.Is(err, store.ErrNotFound) {
		s.finishDueApproval(ctx, req, now, "объект уже отсутствует")
		return
	}
	if err != nil {
		s.failDueApproval(ctx, req, err, now)
		return
	}
	s.finishDueApproval(ctx, req, now, "")
}

func (s *Server) finishDueApproval(ctx context.Context, req model.ApprovalRequest,
	now time.Time, note string) {

	if err := s.store.SetApprovalState(ctx, req.ID, model.ApprovalExecuted,
		&now, nil, req.Escalated, req.GroupName); err != nil {
		s.log.Error().Err(err).Str("заявка", req.ID).
			Msg("действие выполнено, но состояние заявки не записано")
		return
	}
	s.closeApprovalAlert(ctx, req.ID)

	detail := "по заявке " + req.ID + ", окно отмены вышло"
	if note != "" {
		detail += "; " + note
	}
	// Актор — служба, а не человек: заявку подтвердил один, выполнило
	// автоматическое доведение, и приписывать действие подтвердившему значит
	// путать разбор инцидента.
	s.auditSystem(ctx, string(req.Action), model.ScopeServer, req.ObjectID, true, detail)
	s.log.Info().Str("заявка", req.ID).Str("действие", string(req.Action)).
		Msg("согласованное действие выполнено по истечении окна отмены")
}

func (s *Server) failDueApproval(ctx context.Context, req model.ApprovalRequest,
	cause error, now time.Time) {

	msg := cause.Error()
	if storeErr := s.store.SetApprovalError(ctx, req.ID, msg,
		now.Add(retryAfterFailure)); storeErr != nil {
		s.log.Error().Err(storeErr).Str("заявка", req.ID).
			Msg("не удалось записать причину неудачи")
	}
	s.auditSystem(ctx, string(req.Action)+".failed", model.ScopeServer, req.ObjectID, false,
		"по заявке "+req.ID+": "+msg)

	// Оповещение поднимается один раз — при первой неудаче. Повторы каждые
	// пятнадцать минут превратили бы ленту в поток одинаковых сообщений, среди
	// которых теряется всё остальное.
	if req.Error == "" {
		if alertErr := s.store.RaiseAlert(ctx, &model.Alert{
			Scope: model.ScopeServer, ObjectID: req.ID, ObjectName: req.ObjectName,
			Kind: model.AlertApprovalFailed, Severity: model.SeverityWarning,
			Message: "согласованное действие не выполнилось: " + req.Summary,
			Details: msg + "; повтор через " + retryAfterFailure.String(),
		}); alertErr != nil {
			s.log.Error().Err(alertErr).Str("заявка", req.ID).
				Msg("оповещение о неудачном выполнении не поднято")
		}
	}
	s.log.Warn().Err(cause).Str("заявка", req.ID).Str("действие", string(req.Action)).
		Msg("согласованное действие не выполнилось, будет повторено")
}
