package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Умолчания политики согласования.
//
// Трое суток на сбор кворума — не круглое число ради красоты: за меньший срок
// не успевает вернуться человек, ушедший на выходные, а больший превращает
// заявку в забытую. Окно вето сутки — чтобы отсутствовавший успел зайти и
// отменить, но удаление не откладывалось на неделю.
const (
	defaultApprovalQuorum  = 2
	defaultApprovalTimeout = 72 * time.Hour
	defaultVetoWindow      = 24 * time.Hour
	// defaultApprovalGroup — группа, которая подразумевается, пока политика не
	// настроена.
	defaultApprovalGroup = "approvers"
)

// policyFor возвращает действующую политику для действия.
//
// Уровень по умолчанию берётся из каталога в коде, а не из базы: новое опасное
// действие обязано получить согласование сразу, а строка в базе появилась бы
// только после ручной правки — то есть действие какое-то время шло бы вовсе
// без проверки.
func (s *Server) policyFor(ctx context.Context, action model.GuardedAction) model.ApprovalPolicy {
	info, known := model.GuardedActionByKey(action)
	policy := model.ApprovalPolicy{
		Action:     action,
		Level:      model.LevelAudit,
		Quorum:     defaultApprovalQuorum,
		GroupName:  defaultApprovalGroup,
		Timeout:    defaultApprovalTimeout,
		VetoWindow: defaultVetoWindow,
	}
	if known {
		policy.Level = info.Level
	}

	stored, err := s.store.ListApprovalPolicies(ctx)
	if err != nil {
		// Отказ базы не должен ослаблять защиту: остаётся уровень из каталога,
		// то есть более строгий из доступных.
		s.log.Error().Err(err).Msg("не удалось прочитать политики согласования")
		return policy
	}
	for _, p := range stored {
		if p.Action != action {
			continue
		}
		policy.Level = p.Level
		if p.Quorum > 0 {
			policy.Quorum = p.Quorum
		}
		if p.GroupName != "" {
			policy.GroupName = p.GroupName
		}
		policy.FallbackGroupName = p.FallbackGroupName
		if p.Timeout > 0 {
			policy.Timeout = p.Timeout
		}
		if p.VetoWindow > 0 {
			policy.VetoWindow = p.VetoWindow
		}
		break
	}
	return policy
}

// guardedTarget описывает объект, над которым совершается действие.
//
// Нужен затем, что согласующий должен видеть, что именно подтверждает, а
// проверка при выполнении — убедиться, что подтверждали именно это.
type guardedTarget struct {
	ID      string
	Name    string
	Summary string
	// Payload — параметры, без которых действие не воспроизвести после
	// истечения окна отмены. Пусто там, где хватает ID.
	//
	// Секретам здесь не место: заявка живёт в базе и уходит в оповещения.
	Payload []byte
}

// targetFunc собирает описание объекта из запроса.
type targetFunc func(r *http.Request) guardedTarget

// Описания объектов для заявок.
//
// Имя читается из базы, а не берётся из адреса: согласующий должен видеть, что
// именно подтверждает. «Удалить 7f3c…» и «удалить хранилище "Ночные копии", в
// нём 412 копий» — разные вопросы, и на второй отвечают осмысленно.

// policyTarget — правка политики согласования.
func policyTarget(r *http.Request) guardedTarget {
	return guardedTarget{
		ID:      "approval-policy",
		Name:    "политика согласования",
		Summary: "изменение правил согласования опасных действий",
	}
}

func (s *Server) storageTarget(r *http.Request) guardedTarget {
	id := r.PathValue("id")
	what := guardedTarget{ID: id, Name: id, Summary: "удаление хранилища копий " + id}

	target, err := s.store.GetStorageTarget(r.Context(), id)
	if err != nil {
		return what
	}
	what.Name = target.Name
	what.Summary = "удаление хранилища копий «" + target.Name + "»"

	// Число копий важнее названия: оно и есть цена ошибки.
	if runs, err := s.store.CountRunsOnTarget(r.Context(), id); err == nil && runs > 0 {
		what.Summary += ", в нём " + plural(runs, "копия", "копии", "копий")
	}
	return what
}

func (s *Server) jobTarget(r *http.Request) guardedTarget {
	id := r.PathValue("id")
	what := guardedTarget{ID: id, Name: id, Summary: "удаление задания бэкапа " + id}

	if job, err := s.store.GetBackupJob(r.Context(), id); err == nil {
		what.Name = job.Name
		what.Summary = "удаление задания бэкапа «" + job.Name + "»: расписание перестанет " +
			"выполняться, машины останутся без новых копий"
	}
	return what
}

func (s *Server) serverTarget(r *http.Request) guardedTarget {
	id := r.PathValue("id")
	what := guardedTarget{ID: id, Name: id, Summary: "удаление подключения к движку " + id}

	if server, err := s.store.GetServer(r.Context(), id); err == nil {
		what.Name = server.Name
		what.Summary = "удаление подключения «" + server.Name + "»: вместе с ним теряются " +
			"задания и связь с уже сделанными копиями"
	}
	return what
}

// retentionTarget описывает применение политики хранения.
//
// Принимает разобранный запрос, а не *http.Request: параметры нужны заявке,
// чтобы выполниться самой по истечении окна отмены, а прочитать тело дважды
// нельзя. Секретов в них нет — только идентификаторы и сама политика.
func retentionTarget(req retentionRequest) guardedTarget {
	payload, err := json.Marshal(req)
	if err != nil {
		// Единственная причина — незакодируемое поле в структуре запроса,
		// которого там нет. Заявка при этом заводится без параметров и
		// потребует ручного повтора вместо автоматического выполнения.
		payload = nil
	}
	return guardedTarget{
		ID:   "retention",
		Name: "политика хранения",
		Summary: "применение политики хранения к ВМ " + req.VMID +
			" в хранилище " + req.StorageTargetID + ": устаревшие копии будут удалены пачкой",
		Payload: payload,
	}
}

// plural склоняет число с существительным.
func plural(n int, one, few, many string) string {
	word := many
	switch {
	case n%10 == 1 && n%100 != 11:
		word = one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 10 || n%100 >= 20):
		word = few
	}
	return strconv.Itoa(n) + " " + word
}

// guarded оборачивает обработчик опасного действия.
//
// Три пути. Уровень audit — выполняется сразу, как и раньше. Есть подтверждённая
// заявка — выполняется и помечается исполненной. Ничего нет — заводится заявка
// и возвращается 202: действие не выполнено, и это видно по коду ответа.
func (s *Server) guarded(action model.GuardedAction, target targetFunc, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.guardAction(w, r, action, target(r), next)
	}
}

// guardAction — то же самое, но вызывается изнутри обработчика, когда тело
// запроса уже прочитано.
//
// Нужно там, где охраняется не маршрут, а содержимое: `PUT /storages/{id}`
// меняет и название, и адрес, и требовать кворум за переименование значило бы
// сделать согласование помехой вместо защиты. Продолжение (next) при этом
// обязано не читать тело повторно — оно уже разобрано.
func (s *Server) guardAction(w http.ResponseWriter, r *http.Request,
	action model.GuardedAction, what guardedTarget, next http.HandlerFunc) {
	policy := s.policyFor(r.Context(), action)
	if policy.Level == model.LevelAudit {
		next(w, r)
		return
	}

	actor := "anonymous"
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}

	// Пока группа согласующих не заведена, согласовывать не с кем, и
	// требование подписи превратило бы систему в неработающую с первого дня:
	// завести группу тоже нельзя, не пройдя согласование.
	//
	// Действие выполняется, но в журнале это видно отдельной пометкой —
	// установка без настроенных согласующих не должна выглядеть защищённой.
	if !s.approversReady(r.Context(), policy) {
		s.audit(r, string(action)+".unguarded", model.ScopeServer, what.ID, true,
			"группа согласующих "+policy.GroupName+" не настроена")
		s.log.Warn().Str("действие", string(action)).Str("группа", policy.GroupName).
			Msg("опасное действие выполнено без согласования: группа согласующих не настроена")
		next(w, r)
		return
	}

	if id := strings.TrimSpace(r.URL.Query().Get("approval")); id != "" {
		s.executeApproved(w, r, action, what, id, actor, next)
		return
	}
	if strings.EqualFold(r.URL.Query().Get("break_glass"), "true") {
		s.executeBreakGlass(w, r, action, what, actor, next)
		return
	}

	s.openApprovalRequest(w, r, policy, action, what, actor)
}

// executeApproved выполняет действие по уже подтверждённой заявке.
func (s *Server) executeApproved(w http.ResponseWriter, r *http.Request, action model.GuardedAction,
	what guardedTarget, requestID, actor string, next http.HandlerFunc) {
	req, err := s.store.GetApprovalRequest(r.Context(), requestID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// Заявка привязана к действию и к объекту. Без этой проверки подтверждение
	// на удаление одного хранилища разрешало бы удалить любое другое — надо
	// лишь подставить чужой идентификатор в адрес.
	if req.Action != action || req.ObjectID != what.ID {
		s.writeError(w, r, badRequest(
			"заявка %s согласована для другого действия или объекта", requestID))
		return
	}

	switch req.State {
	case model.ApprovalApproved:
	case model.ApprovalScheduled:
		if req.ExecuteAfter != nil && time.Now().UTC().Before(*req.ExecuteAfter) {
			s.writeError(w, r, badRequest(
				"действие запланировано и может быть отменено до %s",
				req.ExecuteAfter.Format(time.RFC3339)))
			return
		}
	default:
		s.writeError(w, r, badRequest("заявка %s в состоянии %q и не даёт права выполнить действие",
			requestID, req.State))
		return
	}

	next(w, r)

	now := time.Now().UTC()
	if err := s.store.SetApprovalState(context.WithoutCancel(r.Context()), req.ID,
		model.ApprovalExecuted, &now, req.ExecuteAfter, req.Escalated, req.GroupName); err != nil {
		s.log.Error().Err(err).Str("заявка", req.ID).Msg("заявка не помечена исполненной")
	}
	s.audit(r, string(action)+".approved", model.ScopeServer, what.ID, true,
		"по заявке "+req.ID+", инициатор "+req.Requester+", исполнил "+actor)
}

// executeBreakGlass выполняет действие в обход согласования.
//
// Запретить аварийный доступ нельзя: согласующие бывают недоступны, а действие
// нужно сейчас. Смысл в том, что тихо им воспользоваться невозможно — событие
// уходит в отдельную таблицу, в журнал аудита особой пометкой и оповещением
// всем согласующим.
func (s *Server) executeBreakGlass(w http.ResponseWriter, r *http.Request, action model.GuardedAction,
	what guardedTarget, actor string, next http.HandlerFunc) {
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	// Требование внятного объяснения — не формальность: аварийный доступ без
	// него неотличим от злоупотребления им, а разбирать это будут месяцы спустя.
	if len([]rune(reason)) < 20 {
		s.writeError(w, r, badRequest(
			"для выполнения в обход согласования нужно обоснование не короче 20 символов: "+
				"оно попадёт в журнал и будет разослано согласующим"))
		return
	}

	policy := s.policyFor(r.Context(), action)
	notified := s.approverNames(r.Context(), policy)

	event := &model.BreakGlassEvent{
		Actor: actor, Action: action, ObjectID: what.ID, Reason: reason, Notified: notified,
	}
	if err := s.store.RecordBreakGlass(context.WithoutCancel(r.Context()), event); err != nil {
		// Незаписанный обход хуже несовершённого действия: смысл break-glass в
		// том, что след остаётся всегда.
		s.writeError(w, r, err)
		return
	}

	s.audit(r, string(action)+".break_glass", model.ScopeServer, what.ID, true,
		"в обход согласования: "+reason)

	// Критическая важность намеренно: обход согласования — это либо настоящая
	// авария, либо злоупотребление, и оба случая требуют, чтобы о них узнали
	// сразу, а не при следующем разборе журнала.
	if err := s.store.RaiseAlert(context.WithoutCancel(r.Context()), &model.Alert{
		Scope: model.ScopeServer, ObjectID: event.ID, ObjectName: what.Name,
		Kind: model.AlertBreakGlassUsed, Severity: model.SeverityCritical,
		Message: "Действие выполнено в обход согласования: " + what.Summary,
		Details: "Исполнил: " + actor + ". Обоснование: " + reason,
	}); err != nil {
		s.log.Error().Err(err).Msg("оповещение об обходе согласования не поднято")
	}
	s.log.Warn().Str("кто", actor).Str("действие", string(action)).
		Str("объект", what.Name).Str("обоснование", reason).
		Msg("действие выполнено в обход согласования")

	next(w, r)
}

// approvalLink — адрес заявки в интерфейсе.
//
// Ссылка ведёт на страницу входа, а не выполняет действие сама. Одноразовый
// токен в адресе решал бы задачу «отменить с телефона, не входя», но адрес
// проходит через почтовый сервер, прокси и историю браузера и оседает в
// журналах каждого из них. Право вето, лежащее в чьём-то access.log, — это не
// право вето.
//
// Поэтому ссылка экономит только навигацию: человек входит под собой и
// попадает сразу на нужную заявку, а не ищет её среди прочих.
func (s *Server) approvalLink(id string) string {
	base := strings.TrimRight(s.cfg.Server.ExternalURL, "/")
	if base == "" {
		return ""
	}
	return base + "/settings?tab=approvals&approval=" + url.QueryEscape(id)
}

// openApprovalRequest заводит заявку и отвечает 202.
func (s *Server) openApprovalRequest(w http.ResponseWriter, r *http.Request,
	policy model.ApprovalPolicy, action model.GuardedAction, what guardedTarget, actor string) {
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if len([]rune(reason)) < 10 {
		s.writeError(w, r, badRequest(
			"нужно обоснование не короче 10 символов: заявка без объяснения "+
				"подтверждается не глядя"))
		return
	}

	now := time.Now().UTC()
	req := &model.ApprovalRequest{
		Action: action, ObjectID: what.ID, ObjectName: what.Name, Summary: what.Summary,
		Requester: actor, Reason: reason,
		State:     model.ApprovalPending,
		Level:     policy.Level,
		Quorum:    policy.Quorum,
		GroupName: policy.GroupName,
		CreatedAt: now,
		ExpiresAt: now.Add(policy.Timeout),
		Payload:   what.Payload,
	}
	// На уровне вето кворум равен одному: смысл там не в числе подтверждений, а
	// в окне, за которое действие можно отменить.
	if policy.Level == model.LevelVeto {
		req.Quorum = 1
	}

	if err := s.store.CreateApprovalRequest(r.Context(), req); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, string(action)+".requested", model.ScopeServer, what.ID, true,
		"заявка "+req.ID+": "+reason)

	// Оповещение поднимается через обычный механизм: у него уже есть очередь,
	// повторы и каналы наружу. Заявка, о которой согласующие узнают только зайдя
	// в интерфейс, доживёт до истечения срока и закроется сама — и согласование
	// превратится в способ не дать сделать работу вместо защиты.
	if err := s.store.RaiseAlert(context.WithoutCancel(r.Context()), &model.Alert{
		Scope: model.ScopeServer, ObjectID: req.ID, ObjectName: what.Name,
		Kind: model.AlertApprovalPending, Severity: model.SeverityWarning,
		Message: "Требуется подтверждение: " + what.Summary,
		Details: "Инициатор: " + actor + ". Причина: " + reason +
			". Группа согласующих: " + policy.GroupName +
			". Срок: до " + req.ExpiresAt.Format(time.RFC3339) +
			". Открыть: " + s.approvalLink(req.ID),
	}); err != nil {
		s.log.Error().Err(err).Str("заявка", req.ID).
			Msg("оповещение о заявке не поднято: согласующие могут о ней не узнать")
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":         "approval_required",
		"request":        req,
		"message":        approvalMessage(policy),
		"approver_group": policy.GroupName,
	})
}

// approvalMessage объясняет инициатору, чего теперь ждать.
func approvalMessage(policy model.ApprovalPolicy) string {
	if policy.Level == model.LevelVeto {
		return "Действие не выполнено. После подтверждения оно будет запланировано и " +
			"выполнится через " + policy.VetoWindow.String() + ", если никто не отменит."
	}
	return "Действие не выполнено. Нужны подтверждения от других участников группы: " +
		policy.GroupName + "."
}

// approversReady сообщает, есть ли кому подтверждать.
//
// Группы из одного человека недостаточно: кворум по умолчанию два, и заявка в
// такой группе не собралась бы никогда. Лучше честно выполнить действие с
// пометкой, чем оставить администратора наедине с заявкой, которую некому
// подтвердить.
func (s *Server) approversReady(ctx context.Context, policy model.ApprovalPolicy) bool {
	group, err := s.store.ApprovalGroupByName(ctx, policy.GroupName)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error().Err(err).Str("группа", policy.GroupName).
				Msg("группа согласующих не прочитана")
		}
		return false
	}
	need := policy.Quorum
	if policy.Level == model.LevelVeto {
		need = 1
	}
	// Инициатор за себя не голосует, поэтому подтверждающих нужно на одного
	// больше, чем размер кворума.
	return len(group.Members) > need
}

// approverNames перечисляет тех, кого касается заявка.
func (s *Server) approverNames(ctx context.Context, policy model.ApprovalPolicy) []string {
	names := []string{}
	for _, name := range []string{policy.GroupName, policy.FallbackGroupName} {
		if name == "" {
			continue
		}
		group, err := s.store.ApprovalGroupByName(ctx, name)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Warn().Err(err).Str("группа", name).Msg("группа согласующих не прочитана")
			}
			continue
		}
		names = append(names, group.Members...)
	}
	return names
}

// closeApprovalAlert снимает оповещение о заявке.
//
// Вызывается при любом исходе, включая истечение срока: висящее оповещение о
// заявке, которую уже отклонили, — это шум, а к шуму привыкают и перестают
// читать вместе с ним всё остальное.
func (s *Server) closeApprovalAlert(ctx context.Context, requestID string) {
	if err := s.store.ResolveAlert(ctx, "", model.ScopeServer, requestID,
		model.AlertApprovalPending); err != nil {
		s.log.Warn().Err(err).Str("заявка", requestID).
			Msg("оповещение о заявке не закрыто")
	}
}

// castVote регистрирует решение согласующего и двигает заявку дальше.
func (s *Server) castVote(ctx context.Context, req *model.ApprovalRequest, caller string,
	approve bool, comment string, creds delegationCredentials) (*model.ApprovalRequest, error) {
	if req.State.Final() {
		return nil, badRequest("заявка уже отыграна: %s", req.State)
	}

	// voter — чей это голос, castBy — кто нажал кнопку. При обычном
	// голосовании это один человек, при делегировании — разные, и кворум
	// считается по первому.
	voter, castBy := caller, ""
	var delegation *model.ApprovalDelegation
	if creds.present() {
		d, err := s.resolveDelegation(ctx, creds, caller, req.GroupName)
		if err != nil {
			return nil, err
		}
		// Делегат, оказавшийся инициатором, — это способ подтвердить свою же
		// заявку чужим правом. Проверка ниже смотрит на voter и такое
		// пропустила бы: там уже стоит имя делегирующего.
		if caller == req.Requester {
			return nil, badRequest("инициатор не может подтверждать собственную заявку, " +
				"в том числе по делегированию")
		}
		delegation, voter, castBy = d, d.Delegator, caller
	}

	// Инициатор не голосует за себя. Иначе кворум из двух в группе из двух
	// человек вырождается в одного — того самого, чья учётная запись могла
	// утечь.
	if voter == req.Requester {
		return nil, badRequest("инициатор не может подтверждать собственную заявку")
	}

	group, err := s.store.ApprovalGroupByName(ctx, req.GroupName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, badRequest("группа согласующих %q не заведена", req.GroupName)
		}
		return nil, err
	}
	if !group.Has(voter) {
		if castBy != "" {
			// Право голоса передал тот, у кого его нет. Обычно это значит, что
			// делегирующего вывели из группы уже после выдачи токена.
			return nil, badRequest(
				"%s не входит в группу согласующих %q — переданное право недействительно",
				voter, req.GroupName)
		}
		return nil, badRequest("вы не входите в группу согласующих %q", req.GroupName)
	}

	vote := model.ApprovalVote{
		RequestID: req.ID, Voter: voter, CastBy: castBy,
		Approve: approve, Comment: comment,
	}
	if err := s.store.AddApprovalVote(ctx, vote); err != nil {
		return nil, err
	}
	if delegation != nil {
		// Счётчик использований — то, по чему владелец замечает, что
		// делегированием пользуются не так, как он ожидал.
		if err := s.store.TouchApprovalDelegation(context.WithoutCancel(ctx),
			delegation.ID, time.Now().UTC()); err != nil {
			s.log.Warn().Err(err).Str("делегирование", delegation.ID).
				Msg("не удалось отметить использование делегирования")
		}
	}
	req.Votes = append(req.Votes, vote)

	now := time.Now().UTC()
	switch {
	case !approve:
		// Одного «против» достаточно. Согласование существует, чтобы кто-то мог
		// остановить действие, а не чтобы набрать большинство.
		if err := s.store.SetApprovalState(ctx, req.ID, model.ApprovalRejected,
			&now, nil, req.Escalated, req.GroupName); err != nil {
			return nil, err
		}
		req.State = model.ApprovalRejected

	case req.Approvals() >= req.Quorum && req.Level == model.LevelVeto:
		executeAfter := now.Add(s.policyFor(ctx, req.Action).VetoWindow)
		if err := s.store.SetApprovalState(ctx, req.ID, model.ApprovalScheduled,
			&now, &executeAfter, req.Escalated, req.GroupName); err != nil {
			return nil, err
		}
		req.State, req.ExecuteAfter = model.ApprovalScheduled, &executeAfter

	case req.Approvals() >= req.Quorum:
		if err := s.store.SetApprovalState(ctx, req.ID, model.ApprovalApproved,
			&now, nil, req.Escalated, req.GroupName); err != nil {
			return nil, err
		}
		req.State = model.ApprovalApproved
	}
	req.DecidedAt = &now

	// Подтверждений больше не ждут — ни когда собран кворум, ни когда заявку
	// отклонили.
	if req.State != model.ApprovalPending && req.State != model.ApprovalEscalated {
		s.closeApprovalAlert(ctx, req.ID)
	}
	return req, nil
}

// RunApprovalSweeper следит за сроками заявок, пока жив контекст.
//
// Минута — достаточная точность: сроки здесь измеряются часами, а более частый
// перебор нагружал бы базу ради ничего. Первый проход сразу после запуска, а не
// через минуту: служба могла простоять выключенной дольше любого срока.
func (s *Server) RunApprovalSweeper(ctx context.Context) {
	s.sweepApprovals(ctx)
	s.sweepDueApprovals(ctx)
	s.sweepDelegations(ctx)

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepApprovals(ctx)
			s.sweepDueApprovals(ctx)
			s.sweepDelegations(ctx)
		}
	}
}

// delegationRetention — сколько истёкшее делегирование ещё видно в списке.
//
// Не удаляется сразу вместе с истечением срока: «почему у меня перестало
// работать» — вопрос, который задают в тот же день, и ответ «делегирование
// истекло вчера» полезнее, чем пустой список.
const delegationRetention = 7 * 24 * time.Hour

// sweepDelegations убирает давно истёкшие делегирования.
//
// Само по себе истёкшее делегирование безвредно — оно не проходит проверку. Но
// список «что я передал» через год превращается в свалку, в которой
// действующее делегирование не разглядеть.
func (s *Server) sweepDelegations(ctx context.Context) {
	n, err := s.store.PurgeExpiredDelegations(ctx, time.Now().UTC().Add(-delegationRetention))
	if err != nil {
		s.log.Error().Err(err).Msg("не удалось убрать истёкшие делегирования")
		return
	}
	if n > 0 {
		s.log.Info().Int64("удалено", n).Msg("убраны истёкшие делегирования права голоса")
	}
}

// sweepApprovals переводит просроченные заявки на резервную группу или закрывает.
//
// Истечение срока НЕ выполняет действие. Иначе таймаут стал бы способом
// провести что угодно, дождавшись отпуска согласующих, — и вся конструкция
// защищала бы ровно до первого длинного выходного.
func (s *Server) sweepApprovals(ctx context.Context) {
	expired, err := s.store.ExpiredApprovalRequests(ctx, time.Now().UTC())
	if err != nil {
		s.log.Error().Err(err).Msg("не удалось перебрать просроченные заявки")
		return
	}

	for _, req := range expired {
		policy := s.policyFor(ctx, req.Action)
		now := time.Now().UTC()

		// Первый срок вышел, а резервная группа есть и ещё не привлекалась —
		// заявка переходит к ней, а не закрывается.
		if !req.Escalated && policy.FallbackGroupName != "" {
			if err := s.store.SetApprovalState(ctx, req.ID, model.ApprovalEscalated,
				nil, nil, true, policy.FallbackGroupName); err != nil {
				s.log.Error().Err(err).Str("заявка", req.ID).Msg("эскалация не выполнена")
				continue
			}
			s.log.Warn().Str("заявка", req.ID).Str("группа", policy.FallbackGroupName).
				Msg("заявка передана резервной группе согласующих")
			continue
		}

		if err := s.store.SetApprovalState(ctx, req.ID, model.ApprovalExpired,
			&now, nil, req.Escalated, req.GroupName); err != nil {
			s.log.Error().Err(err).Str("заявка", req.ID).Msg("заявка не закрыта по сроку")
			continue
		}
		s.closeApprovalAlert(ctx, req.ID)
		s.log.Warn().Str("заявка", req.ID).Str("действие", string(req.Action)).
			Msg("срок согласования вышел, действие не выполнено")
	}
}
