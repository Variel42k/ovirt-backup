package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/dispatch"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/scheduler"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Раздел «Проверка ВМ»: площадки проверки загрузкой, расписания проверок,
// журнал и остатки проверочных ВМ.

// ---- Площадки ----

type verifyTargetView struct {
	*model.VerifyTarget
	UsedBy store.VerifyTargetUsers `json:"used_by"`
}

func (s *Server) handleListVerifyTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.store.ListVerifyTargets(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]verifyTargetView, 0, len(targets))
	for _, t := range targets {
		users, err := s.store.VerifyTargetUsers(r.Context(), t.ID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		views = append(views, verifyTargetView{VerifyTarget: t, UsedBy: users})
	}
	writeList(w, views)
}

type verifyTargetPayload struct {
	Name             string                 `json:"name"`
	Kind             model.VerifyTargetKind `json:"kind"`
	ServerID         string                 `json:"server_id"`
	ClusterID        string                 `json:"cluster_id"`
	StorageDomainIDs []string               `json:"storage_domain_ids"`
	MemoryMiB        int                    `json:"memory_mib"`
	VCPUs            int                    `json:"vcpus"`
	TimeoutSec       int                    `json:"timeout_sec"`
	MaxParallel      int                    `json:"max_parallel"`
	KeepOnFailure    bool                   `json:"keep_on_failure"`
}

func (p verifyTargetPayload) apply(t *model.VerifyTarget) {
	t.Name, t.Kind, t.ServerID = p.Name, p.Kind, strings.TrimSpace(p.ServerID)
	t.ClusterID, t.StorageDomainIDs = strings.TrimSpace(p.ClusterID), p.StorageDomainIDs
	t.MemoryMiB, t.VCPUs, t.TimeoutSec = p.MemoryMiB, p.VCPUs, p.TimeoutSec
	t.MaxParallel, t.KeepOnFailure = p.MaxParallel, p.KeepOnFailure
}

func (s *Server) handleCreateVerifyTarget(w http.ResponseWriter, r *http.Request) {
	var payload verifyTargetPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	target := &model.VerifyTarget{}
	payload.apply(target)
	if err := s.validateVerifyTarget(r.Context(), target); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.CreateVerifyTarget(r.Context(), target); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "verify.target.create", model.ScopeBackup, target.ID, true, target.Name)
	writeJSON(w, http.StatusCreated, target)
}

func (s *Server) handleUpdateVerifyTarget(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.GetVerifyTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var payload verifyTargetPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.apply(target)
	if err := s.validateVerifyTarget(r.Context(), target); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.UpdateVerifyTarget(r.Context(), target); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "verify.target.update", model.ScopeBackup, target.ID, true, target.Name)
	writeJSON(w, http.StatusOK, target)
}

func (s *Server) handleDeleteVerifyTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	target, err := s.store.GetVerifyTarget(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	users, err := s.store.VerifyTargetUsers(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !users.Empty() {
		var parts []string
		if len(users.Jobs) > 0 {
			parts = append(parts, "задания: "+strings.Join(users.Jobs, ", "))
		}
		if len(users.Schedules) > 0 {
			parts = append(parts, "расписания проверок: "+strings.Join(users.Schedules, ", "))
		}
		s.writeError(w, r, fmt.Errorf("%w: площадку «%s» используют %s — сначала выберите им другую площадку",
			store.ErrConflict, target.Name, strings.Join(parts, "; ")))
		return
	}
	if err := s.store.DeleteVerifyTarget(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "verify.target.delete", model.ScopeBackup, id, true, target.Name)
	w.WriteHeader(http.StatusNoContent)
}

// validateVerifyTarget сверяет площадку с подключением и его инвентарём.
func (s *Server) validateVerifyTarget(ctx context.Context, t *model.VerifyTarget) error {
	if err := t.Validate(); err != nil {
		return badRequest("%v", err)
	}
	srv, err := s.store.GetServer(ctx, t.ServerID)
	if err != nil {
		return badRequest("подключение площадки не найдено")
	}
	switch t.Kind {
	case model.VerifyTargetKVM:
		if !srv.Kind.UsesLibvirt() {
			return badRequest("площадка KVM поднимает проверочные ВМ на KVM-хосте, а %q — %s", srv.Name, srv.Kind.Title())
		}
		return nil
	case model.VerifyTargetEngine:
		if !srv.Kind.UsesOVirtAPI() {
			return badRequest("площадка в движке поднимает проверочные ВМ в oVirt и его производных, а %q — %s",
				srv.Name, srv.Kind.Title())
		}
	}
	clusters, err := s.store.ListClusters(ctx, srv.ID)
	if err != nil {
		return err
	}
	found := false
	for _, c := range clusters {
		found = found || c.ID == t.ClusterID
	}
	if !found {
		return badRequest("кластер площадки не найден у движка %q", srv.Name)
	}
	domains, err := s.store.ListStorageDomains(ctx, srv.ID)
	if err != nil {
		return err
	}
	known := map[string]*model.StorageDomain{}
	for _, d := range domains {
		known[d.ID] = d
	}
	for _, id := range t.StorageDomainIDs {
		d, ok := known[id]
		if !ok {
			return badRequest("домен хранения %s не найден у движка %q", id, srv.Name)
		}
		if d.Type != "" && d.Type != "data" {
			return badRequest("домен хранения %q не для данных (%s): диски ВМ на нём не создать", d.Name, d.Type)
		}
	}
	return nil
}

// validateVerifyTargetUse проверяет, что площадкой можно проверить копии
// ВМ этого подключения.
func (s *Server) validateVerifyTargetUse(ctx context.Context, sourceServerID, targetID string) error {
	target, err := s.store.GetVerifyTarget(ctx, targetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return badRequest("площадка проверки не найдена")
		}
		return err
	}
	srv, err := s.store.GetServer(ctx, target.ServerID)
	if err != nil {
		return badRequest("подключение площадки «%s» не найдено", target.Name)
	}
	if !srv.Enabled {
		return badRequest("подключение %q площадки «%s» отключено", srv.Name, target.Name)
	}
	if target.Kind == model.VerifyTargetEngine {
		if source, err := s.store.GetServer(ctx, sourceServerID); err == nil && !source.Kind.UsesOVirtAPI() {
			return badRequest("площадка «%s» в движке проверяет только копии ВМ oVirt; копии с %s проверяйте на "+
				"площадке KVM", target.Name, source.Kind.Title())
		}
	}
	return nil
}

type verifyTargetCapacity struct {
	Kind model.VerifyTargetKind `json:"kind"`
	// Domains — домены площадки в порядке приоритета с оценкой места.
	Domains  []backup.BootDomainCheck `json:"domains"`
	ChosenID string                   `json:"chosen_id,omitempty"`
	Message  string                   `json:"message"`
	NeedData int64                    `json:"need_data"`
	NeedFull int64                    `json:"need_full"`
	Basis    string                   `json:"basis,omitempty"`
}

// handleVerifyTargetCapacity: GET /verify/targets/{id}/capacity — какой домен
// площадки проверка выбрала бы сейчас. Параметры оценки — как у
// /boot-verify/engines/{id}/targets.
func (s *Server) handleVerifyTargetCapacity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	target, err := s.store.GetVerifyTarget(ctx, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	resp := verifyTargetCapacity{Kind: target.Kind, Domains: []backup.BootDomainCheck{}, NeedData: -1, NeedFull: -1}
	if target.Kind == model.VerifyTargetKVM {
		resp.Message = "место в каталоге scratch KVM-хоста проверяется перед каждой проверкой"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	srv, err := s.store.GetServer(ctx, target.ServerID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	all, err := s.bootTargets(ctx, srv, r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	resp.NeedData, resp.NeedFull, resp.Basis = all.NeedData, all.NeedFull, all.Basis
	byID := map[string]backup.BootDomainCheck{}
	for _, d := range all.Domains {
		byID[d.ID] = d
	}
	for _, id := range target.StorageDomainIDs {
		d, ok := byID[id]
		if !ok {
			d = backup.BootDomainCheck{ID: id, Name: id, Available: -1, Total: -1, Verdict: backup.BootSpaceInactive,
				Message: "домена нет в инвентаре движка: удалён или инвентарь ещё не обновился"}
		}
		resp.Domains = append(resp.Domains, d)
	}
	if chosen, err := dispatch.ChooseVerifyDomain(target.Name, resp.Domains); err != nil {
		resp.Message = err.Error()
	} else {
		resp.ChosenID = chosen.ID
		resp.Message = fmt.Sprintf("проверочная ВМ встанет на домен «%s»", chosen.Name)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- Расписания ----

type verifyScheduleView struct {
	*model.VerifySchedule
	Running bool `json:"running"`
}

func (s *Server) handleListVerifySchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := s.store.ListVerifySchedules(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	loc := s.scheduleLocation()
	views := make([]verifyScheduleView, 0, len(schedules))
	for _, v := range schedules {
		if v.Enabled {
			if next, err := scheduler.ValidateSchedule(v.Schedule, loc); err == nil {
				next = next.UTC()
				v.NextRunAt = &next
			}
		}
		view := verifyScheduleView{VerifySchedule: v}
		if s.scheduler != nil {
			view.Running = s.scheduler.VerifyScheduleActive(v.ID)
		}
		views = append(views, view)
	}
	writeList(w, views)
}

type verifySchedulePayload struct {
	Name            string   `json:"name"`
	Enabled         *bool    `json:"enabled"`
	TargetID        string   `json:"target_id"`
	ServerID        string   `json:"server_id"`
	VMIDs           []string `json:"vm_ids"`
	StorageTargetID string   `json:"storage_target_id"`
	Schedule        string   `json:"schedule"`
	MaxAgeHours     int      `json:"max_age_hours"`
}

func (p verifySchedulePayload) apply(v *model.VerifySchedule) {
	v.Name, v.TargetID, v.ServerID = p.Name, strings.TrimSpace(p.TargetID), strings.TrimSpace(p.ServerID)
	v.VMIDs, v.StorageTargetID = p.VMIDs, strings.TrimSpace(p.StorageTargetID)
	v.Schedule, v.MaxAgeHours = strings.TrimSpace(p.Schedule), p.MaxAgeHours
	if p.Enabled != nil {
		v.Enabled = *p.Enabled
	}
}

func (s *Server) handleCreateVerifySchedule(w http.ResponseWriter, r *http.Request) {
	var payload verifySchedulePayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	v := &model.VerifySchedule{Enabled: true}
	payload.apply(v)
	if err := s.validateVerifySchedule(r.Context(), v); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.CreateVerifySchedule(r.Context(), v); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "verify.schedule.create", model.ScopeBackup, v.ID, true, v.Name)
	s.reloadVerifySchedules(r.Context())
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleUpdateVerifySchedule(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.GetVerifySchedule(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var payload verifySchedulePayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.apply(v)
	if err := s.validateVerifySchedule(r.Context(), v); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.UpdateVerifySchedule(r.Context(), v); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "verify.schedule.update", model.ScopeBackup, v.ID, true, v.Name)
	s.reloadVerifySchedules(r.Context())
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteVerifySchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, err := s.store.GetVerifySchedule(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.DeleteVerifySchedule(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "verify.schedule.delete", model.ScopeBackup, id, true, v.Name)
	s.reloadVerifySchedules(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// handleRunVerifySchedule: POST /verify/schedules/{id}/run — прогон сейчас.
func (s *Server) handleRunVerifySchedule(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.GetVerifySchedule(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.scheduler == nil {
		s.writeError(w, r, fmt.Errorf("планировщик не запущен"))
		return
	}
	if err := s.scheduler.StartVerifySchedule(v.ID); err != nil {
		if errors.Is(err, scheduler.ErrVerifyScheduleBusy) {
			err = fmt.Errorf("%w: %v", scheduler.ErrJobBusy, err)
		}
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "verify.schedule.run", model.ScopeBackup, v.ID, true, v.Name)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) validateVerifySchedule(ctx context.Context, v *model.VerifySchedule) error {
	if err := v.Validate(); err != nil {
		return badRequest("%v", err)
	}
	if _, err := scheduler.ValidateSchedule(v.Schedule, s.scheduleLocation()); err != nil {
		return badRequest("%v", err)
	}
	srv, err := s.store.GetServer(ctx, v.ServerID)
	if err != nil {
		return badRequest("подключение, копии чьих ВМ проверять, не найдено")
	}
	if !srv.Kind.UsesOVirtAPI() && !srv.Kind.UsesLibvirt() {
		return badRequest("проверка загрузкой недоступна для копий %s", srv.Kind.Title())
	}
	if err := s.validateVerifyTargetUse(ctx, v.ServerID, v.TargetID); err != nil {
		return err
	}
	if v.StorageTargetID != "" {
		if _, err := s.store.GetStorageTarget(ctx, v.StorageTargetID); err != nil {
			return badRequest("хранилище копий не найдено")
		}
	}
	return nil
}

func (s *Server) reloadVerifySchedules(ctx context.Context) {
	if s.scheduler != nil {
		if err := s.scheduler.Reload(ctx); err != nil {
			s.log.Warn().Err(err).Msg("не удалось перечитать расписания проверок")
		}
	}
}

func (s *Server) scheduleLocation() *time.Location {
	if s.scheduler != nil {
		return s.scheduler.Location()
	}
	return s.cfg.Location()
}

// ---- Журнал ----

type bootCheckView struct {
	*model.BootCheck
	Summary           string   `json:"summary,omitempty"`
	Problems          []string `json:"problems,omitempty"`
	Duration          string   `json:"duration,omitempty"`
	Host              string   `json:"host,omitempty"`
	CheckVMName       string   `json:"check_vm_name,omitempty"`
	ClusterName       string   `json:"cluster_name,omitempty"`
	StorageDomainName string   `json:"storage_domain_name,omitempty"`
	Started           bool     `json:"started"`
	AgentReplied      bool     `json:"agent_replied"`
	GuestOS           string   `json:"guest_os,omitempty"`
	Hostname          string   `json:"hostname,omitempty"`
	Elapsed           string   `json:"elapsed,omitempty"`
	Notes             []string `json:"notes,omitempty"`
}

// handleListBootChecks: GET /verify/checks — журнал проверок загрузкой.
func (s *Server) handleListBootChecks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.BootCheckFilter{
		TargetID: q.Get("target_id"), ServerID: q.Get("server_id"), VMID: q.Get("vm_id"),
		Status: model.RunStatus(q.Get("status")), Trigger: q.Get("trigger"),
		Limit: queryInt(r, "limit", 100), Offset: queryInt(r, "offset", 0),
	}
	if days := queryInt(r, "days", 0); days > 0 {
		since := time.Now().AddDate(0, 0, -days)
		filter.Since = &since
	}
	checks, err := s.store.ListBootChecks(r.Context(), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]bootCheckView, 0, len(checks))
	for _, c := range checks {
		views = append(views, bootCheckViewOf(c))
	}
	writeList(w, views)
}

// bootCheckViewOf разворачивает отчёт проверки в поля журнала; сам отчёт в
// ответ не попадает — он есть в карточке проверки.
func bootCheckViewOf(c *model.BootCheck) bootCheckView {
	view := bootCheckView{BootCheck: c}
	var report backup.VerifyReport
	if c.Details != "" && json.Unmarshal([]byte(c.Details), &report) == nil {
		view.Summary, view.Problems, view.Duration = report.Summary, report.Problems, report.Duration
		if b := report.Boot; b != nil {
			view.Host, view.CheckVMName, view.Started, view.AgentReplied = b.Host, firstNonEmpty(b.VMName, b.DomainName), b.Started, b.AgentReplied
			view.ClusterName, view.StorageDomainName = b.ClusterName, b.StorageDomainName
			view.GuestOS, view.Hostname, view.Elapsed, view.Notes = b.GuestOS, b.Hostname, b.Elapsed, b.Notes
		}
	}
	c.Details = ""
	return view
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// ---- Остатки ----

func (s *Server) handleListVerifyLeftovers(w http.ResponseWriter, r *http.Request) {
	scan, err := s.engine.FindVerifyLeftovers(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, scan)
}

type verifyLeftoverRemoveRequest struct {
	Kind     string `json:"kind"`
	ServerID string `json:"server_id"`
	Ref      string `json:"ref"`
}

func (s *Server) handleRemoveVerifyLeftover(w http.ResponseWriter, r *http.Request) {
	var req verifyLeftoverRemoveRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Kind == "" || req.ServerID == "" || req.Ref == "" {
		s.writeError(w, r, badRequest("нужны kind, server_id и ref"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Minute)
	defer cancel()
	err := s.engine.RemoveVerifyLeftover(ctx, req.Kind, req.ServerID, req.Ref)
	s.audit(r, "verify.leftover.remove", model.ScopeBackup, req.Ref, err == nil, req.Kind)
	if err != nil {
		if errors.Is(err, dispatch.ErrLeftoverActive) {
			err = fmt.Errorf("%w: %v", store.ErrConflict, err)
		}
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCancelBootCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetVerifyRun(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	err := s.engine.CancelBootCheck(id)
	s.audit(r, "verify.cancel", model.ScopeBackup, id, err == nil, "")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

func (s *Server) handleCancelVerifyDiskTransfer(w http.ResponseWriter, r *http.Request) {
	var req verifyLeftoverRemoveRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Kind != dispatch.LeftoverEngineDisk || req.ServerID == "" || req.Ref == "" {
		s.writeError(w, r, badRequest("нужны kind=engine_disk, server_id и ref"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Minute)
	defer cancel()
	err := s.engine.CancelVerifyDisk(ctx, req.ServerID, req.Ref)
	s.audit(r, "verify.disk.cancel_transfer", model.ScopeBackup, req.Ref, err == nil, req.ServerID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
