package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/retention"
	"github.com/Variel42k/ovirt-backup/internal/scheduler"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// jobPayload is the write shape of a backup job.
type jobPayload struct {
	Name     string `json:"name"`
	Enabled  *bool  `json:"enabled"`
	ServerID string `json:"server_id"`

	VMIDs          []string `json:"vm_ids"`
	VMNameRegex    string   `json:"vm_name_regex"`
	ClusterIDs     []string `json:"cluster_ids"`
	Tags           []string `json:"tags"`
	ExcludeVMIDs   []string `json:"exclude_vm_ids"`
	ExcludeDiskIDs []string `json:"exclude_disk_ids"`

	Type         string `json:"type"`
	FullEvery    int    `json:"full_every"`
	FallbackType string `json:"fallback_type"`

	Schedule string `json:"schedule"`
	// MaxDurationMinutes ограничивает длительность запуска; 0 — без предела.
	MaxDurationMinutes int `json:"max_duration_minutes"`

	StorageTargetIDs []string              `json:"storage_target_ids"`
	StorageMode      string                `json:"storage_mode"`
	OVAHostID        string                `json:"ova_host_id"`
	OVADirectory     string                `json:"ova_directory"`
	Retention        model.RetentionPolicy `json:"retention"`

	Quiesce bool `json:"quiesce"`
	// Consistency — crash, filesystem или application; пусто — вывести из
	// quiesce, как у клиентов прежней версии.
	Consistency        string `json:"consistency"`
	RequireConsistency bool   `json:"require_consistency"`
	// MaxFreezeSeconds — предел окна заморозки; 0 — backup.max_freeze.
	MaxFreezeSeconds int `json:"max_freeze_seconds"`
	// FreezeBy — service или engine; пусто — служба.
	FreezeBy string `json:"freeze_by"`
	// MaxReadMBps — предел чтения с хранилища ВМ, МиБ/с; 0 — предел службы.
	MaxReadMBps   int                 `json:"max_read_mbps"`
	VerifyAfter   string              `json:"verify_after"`
	VerifyOptions model.VerifyOptions `json:"verify_options"`
	ExportQcow2   bool                `json:"export_qcow2"`
	Encrypt       bool                `json:"encrypt"`

	Priority    int `json:"priority"`
	Concurrency int `json:"concurrency"`
}

func (p jobPayload) apply(dst *model.BackupJob) {
	dst.Name = p.Name
	dst.ServerID = p.ServerID
	dst.VMIDs = p.VMIDs
	dst.VMNameRegex = p.VMNameRegex
	dst.ClusterIDs = p.ClusterIDs
	dst.Tags = p.Tags
	dst.ExcludeVMIDs = p.ExcludeVMIDs
	dst.ExcludeDiskIDs = p.ExcludeDiskIDs
	dst.Type = model.BackupType(p.Type)
	dst.FullEvery = p.FullEvery
	dst.Schedule = p.Schedule
	dst.MaxDuration = time.Duration(p.MaxDurationMinutes) * time.Minute
	dst.StorageTargetIDs = p.StorageTargetIDs
	dst.OVAHostID = strings.TrimSpace(p.OVAHostID)
	dst.OVADirectory = strings.TrimSpace(p.OVADirectory)
	if p.StorageMode != "" {
		dst.StorageMode = model.StorageMode(p.StorageMode)
	}
	dst.Retention = p.Retention
	dst.Quiesce = p.Quiesce
	dst.Consistency = model.Consistency(strings.TrimSpace(p.Consistency))
	dst.RequireConsistency = p.RequireConsistency
	dst.MaxFreeze = time.Duration(p.MaxFreezeSeconds) * time.Second
	dst.FreezeBy = model.FreezeBy(strings.TrimSpace(p.FreezeBy))
	dst.MaxReadMBps = p.MaxReadMBps
	dst.VerifyAfter = model.VerifyMode(p.VerifyAfter)
	dst.VerifyOptions = p.VerifyOptions
	dst.ExportQcow2 = p.ExportQcow2
	dst.Encrypt = p.Encrypt
	dst.Priority = p.Priority
	dst.Concurrency = p.Concurrency
	if p.FallbackType != "" {
		dst.FallbackType = model.BackupType(p.FallbackType)
	}
	if p.Enabled != nil {
		dst.Enabled = *p.Enabled
	}
}

func (s *Server) validateJob(ctx context.Context, job *model.BackupJob) error {
	if err := job.Validate(); err != nil {
		return badRequest("%v", err)
	}
	srv, err := s.store.GetServer(ctx, job.ServerID)
	if err != nil {
		return badRequest("сервер %s не найден", job.ServerID)
	}
	if !srv.Kind.SupportsBackup() {
		return badRequest("резервное копирование %s в этой версии не поддерживается", srv.Kind.Title())
	}
	if job.FreezeBy.NeedsEngine() && !srv.Kind.UsesOVirtAPI() {
		return badRequest("заморозку силами движка поддерживает только oVirt и его производные: "+
			"у %s выберите заморозку службой", srv.Kind.Title())
	}
	if srv.Kind.UsesProxmoxAPI() {
		if !srv.HasProxmoxDataPlane() {
			return badRequest("для задания Proxmox сначала настройте SSH-канал данных и закрепите ключи всех узлов")
		}
		if job.Type != model.BackupFull {
			return badRequest("Proxmox поддерживает только полный нативный бэкап vzdump")
		}
		if len(job.ExcludeDiskIDs) > 0 || job.ExportQcow2 {
			return badRequest("нативный vzdump сохраняет гостя целиком; исключение дисков и экспорт qcow2 недоступны")
		}
		if job.RequireConsistency {
			return badRequest("заморозку гостя Proxmox выполняет сам vzdump (agent=1 в настройках гостя): " +
				"служба не видит её результата и не может требовать уровень согласованности")
		}
		if job.VerifyAfter != "" && job.VerifyAfter != model.VerifyQuick &&
			job.VerifyAfter != model.VerifyManifest && job.VerifyAfter != model.VerifyChain {
			return badRequest("для нативного архива Proxmox доступны проверки quick, manifest и chain")
		}
	}
	for _, id := range job.StorageTargetIDs {
		target, err := s.store.GetStorageTarget(ctx, id)
		if err != nil {
			return badRequest("хранилище %s не найдено", id)
		}
		if !target.Enabled {
			return badRequest("хранилище %q отключено", target.Name)
		}
	}
	if job.Schedule != "" {
		loc := s.cfg.Location()
		if s.scheduler != nil {
			loc = s.scheduler.Location()
		}
		if _, err := scheduler.ValidateSchedule(job.Schedule, loc); err != nil {
			return badRequest("%v", err)
		}
	}
	if job.ExportQcow2 {
		if job.Type == model.BackupConfig || job.Type == model.BackupOVA {
			return badRequest("export_qcow2 is only available for backups containing disks")
		}
		if _, err := backup.FindQemuImg(s.cfg.Backup.QemuImgPath); err != nil {
			return badRequest("export_qcow2: %v", err)
		}
	}
	if job.VerifyAfter != "" {
		if !knownVerifyMode(job.VerifyAfter) {
			return badRequest("неизвестный режим проверки: %q", job.VerifyAfter)
		}
		if job.VerifyAfter.NeedsHypervisor() {
			if err := s.validateBootOptions(ctx, job.ServerID, job.Type, &job.VerifyOptions); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListBackupJobs(r.Context(), r.URL.Query().Get("server_id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, jobs)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetBackupJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var payload jobPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	job := &model.BackupJob{Enabled: true, Concurrency: 1, FallbackType: model.BackupSnapshot,
		ReplicationEnabled: true}
	payload.apply(job)
	if job.Type == model.BackupOVA {
		job.StorageMode = model.StorageModeSeparate
		job.ReplicationEnabled = false
		job.StorageTargetIDs = nil
		job.Retention = model.RetentionPolicy{}
		job.VerifyAfter = ""
		job.ExportQcow2 = false
	} else {
		job.NormalizeStorageMode()
	}
	if job.Type != model.BackupOVA && job.Retention.Empty() {
		job.Retention = model.DefaultRetention()
	}
	if err := s.validateJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}

	if err := s.store.CreateBackupJob(r.Context(), job); err != nil {
		s.audit(r, "job.create", model.ScopeBackup, job.Name, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "job.create", model.ScopeBackup, job.ID, true, job.Name)

	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.log.Warn().Err(err).Msg("не удалось перечитать расписание")
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.store.GetBackupJob(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	previousPrimary := ""
	if len(job.StorageTargetIDs) > 0 {
		previousPrimary = job.StorageTargetIDs[0]
	}
	var payload jobPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.apply(job)
	if job.Type == model.BackupOVA {
		job.StorageMode = model.StorageModeSeparate
		job.ReplicationEnabled = false
		job.StorageTargetIDs = nil
		job.Retention = model.RetentionPolicy{}
		job.VerifyAfter = ""
		job.ExportQcow2 = false
	} else {
		job.NormalizeStorageMode()
	}
	if job.ReplicationEnabled && previousPrimary != "" && len(job.StorageTargetIDs) > 0 &&
		job.StorageTargetIDs[0] != previousPrimary {
		s.writeError(w, r, badRequest("основное хранилище меняется отдельным действием change-primary"))
		return
	}
	if job.ReplicationEnabled && !slices.Contains(job.StorageTargetIDs, previousPrimary) {
		s.writeError(w, r, badRequest("нельзя удалить основное хранилище обычным редактированием задания"))
		return
	}
	if err := s.validateJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}

	if err := s.store.UpdateBackupJob(r.Context(), job); err != nil {
		s.audit(r, "job.update", model.ScopeBackup, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "job.update", model.ScopeBackup, id, true, job.Name)

	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.log.Warn().Err(err).Msg("не удалось перечитать расписание")
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteBackupJob(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "job.delete", model.ScopeBackup, id, true, "")

	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.log.Warn().Err(err).Msg("не удалось перечитать расписание")
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor := "api"
	if p := principalFrom(r.Context()); p != nil {
		actor = "user:" + p.Username
	}

	jobRun, err := s.scheduler.TriggerJob(context.WithoutCancel(r.Context()), id, actor, nil)
	if err != nil {
		s.audit(r, "job.run", model.ScopeBackup, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "job.run", model.ScopeBackup, id, true, "")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":     "queued",
		"job_run_id": jobRun.ID,
		"vms":        jobRun.VMCount,
		"replicas":   jobRun.ReplicaCount,
	})
}

// handlePreviewJob shows which VMs a job's selector currently matches, so an
// operator can see the effect of a filter before saving it.
func (s *Server) handlePreviewJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetBackupJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	vms, err := s.store.ListVMs(r.Context(), job.ServerID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	type preview struct {
		VMID     string `json:"vm_id"`
		VMName   string `json:"vm_name"`
		Status   string `json:"status"`
		Disks    int    `json:"disks"`
		Included bool   `json:"included"`
		Reason   string `json:"reason"`
	}

	selector, err := model.NewVMSelector(job)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	out := make([]preview, 0, len(vms))
	for _, vm := range vms {
		p := preview{VMID: vm.ID, VMName: vm.Name, Status: vm.Status, Disks: vm.DiskCount}
		p.Included, p.Reason = selector.Match(vm)
		out = append(out, p)
	}
	writeList(w, out)
}

// adHocRequest starts one backup outside any job.
type adHocRequest struct {
	ServerID        string `json:"server_id"`
	VMID            string `json:"vm_id"`
	Type            string `json:"type"`
	StorageTargetID string `json:"storage_target_id"`
	Quiesce         bool   `json:"quiesce"`
	// Consistency и RequireConsistency — как у задания; пусто — из quiesce.
	Consistency        string              `json:"consistency"`
	RequireConsistency bool                `json:"require_consistency"`
	MaxFreezeSeconds   int                 `json:"max_freeze_seconds"`
	FreezeBy           string              `json:"freeze_by"`
	Encrypt            bool                `json:"encrypt"`
	VerifyAfter        string              `json:"verify_after"`
	VerifyOptions      model.VerifyOptions `json:"verify_options"`
	// RetainDays ставит срок годности разовой копии; 0 — хранить бессрочно.
	RetainDays   int      `json:"retain_days"`
	ExcludeDisks []string `json:"exclude_disk_ids"`
	OVAHostID    string   `json:"ova_host_id"`
	OVADirectory string   `json:"ova_directory"`
}

func (s *Server) handleAdHocBackup(w http.ResponseWriter, r *http.Request) {
	var req adHocRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.ServerID == "" || req.VMID == "" || (req.Type != string(model.BackupOVA) && req.StorageTargetID == "") {
		s.writeError(w, r, badRequest("нужны server_id, vm_id и storage_target_id (для OVA — host и directory)"))
		return
	}
	if req.Type == "" {
		req.Type = string(model.BackupFull)
	}
	srv, err := s.store.GetServer(r.Context(), req.ServerID)
	if err != nil {
		s.writeError(w, r, badRequest("сервер не найден"))
		return
	}
	if !srv.Kind.SupportsBackup() {
		s.writeError(w, r, badRequest("резервное копирование %s в этой версии не поддерживается", srv.Kind.Title()))
		return
	}
	if srv.Kind.UsesProxmoxAPI() {
		if !srv.HasProxmoxDataPlane() {
			s.writeError(w, r, badRequest("для Proxmox сначала настройте SSH-канал данных и закрепите ключи всех узлов"))
			return
		}
		if model.BackupType(req.Type) != model.BackupFull || len(req.ExcludeDisks) > 0 {
			s.writeError(w, r, badRequest("Proxmox поддерживает полный нативный vzdump без исключения отдельных дисков"))
			return
		}
	}
	consistency := model.Consistency(strings.TrimSpace(req.Consistency))
	if consistency != "" && !consistency.Valid() {
		s.writeError(w, r, badRequest("неизвестный уровень согласованности: %q", req.Consistency))
		return
	}
	if req.RequireConsistency && srv.Kind.UsesProxmoxAPI() {
		s.writeError(w, r, badRequest("заморозку гостя Proxmox выполняет сам vzdump: требовать уровень нельзя"))
		return
	}
	freezeBy := model.FreezeBy(strings.TrimSpace(req.FreezeBy))
	if !freezeBy.Valid() {
		s.writeError(w, r, badRequest("неизвестный способ заморозки: %q", req.FreezeBy))
		return
	}
	if freezeBy.NeedsEngine() && !srv.Kind.UsesOVirtAPI() {
		s.writeError(w, r, badRequest("заморозку силами движка поддерживает только oVirt и его производные"))
		return
	}
	maxFreeze := time.Duration(req.MaxFreezeSeconds) * time.Second
	if maxFreeze < 0 || maxFreeze > model.MaxFreezeLimit {
		s.writeError(w, r, badRequest("предел заморозки должен быть от 0 до %d с", int(model.MaxFreezeLimit.Seconds())))
		return
	}
	verifyMode := model.VerifyMode(req.VerifyAfter)
	if verifyMode != "" {
		if !knownVerifyMode(verifyMode) {
			s.writeError(w, r, badRequest("неизвестный режим проверки: %q", verifyMode))
			return
		}
		if srv.Kind.UsesProxmoxAPI() && verifyMode != model.VerifyQuick &&
			verifyMode != model.VerifyManifest && verifyMode != model.VerifyChain {
			s.writeError(w, r, badRequest("для нативного архива Proxmox доступны проверки quick, manifest и chain"))
			return
		}
		if verifyMode.NeedsHypervisor() {
			if err := s.validateBootOptions(r.Context(), req.ServerID, model.BackupType(req.Type), &req.VerifyOptions); err != nil {
				s.writeError(w, r, err)
				return
			}
		}
	}

	actor := "api"
	if p := principalFrom(r.Context()); p != nil {
		actor = "user:" + p.Username
	}

	runReq := backup.RunRequest{
		ServerID:           req.ServerID,
		VMID:               req.VMID,
		Type:               model.BackupType(req.Type),
		FallbackType:       model.BackupSnapshot,
		StorageTargetID:    req.StorageTargetID,
		ExcludeDiskIDs:     req.ExcludeDisks,
		Quiesce:            req.Quiesce,
		Consistency:        consistency,
		RequireConsistency: req.RequireConsistency && consistency.NeedsFreeze(),
		MaxFreeze:          maxFreeze,
		FreezeBy:           freezeBy,
		Encrypt:            req.Encrypt,
		VerifyAfter:        verifyMode,
		VerifyOptions:      req.VerifyOptions,
		OVAHostID:          req.OVAHostID,
		OVADirectory:       req.OVADirectory,
		TriggeredBy:        actor,
	}
	if req.RetainDays > 0 {
		runReq.Retention = model.RetentionPolicy{MaxAge: time.Duration(req.RetainDays) * 24 * time.Hour}
	}

	s.audit(r, "backup.run", model.ScopeVM, req.VMID, true, req.Type)

	// The run outlives this request: a full backup takes hours, and the client
	// must not have to hold a connection open for it.
	//
	// Через планировщик, а не напрямую в движок: он держит предел
	// backup.workers, регистрирует запуск как отменяемый и сам выполняет
	// проверку после бэкапа. Прямой вызов движка всё это обходил, и десять
	// нажатий кнопки давали десять параллельных бэкапов при workers: 2.
	s.scheduler.RunOnce(context.WithoutCancel(r.Context()), runReq)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "queued",
		"message": "бэкап поставлен в очередь; следите за прогрессом в списке запусков",
	})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	filter := store.RunFilter{
		ServerID:       r.URL.Query().Get("server_id"),
		VMID:           r.URL.Query().Get("vm_id"),
		JobID:          r.URL.Query().Get("job_id"),
		ChainID:        r.URL.Query().Get("chain_id"),
		TargetID:       r.URL.Query().Get("storage_target_id"),
		IncludeDeleted: queryBool(r, "include_deleted"),
		Limit:          queryInt(r, "limit", 100),
		Offset:         queryInt(r, "offset", 0),
	}
	if status := r.URL.Query().Get("status"); status != "" {
		filter.Statuses = []model.RunStatus{model.RunStatus(status)}
	}
	if days := queryInt(r, "days", 0); days > 0 {
		since := time.Now().AddDate(0, 0, -days)
		filter.Since = &since
	}

	runs, err := s.store.ListBackupRuns(r.Context(), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	for _, run := range runs {
		if err := s.store.EnrichRunCopies(r.Context(), run, false); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	writeList(w, runs)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetBackupRunFull(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.EnrichRunCopies(r.Context(), run, true); err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleRunChain returns the chain a restore point depends on, which is what
// the UI shows before a restore so the operator can see what has to be intact.
// handleRunEvents отдаёт хронологию запуска: что и когда делала служба.
//
// Отдельным запросом, а не полем точки: список копий читают все страницы, а
// хронология нужна только когда открыли конкретную точку.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetBackupRun(r.Context(), r.PathValue("id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	events, err := s.store.ListRunEvents(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, events)
}

type runTelemetryResponse struct {
	Events    []*model.RunEvent      `json:"events"`
	Databases []*model.DBStatsSample `json:"databases"`
	Disks     []*model.DiskSample    `json:"disks"`
}

func (s *Server) handleRunTelemetry(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetBackupRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	events, err := s.store.ListRunEvents(r.Context(), run.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	databases, err := s.store.ListDBStatsSamples(r.Context(), run.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	since := run.CreatedAt.Add(-30 * time.Second)
	until := time.Now().UTC()
	if run.EndedAt != nil {
		until = run.EndedAt.Add(30 * time.Second)
	}
	disks, err := s.store.ListDiskSamples(r.Context(), store.DiskSampleFilter{ServerID: run.ServerID, RunID: run.ID, VMID: run.VMID, Since: since, Until: until, Limit: 5000})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runTelemetryResponse{Events: events, Databases: databases, Disks: disks})
}

func (s *Server) handleRunChain(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetBackupRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	chain, err := s.store.ListBackupRuns(r.Context(), store.RunFilter{
		ChainID:        run.ChainID,
		IncludeDeleted: true,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	for _, item := range chain {
		if err := s.store.EnrichRunCopies(r.Context(), item, false); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	writeList(w, chain)
}

func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// purge=true стирает данные немедленно, минуя карантин. Нужно, когда место
	// требуется прямо сейчас; в остальных случаях промежуток, за который можно
	// передумать, важнее освободившихся гигабайт.
	if queryBool(r, "purge") {
		if err := s.engine.PurgeRunData(context.WithoutCancel(r.Context()), id); err != nil {
			s.audit(r, "backup.purge", model.ScopeBackup, id, false, err.Error())
			s.writeError(w, r, err)
			return
		}
		s.audit(r, "backup.purge", model.ScopeBackup, id, true, "данные стёрты без карантина")
		writeJSON(w, http.StatusOK, map[string]string{"status": "purged"})
		return
	}

	if err := s.engine.DeleteRunData(context.WithoutCancel(r.Context()), id); err != nil {
		s.audit(r, "backup.delete", model.ScopeBackup, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}

	// Ответ различает карантин и стирание: «deleted» там, где данные ещё целы,
	// ввело бы в заблуждение — оператор решил бы, что место освободилось.
	run, err := s.store.GetBackupRun(r.Context(), id)
	if err == nil && run.PurgeAfter != nil {
		s.audit(r, "backup.delete", model.ScopeBackup, id, true,
			"в карантине до "+run.PurgeAfter.Format(time.RFC3339))
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      "quarantined",
			"purge_after": run.PurgeAfter,
		})
		return
	}

	s.audit(r, "backup.delete", model.ScopeBackup, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleListTrash отдаёт копии, которые ещё можно вернуть.
func (s *Server) handleListTrash(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.QuarantinedRuns(r.Context(), queryInt(r, "limit", 200))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, runs)
}

// handleRestoreFromTrash возвращает копию из карантина.
func (s *Server) handleRestoreFromTrash(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.RestoreRunFromQuarantine(r.Context(), id); err != nil {
		s.audit(r, "backup.undelete", model.ScopeBackup, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "backup.undelete", model.ScopeBackup, id, true, "возвращено из карантина")
	writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.scheduler.CancelRun(id) {
		s.writeError(w, r, badRequest("бэкап %s сейчас не выполняется", id))
		return
	}
	s.audit(r, "backup.cancel", model.ScopeBackup, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelling"})
}

// verifyRequest picks how deeply to check a stored backup.
type verifyRequest struct {
	Mode   string `json:"mode"`
	CopyID string `json:"copy_id"`
	// Параметры пробного запуска; для остальных режимов игнорируются.
	BootHostID    string `json:"boot_host_id"`
	DiskID        string `json:"disk_id"`
	MemoryMiB     int    `json:"memory_mib"`
	VCPUs         int    `json:"vcpus"`
	TimeoutSec    int    `json:"timeout_sec"`
	KeepOnFailure bool   `json:"keep_on_failure"`
}

func (s *Server) handleVerifyRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req verifyRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	mode := model.VerifyMode(req.Mode)
	if mode == "" {
		mode = model.VerifyManifest
	}
	if !knownVerifyMode(mode) {
		s.writeError(w, r, badRequest("неизвестный режим проверки: %q", mode))
		return
	}
	opts := model.VerifyOptions{
		BootHostID:    req.BootHostID,
		DiskID:        req.DiskID,
		MemoryMiB:     req.MemoryMiB,
		VCPUs:         req.VCPUs,
		TimeoutSec:    req.TimeoutSec,
		KeepOnFailure: req.KeepOnFailure,
	}

	// The boot test starts a copy of a real system. Refusing an unusable
	// request now is better than reporting the failure ten minutes later,
	// after the image has already been streamed to the hypervisor.
	if mode.NeedsHypervisor() {
		run, err := s.store.GetBackupRun(r.Context(), id)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if err := s.validateBootOptions(r.Context(), run.ServerID, run.Type, &opts); err != nil {
			s.writeError(w, r, err)
			return
		}
	}

	// Quick checks answer in a moment, so they run inline and the operator
	// gets the verdict straight away. The heavy modes go to the background.
	if mode == model.VerifyQuick || mode == model.VerifyChain {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()

		record, err := s.engine.VerifyCopy(ctx, id, req.CopyID, mode, opts)
		if s.bus != nil && record != nil {
			s.bus.Publish(events.Event{Kind: events.KindVerifyRun, ObjectID: record.ID,
				Message: "verification finished", Payload: record})
		}
		s.audit(r, "backup.verify", model.ScopeBackup, id, err == nil, string(mode))
		if err != nil {
			// A failed verification is a valid answer, not a broken request:
			// return the record with its details.
			writeJSON(w, http.StatusOK, record)
			return
		}
		writeJSON(w, http.StatusOK, record)
		return
	}

	s.audit(r, "backup.verify", model.ScopeBackup, id, true, string(mode))
	go func() {
		ctx := context.WithoutCancel(r.Context())
		record, err := s.engine.VerifyCopy(ctx, id, req.CopyID, mode, opts)
		if err != nil {
			s.log.Warn().Err(err).Str("run", id).Str("режим", string(mode)).Msg("проверка не пройдена")
		}
		if s.bus != nil && record != nil {
			s.bus.Publish(events.Event{Kind: events.KindVerifyRun, ObjectID: record.ID,
				Message: "verification finished", Payload: record})
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "queued",
		"message": "проверка запущена; результат появится в истории проверок",
	})
}

func knownVerifyMode(want model.VerifyMode) bool {
	for _, mode := range model.AllVerifyModes() {
		if want == mode {
			return true
		}
	}
	return false
}

// validateBootOptions resolves and validates the hypervisor a boot test will
// run on, filling opts.BootHostID when the operator did not name one.
//
// The default only applies when the backup itself came from a libvirt host:
// then "boot it back where it came from" is what the operator means. For an
// oVirt backup there is no such answer — the engine cannot start a foreign
// image — so the request is refused with the list of hosts that would work.
func (s *Server) validateBootOptions(ctx context.Context, sourceServerID string, backupType model.BackupType, opts *model.VerifyOptions) error {
	if backupType == model.BackupConfig || backupType == model.BackupOVA {
		return badRequest("пробный запуск недоступен для типа бэкапа %q: он не содержит восстанавливаемого образа диска", backupType)
	}
	if err := opts.Validate(); err != nil {
		return badRequest("параметры пробного запуска: %v", err)
	}

	if opts.BootHostID == "" {
		own, err := s.store.GetServer(ctx, sourceServerID)
		if err != nil {
			return err
		}
		if !own.Kind.UsesLibvirt() {
			return badRequest("для пробного запуска нужно указать KVM-хост: %s", s.bootHostHint(ctx))
		}
		opts.BootHostID = own.ID
	}

	host, err := s.store.GetServer(ctx, opts.BootHostID)
	if err != nil {
		return badRequest("хост для пробного запуска не найден")
	}
	if !host.Kind.UsesLibvirt() {
		return badRequest(
			"пробный запуск выполняется только на подключении типа KVM, а %q — %s",
			host.Name, host.Kind.Title())
	}
	if !host.Enabled {
		return badRequest("подключение %q отключено", host.Name)
	}
	return nil
}

// bootHostHint names the connections a boot test could use, so the error tells
// the operator what to pick instead of only what went wrong.
func (s *Server) bootHostHint(ctx context.Context) string {
	servers, err := s.store.ListServers(ctx)
	if err != nil {
		return "подходящих подключений не найдено"
	}
	var names []string
	for _, srv := range servers {
		if srv.Kind.UsesLibvirt() && srv.Enabled {
			names = append(names, srv.Name)
		}
	}
	if len(names) == 0 {
		return "ни одного подключения типа KVM не настроено, а движок oVirt " +
			"не умеет поднимать ВМ из чужого образа — добавьте KVM-хост для проверок"
	}
	return "доступны " + strings.Join(names, ", ")
}

func (s *Server) handleListVerifications(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListVerifyRuns(r.Context(), r.URL.Query().Get("run_id"), queryInt(r, "limit", 100))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetVerification(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.GetVerifyRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// restoreRequest describes a restore operation.
type restoreRequest struct {
	CopyID         string   `json:"copy_id"`
	Target         string   `json:"target"` // file | disk | new_disk
	DiskIDs        []string `json:"disk_ids"`
	OutputDir      string   `json:"output_dir"`
	OutputFormat   string   `json:"output_format"` // raw | qcow2
	TargetServerID string   `json:"target_server_id"`
	TargetDiskID   string   `json:"target_disk_id"`
	TargetDomainID string   `json:"target_domain_id"`
	AttachToVMID   string   `json:"attach_to_vm_id"`
	// Confirm обязателен для записи поверх существующего диска.
	Confirm bool `json:"confirm"`
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req restoreRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	target := model.RestoreTarget(req.Target)
	switch target {
	case model.RestoreToFile, model.RestoreToDisk, model.RestoreToNewDisk:
	default:
		s.writeError(w, r, badRequest("неизвестная цель восстановления: %q", req.Target))
		return
	}
	// Writing into an existing disk destroys whatever is on it, including a
	// running VM's data.
	if target == model.RestoreToDisk && !req.Confirm {
		s.writeError(w, r, badRequest(
			"восстановление в существующий диск затрёт его содержимое и требует подтверждения (confirm=true)"))
		return
	}
	if target == model.RestoreToNewDisk && req.TargetDomainID == "" {
		s.writeError(w, r, badRequest("для нового диска нужно указать домен хранения"))
		return
	}
	// Каталог проверяем здесь, а не только в движке: иначе отказ прилетел бы
	// в фоновую горутину и увиделся бы оператором как «восстановление не
	// выполнено» без внятной причины.
	if target == model.RestoreToFile {
		if _, err := backup.ResolveOutputDir(req.OutputDir, s.cfg.Backup.RestoreRoots()); err != nil {
			s.writeError(w, r, badRequest("%s", err))
			return
		}
	}

	actor := "api"
	if p := principalFrom(r.Context()); p != nil {
		actor = "user:" + p.Username
	}

	restoreReq := backup.RestoreRequest{
		RunID:          id,
		CopyID:         req.CopyID,
		DiskIDs:        req.DiskIDs,
		Target:         target,
		OutputDir:      req.OutputDir,
		OutputFormat:   req.OutputFormat,
		TargetServerID: req.TargetServerID,
		TargetDiskID:   req.TargetDiskID,
		TargetDomainID: req.TargetDomainID,
		AttachToVMID:   req.AttachToVMID,
		TriggeredBy:    actor,
	}

	// Validate the restore point before answering, so an impossible request
	// fails immediately instead of in a background goroutine nobody watches.
	set, err := s.engine.LoadChainCopy(r.Context(), id, req.CopyID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	set.Close()

	s.audit(r, "backup.restore", model.ScopeBackup, id, true, string(target))

	go func() {
		ctx := context.WithoutCancel(r.Context())
		record, err := s.engine.Restore(ctx, restoreReq)
		if err != nil {
			s.log.Error().Err(err).Str("run", id).Msg("восстановление не выполнено")
		}
		if s.bus != nil && record != nil {
			s.bus.Publish(events.Event{Kind: events.KindRestoreRun, ObjectID: record.ID,
				Message: "restore finished", Payload: record})
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "queued",
		"disks":   len(set.DiskOrder),
		"message": "восстановление запущено; следите за прогрессом в истории восстановлений",
	})
}

func (s *Server) handleListRestores(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListRestoreRuns(r.Context(), r.URL.Query().Get("run_id"), queryInt(r, "limit", 100))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetRestore(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.GetRestoreRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// retentionRequest evaluates a policy against a VM's stored backups.
type retentionRequest struct {
	ServerID        string                `json:"server_id"`
	VMID            string                `json:"vm_id"`
	StorageTargetID string                `json:"storage_target_id"`
	Policy          model.RetentionPolicy `json:"policy"`
	PlanToken       string                `json:"plan_token,omitempty"`
}

func (s *Server) handleRetentionPreview(w http.ResponseWriter, r *http.Request) {
	plan, err := s.evaluateRetention(r, true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleRetentionApply(w http.ResponseWriter, r *http.Request) {
	var req retentionRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := req.validate(true); err != nil {
		s.writeError(w, r, err)
		return
	}

	// Продолжение не читает тело: оно уже разобрано, а согласование может
	// вызвать его позже — повторным запросом со ссылкой на заявку или само,
	// когда выйдет окно отмены.
	apply := func(w http.ResponseWriter, r *http.Request) {
		plan, err := s.applyRetention(context.WithoutCancel(r.Context()), req)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		s.audit(r, "retention.apply", model.ScopeVM, plan.VMID, true, "")
		writeJSON(w, http.StatusOK, plan)
	}

	s.guardAction(w, r, model.GuardRetentionApply, retentionTarget(req), apply)
}

// validate проверяет запрос ретенции до того, как он попадёт в заявку.
//
// Отдельно от выполнения: заявка на действие, которое всё равно отвергнут,
// зря отнимает у согласующих внимание, а обнаруживается это через сутки.
func (r retentionRequest) validate(requirePlanToken bool) error {
	if r.ServerID == "" || r.VMID == "" || r.StorageTargetID == "" {
		return badRequest("нужны server_id, vm_id и storage_target_id")
	}
	if err := r.Policy.Validate(); err != nil {
		return badRequest("правила хранения: %v", err)
	}
	if requirePlanToken && r.PlanToken == "" {
		return badRequest("сначала постройте план хранения и передайте его plan_token")
	}
	if requirePlanToken {
		if decoded, err := hex.DecodeString(r.PlanToken); err != nil || len(decoded) != sha256.Size {
			return badRequest("plan_token должен быть SHA-256 отпечатком из предпросмотра")
		}
	}
	return nil
}

// applyRetention выполняет ретенцию по разобранному запросу.
func (s *Server) applyRetention(ctx context.Context, req retentionRequest) (retention.Plan, error) {
	return s.engine.ApplyRetentionConfirmed(ctx, req.ServerID, req.VMID, req.StorageTargetID,
		req.Policy, req.PlanToken)
}

func (s *Server) evaluateRetention(r *http.Request, dryRun bool) (retention.Plan, error) {
	var req retentionRequest
	if err := decodeJSON(r, &req); err != nil {
		return retention.Plan{}, err
	}
	if err := req.validate(false); err != nil {
		return retention.Plan{}, err
	}
	return s.engine.ApplyRetention(context.WithoutCancel(r.Context()),
		req.ServerID, req.VMID, req.StorageTargetID, req.Policy, dryRun)
}

// handleBackupOptions returns the offered strategies for one VM, with the
// reasoning behind each. This is the screen an operator lands on before
// creating a job.
func (s *Server) handleBackupOptions(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !srv.Kind.SupportsBackup() {
		s.writeError(w, r, badRequest("резервное копирование %s в этой версии не поддерживается", srv.Kind.Title()))
		return
	}
	if srv.Kind.UsesProxmoxAPI() {
		vm, getErr := s.store.GetVM(r.Context(), srv.ID, r.PathValue("vmID"))
		if getErr != nil {
			s.writeError(w, r, getErr)
			return
		}
		writeJSON(w, http.StatusOK, backup.ProxmoxRecommendation(srv, vm))
		return
	}
	rec, err := s.engine.Recommend(r.Context(), r.PathValue("id"), r.PathValue("vmID"),
		r.URL.Query().Get("storage_target_id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// fillBackupStats adds the backup counters to a server summary.
func (s *Server) fillBackupStats(ctx context.Context, serverID string, summary *model.ServerSummary) error {
	since := time.Now().Add(-24 * time.Hour)
	runs, err := s.store.ListBackupRuns(ctx, store.RunFilter{
		ServerID:       serverID,
		Since:          &since,
		IncludeDeleted: true,
		Limit:          1000,
	})
	if err != nil {
		return err
	}
	summary.BackupsLast24h = len(runs)
	for _, run := range runs {
		if run.Status == model.RunFailed {
			summary.BackupsFailed24h++
		}
	}

	// "Protected" means a VM with at least one usable backup — the number an
	// operator actually cares about when asked "are we covered".
	protected, err := s.store.ListBackupRuns(ctx, store.RunFilter{
		ServerID: serverID,
		Statuses: []model.RunStatus{model.RunSucceeded, model.RunPartial},
		Limit:    5000,
	})
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, run := range protected {
		seen[run.VMID] = true
	}
	summary.ProtectedVMs = len(seen)
	return nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// handleVMLeftovers показывает остатки бэкапов на движке по ВМ, ничего не меняя.
func (s *Server) handleVMLeftovers(w http.ResponseWriter, r *http.Request) {
	rep, err := s.engine.InspectLeftovers(r.Context(), r.PathValue("id"), r.PathValue("vmID"))
	if errors.Is(err, backup.ErrLeftoversUnsupported) {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleCleanupVMLeftovers убирает остатки службы по ВМ по команде оператора.
// Чужое не трогается; блокировки в базе движка не снимаются.
func (s *Server) handleCleanupVMLeftovers(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("vmID")
	res, err := s.engine.CleanupLeftovers(r.Context(), r.PathValue("id"), vmID)
	if errors.Is(err, backup.ErrLeftoversUnsupported) {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if err != nil {
		s.audit(r, "backup.leftovers.cleanup", model.ScopeVM, vmID, false, err.Error())
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	done, failed := 0, 0
	for _, action := range res.Actions {
		if action.OK {
			done++
		} else {
			failed++
		}
	}
	s.audit(r, "backup.leftovers.cleanup", model.ScopeVM, vmID, failed == 0,
		fmt.Sprintf("выполнено действий: %d, не удалось: %d", done, failed))
	writeJSON(w, http.StatusOK, res)
}
