package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/monitor"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	filter := store.AlertFilter{
		ServerID: r.URL.Query().Get("server_id"),
		Scope:    model.Scope(r.URL.Query().Get("scope")),
		ObjectID: r.URL.Query().Get("object_id"),
		Severity: model.Severity(r.URL.Query().Get("severity")),
		// Адресат: backup, infrastructure или service. Пусто — показываются все,
		// как было до разделения.
		Audience: model.AlertAudience(r.URL.Query().Get("audience")),
		Limit:    queryInt(r, "limit", 200),
		Accepted: queryBool(r, "accepted"),
	}
	// По умолчанию список показывает, что еще требует внимания; Решенные предупреждения
	// are history and only clutter the working view. Раздел принятых — журнал:
	// закрытые оповещения с пояснениями остаются в нём.
	if state := r.URL.Query().Get("state"); state != "" {
		filter.States = []model.AlertState{model.AlertState(state)}
	} else if !filter.Accepted && !queryBool(r, "include_resolved") {
		filter.States = []model.AlertState{model.AlertFiring, model.AlertAcked}
	}

	alerts, err := s.store.ListAlerts(r.Context(), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, alerts)
}

func (s *Server) handleAckAlert(w http.ResponseWriter, r *http.Request) {
	// Тело необязательно: прежние клиенты принимают оповещение пустым запросом.
	var req struct {
		Comment string `json:"comment"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	req.Comment = strings.TrimSpace(req.Comment)
	if utf8.RuneCountInString(req.Comment) > model.AlertCommentMaxLength {
		s.writeError(w, r, badRequest("пояснение не должно превышать %d символов", model.AlertCommentMaxLength))
		return
	}
	id := r.PathValue("id")
	actor := runtimeActor(r)

	if err := s.store.AckAlert(r.Context(), id, actor, req.Comment); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "alert.ack", model.ScopeServer, id, true, "")
	if s.bus != nil {
		s.bus.Publish(events.Event{Kind: events.KindAlert, ObjectID: id, Message: "alert acknowledged"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "acked"})
}

func (s *Server) handleListAlertComments(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListAlertComments(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleAddAlertComment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" || utf8.RuneCountInString(req.Message) > model.AlertCommentMaxLength {
		s.writeError(w, r, badRequest("пояснение должно содержать от 1 до %d символов", model.AlertCommentMaxLength))
		return
	}
	item, err := s.store.AddAlertComment(r.Context(), r.PathValue("id"), runtimeActor(r), req.Message)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "alert.comment", model.ScopeServer, item.AlertID, true, "добавлено пояснение")
	if s.bus != nil {
		s.bus.Publish(events.Event{Kind: events.KindAlert, ObjectID: item.AlertID, Message: "alert comment added"})
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) handleListRemediations(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListRemediations(r.Context(), r.URL.Query().Get("server_id"),
		queryInt(r, "limit", 200))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

// modeRequest переключает автовосстановление между проверочным и живым режимом.
type modeRequest struct {
	DryRun bool   `json:"dry_run"`
	Note   string `json:"note"`
	// Confirm обязателен для выхода из режима проверки: после него автоматика
	// начнёт выполнять действия на боевых машинах.
	Confirm bool `json:"confirm"`
}

// modeResponse описывается режим в силе и история, стоящая за ним.
type modeResponse struct {
	DryRun  bool                       `json:"dry_run"`
	Enabled bool                       `json:"enabled"`
	Current *model.RemediationPeriod   `json:"current"`
	History []*model.RemediationPeriod `json:"history"`
	// Observed — что накопилось в текущем периоде проверки: именно это и
	// попадёт в архив при выключении.
	Observed *model.RemediationDigest `json:"observed,omitempty"`
}

func (s *Server) handleGetRemediationMode(w http.ResponseWriter, r *http.Request) {
	history, err := s.store.ListRemediationPeriods(r.Context(), queryInt(r, "limit", 25))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	resp := modeResponse{
		DryRun:  s.remediator.DryRun(),
		Enabled: s.cfg.Monitor.Remediation.Enabled,
		History: history,
	}
	for _, p := range history {
		if p.Open() {
			resp.Current = p
			break
		}
	}
	// Текущий счётчик показывает оператору результаты наблюдений до закрытия периода и
	// записи архива.
	if resp.Current != nil {
		if digest, err := s.observedDigest(r.Context(), resp.Current); err == nil {
			resp.Observed = digest
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSetRemediationMode(w http.ResponseWriter, r *http.Request) {
	var req modeRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	// Выход из режима проверки — момент, когда автоматизация начинает касаться
	// производства.
	if !req.DryRun && !req.Confirm {
		s.writeError(w, r, badRequest(
			"выход из режима проверки означает, что действия начнут выполняться на боевых машинах, "+
				"и требует подтверждения (confirm=true)"))
		return
	}

	who := "api"
	if p := principalFrom(r.Context()); p != nil {
		who = p.Username
	}

	closed, opened, err := s.remediator.SwitchMode(context.WithoutCancel(r.Context()),
		req.DryRun, who, req.Note)
	if err != nil {
		s.audit(r, "remediation.mode", model.ScopeServer, "", false, err.Error())
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	s.audit(r, "remediation.mode", model.ScopeServer, opened.ID, true,
		"режим: "+modeLabel(req.DryRun))

	writeJSON(w, http.StatusOK, map[string]any{
		"dry_run": req.DryRun,
		"current": opened,
		"closed":  closed,
	})
}

// observedDigest подсчитано, что до сих пор наблюдалось в открытом периоде.
func (s *Server) observedDigest(ctx context.Context, period *model.RemediationPeriod) (*model.RemediationDigest, error) {
	records, err := s.store.RemediationsBetween(ctx, period.StartedAt, nil)
	if err != nil {
		return nil, err
	}
	digest := &model.RemediationDigest{ByAction: map[string]int{}}
	objects := map[string]bool{}
	for _, rec := range records {
		digest.Total++
		digest.ByAction[rec.Action.Title()]++
		objects[rec.ServerID+"/"+rec.ObjectID] = true
		switch rec.Status {
		case model.RemDryRun:
			digest.Suppressed++
		case model.RemSkipped:
			digest.Skipped++
		case model.RemSucceeded:
			digest.Succeeded++
		case model.RemFailed:
			digest.Failed++
		}
	}
	digest.Objects = len(objects)
	return digest, nil
}

// handleGetRemediationArchive возвращает решения, собранные в течение закрытого периода
// проверки.
func (s *Server) handleGetRemediationArchive(w http.ResponseWriter, r *http.Request) {
	period, err := s.store.GetRemediationPeriod(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if period.ArchivePath == "" {
		s.writeError(w, r, badRequest(
			"у этого периода нет архива: архив собирается только при выходе из режима проверки"))
		return
	}

	archive, err := monitor.ReadArchive(period.ArchivePath)
	if err != nil {
		s.writeError(w, r, fmt.Errorf("архив %s недоступен: %w", period.ArchivePath, err))
		return
	}
	writeJSON(w, http.StatusOK, archive)
}

func modeLabel(dryRun bool) string {
	if dryRun {
		return "проверка"
	}
	return "боевой"
}

// manualRemediationRequest требует явного корректирующего действия.
type manualRemediationRequest struct {
	ServerID string `json:"server_id"`
	Scope    string `json:"scope"`
	ObjectID string `json:"object_id"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
	// Confirm обязателен для разрушительных действий.
	Confirm bool `json:"confirm"`
}

// handleManualRemediation позволяет оператору выполнить действие монитора без задержки и
// предела попыток. Эти ограничения сдерживают повторные автоматические действия.
func (s *Server) handleManualRemediation(w http.ResponseWriter, r *http.Request) {
	var req manualRemediationRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.ServerID == "" || req.ObjectID == "" || req.Action == "" {
		s.writeError(w, r, badRequest("нужны server_id, object_id и action"))
		return
	}

	action := model.RemediationAction(req.Action)
	// Ручное восстановление ведёт к тем же операциям, что и управление ВМ, но
	// маршрут закрыт правом на оповещения. Без этой проверки право на фенсинг
	// обходилось бы запросом сюда, и разделение прав ничего бы не значило.
	if action.Disruptive() && !s.allowedDisruptive(r) {
		s.writeError(w, r, forbiddenDisruptive(action.Title()))
		return
	}
	if action.Disruptive() && !req.Confirm {
		s.writeError(w, r, badRequest(
			"действие «%s» прерывает работу и требует подтверждения (confirm=true)", action.Title()))
		return
	}

	scope := model.Scope(req.Scope)
	if scope == "" {
		scope = model.ScopeVM
	}
	if !action.ValidForScope(scope) {
		s.writeError(w, r, badRequest("действие %q неприменимо к объекту типа %q", action, scope))
		return
	}

	objectName := req.ObjectID
	switch scope {
	case model.ScopeServer:
		if server, err := s.store.GetServer(r.Context(), req.ServerID); err == nil {
			objectName = server.Name
		}
	case model.ScopeVM:
		if vm, err := s.store.GetVM(r.Context(), req.ServerID, req.ObjectID); err == nil {
			objectName = vm.Name
		}
	case model.ScopeHost:
		if hosts, err := s.store.ListHosts(r.Context(), req.ServerID); err == nil {
			for _, h := range hosts {
				if h.ID == req.ObjectID {
					objectName = h.Name
					break
				}
			}
		}
	}

	actor := "api"
	if p := principalFrom(r.Context()); p != nil {
		actor = "user:" + p.Username
	}
	reason := req.Reason
	if reason == "" {
		reason = "запрошено оператором"
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()

	record, err := s.remediator.Consider(ctx, monitor.Situation{
		ServerID:    req.ServerID,
		Scope:       scope,
		ObjectID:    req.ObjectID,
		ObjectName:  objectName,
		Action:      action,
		Reason:      reason,
		TriggeredBy: actor,
		Force:       true,
	})
	s.audit(r, "remediation."+req.Action, scope, req.ObjectID, err == nil, reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) handleHealthSamples(w http.ResponseWriter, r *http.Request) {
	serverID := r.URL.Query().Get("server_id")
	if serverID == "" {
		s.writeError(w, r, badRequest("нужен server_id"))
		return
	}
	scope := model.Scope(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = model.ScopeServer
	}
	hours := queryInt(r, "hours", 24)
	since := time.Now().Add(-time.Duration(hours) * time.Hour)

	samples, err := s.store.ListHealthSamples(r.Context(), serverID, scope,
		r.URL.Query().Get("object_id"), since, queryInt(r, "limit", 2000))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, samples)
}

// handleEvents потоки изменений состояния в браузере через события, отправленные сервером.
//
// SSE вместо websockets: трафик является однонаправленным, он выживает прокси, которые
// mangle обновляет, и браузер повторно подключается самостоятельно.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, r, fmt.Errorf("потоковая передача не поддерживается этим соединением"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nginx буферизирует ответы по умолчанию, который будет удерживать каждое событие, пока
	// соединение не закроется.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	stream, cancel := s.bus.Subscribe()
	defer cancel()

	fmt.Fprintf(w, "retry: 5000\n\n")
	flusher.Flush()

	// Сердцебиение удерживает посредников от простоя соединения и позволяет клиенту
	// заметить мертвую ссылку.
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case event, ok := <-stream:
			if !ok {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Kind, payload)
			flusher.Flush()
		}
	}
}

// ioOverview — это то, что экран мониторинга диска загружается за один вызов.
type ioOverview struct {
	Disks  []*model.DiskSample  `json:"disks"`
	Mounts []*model.MountSample `json:"mounts"`
	// Paths — текущее состояние каждого пути до хранилища.
	Paths []*model.MountSample `json:"paths"`
}

func (s *Server) handleDiskSamples(w http.ResponseWriter, r *http.Request) {
	serverID := r.URL.Query().Get("server_id")
	if serverID == "" {
		s.writeError(w, r, badRequest("нужен server_id"))
		return
	}
	hours := queryInt(r, "hours", 6)
	items, err := s.store.ListDiskSamples(r.Context(), store.DiskSampleFilter{
		ServerID: serverID,
		VMID:     r.URL.Query().Get("vm_id"),
		Disk:     r.URL.Query().Get("disk"),
		Since:    time.Now().Add(-time.Duration(hours) * time.Hour),
		Limit:    queryInt(r, "limit", 5000),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleMountSamples(w http.ResponseWriter, r *http.Request) {
	serverID := r.URL.Query().Get("server_id")
	if serverID == "" {
		s.writeError(w, r, badRequest("нужен server_id"))
		return
	}
	hours := queryInt(r, "hours", 6)
	items, err := s.store.ListMountSamples(r.Context(), serverID,
		r.URL.Query().Get("target"),
		time.Now().Add(-time.Duration(hours)*time.Hour),
		queryInt(r, "limit", 5000))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

// handleStoragePaths перечисляет текущее состояние каждого пути хранения узла.
func (s *Server) handleStoragePaths(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.LatestMountSamples(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}
