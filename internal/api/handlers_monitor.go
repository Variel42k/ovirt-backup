package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

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
		Limit:    queryInt(r, "limit", 200),
	}
	// By default the list shows what still needs attention; resolved alerts
	// are history and only clutter the working view.
	if state := r.URL.Query().Get("state"); state != "" {
		filter.States = []model.AlertState{model.AlertState(state)}
	} else if !queryBool(r, "include_resolved") {
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
	id := r.PathValue("id")
	actor := "api"
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}

	if err := s.store.AckAlert(r.Context(), id, actor); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "alert.ack", model.ScopeServer, id, true, "")
	if s.bus != nil {
		s.bus.Publish(events.Event{Kind: events.KindAlert, ObjectID: id, Message: "alert acknowledged"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "acked"})
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

// modeRequest switches auto-remediation between check and live mode.
type modeRequest struct {
	DryRun bool   `json:"dry_run"`
	Note   string `json:"note"`
	// Confirm обязателен для выхода из режима проверки: после него автоматика
	// начнёт выполнять действия на боевых машинах.
	Confirm bool `json:"confirm"`
}

// modeResponse describes the mode in force and the history behind it.
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
	// Showing the running tally is what makes the mode usable: an operator
	// deciding whether to go live wants to know what has been observed so far,
	// not only after the archive is written.
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

	// Leaving check mode is the moment the automation starts touching production.
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

// observedDigest counts what the open period has seen so far.
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

// handleGetRemediationArchive returns the decisions collected during a closed
// check period.
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

// manualRemediationRequest asks for a corrective action explicitly.
type manualRemediationRequest struct {
	ServerID string `json:"server_id"`
	Scope    string `json:"scope"`
	ObjectID string `json:"object_id"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
	// Confirm обязателен для разрушительных действий.
	Confirm bool `json:"confirm"`
}

// handleManualRemediation lets an operator run the same corrective action the
// monitor would, bypassing the cooldown and attempt limits — those exist to
// stop a robot from looping, not to stop a person who has looked at the box.
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
	if action.Disruptive() && !req.Confirm {
		s.writeError(w, r, badRequest(
			"действие «%s» прерывает работу и требует подтверждения (confirm=true)", action.Title()))
		return
	}

	scope := model.Scope(req.Scope)
	if scope == "" {
		scope = model.ScopeVM
	}

	objectName := req.ObjectID
	switch scope {
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

// handleEvents streams state changes to the browser over server-sent events.
//
// SSE rather than websockets: the traffic is one-directional, it survives
// proxies that mangle upgrades, and the browser reconnects on its own.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, r, fmt.Errorf("потоковая передача не поддерживается этим соединением"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Nginx buffers responses by default, which would hold every event until
	// the connection closes.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	stream, cancel := s.bus.Subscribe()
	defer cancel()

	fmt.Fprintf(w, "retry: 5000\n\n")
	flusher.Flush()

	// A heartbeat keeps intermediaries from dropping an idle connection and
	// lets the client notice a dead link.
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

// ioOverview is what the disk monitoring screen loads in one call.
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

// handleStoragePaths lists the current state of every storage path of a host.
func (s *Server) handleStoragePaths(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.LatestMountSamples(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}
