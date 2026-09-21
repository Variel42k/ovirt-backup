package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/auditlog"
	"github.com/Variel42k/ovirt-backup/internal/dbdump"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/scheduler"
)

// Логические дампы СУБД. Права — существующие: подключение хоста — как
// подключение гипервизора (servers.admin), задания — jobs.*, точки —
// backups.*. Отдельный раздел прав дал бы администратору ещё одну галочку,
// не закрыв ничего нового.

type dbHostPayload struct {
	Name            string         `json:"name"`
	Address         string         `json:"address"`
	Port            int            `json:"port"`
	Username        string         `json:"username"`
	PrivateKey      string         `json:"private_key"`
	HostKey         string         `json:"host_key"`
	TrustAnyHostKey bool           `json:"trust_any_host_key"`
	ServerID        string         `json:"server_id"`
	VMID            string         `json:"vm_id"`
	MonitorEngine   model.DBEngine `json:"monitor_engine"`
}

func (p dbHostPayload) apply(h *model.DBHost) {
	h.Name = strings.TrimSpace(p.Name)
	h.Address = strings.TrimSpace(p.Address)
	h.Port = p.Port
	h.Username = strings.TrimSpace(p.Username)
	if key := strings.TrimSpace(p.PrivateKey); key != "" {
		h.PrivateKey = key + "\n"
	}
	h.HostKey = strings.TrimSpace(p.HostKey)
	h.TrustAnyHostKey = p.TrustAnyHostKey && h.HostKey == ""
	h.ServerID, h.VMID, h.MonitorEngine = strings.TrimSpace(p.ServerID), strings.TrimSpace(p.VMID), p.MonitorEngine
}

func (s *Server) validateDBHost(ctx context.Context, host *model.DBHost) error {
	if err := host.Validate(); err != nil {
		return err
	}
	if host.VMID == "" {
		return nil
	}
	if _, err := s.store.GetServer(ctx, host.ServerID); err != nil {
		return fmt.Errorf("подключение виртуализации для мониторинга не найдено")
	}
	if _, err := s.store.GetVM(ctx, host.ServerID, host.VMID); err != nil {
		return fmt.Errorf("ВМ для мониторинга не найдена в инвентаре")
	}
	return nil
}

func (s *Server) requireDBDump() error {
	if s.dbDump == nil {
		return errors.New("движок дампов СУБД недоступен")
	}
	return nil
}

func (s *Server) handleListDBHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListDBHosts(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, hosts)
}

func (s *Server) handleCreateDBHost(w http.ResponseWriter, r *http.Request) {
	var payload dbHostPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	host := &model.DBHost{}
	payload.apply(host)
	if err := s.validateDBHost(r.Context(), host); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if err := s.store.CreateDBHost(r.Context(), host); err != nil {
		s.audit(r, "db_host.create", model.ScopeServer, host.Name, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "db_host.create", model.ScopeServer, host.ID, true, host.Name)
	s.auditHostKeyTrust(r, model.ScopeServer, host.ID, host.Name, host.TrustAnyHostKey, true)
	writeJSON(w, http.StatusCreated, host)
}

func (s *Server) handleUpdateDBHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.store.GetDBHost(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var payload dbHostPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.apply(host)
	if err := s.validateDBHost(r.Context(), host); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if err := s.store.UpdateDBHost(r.Context(), host); err != nil {
		s.audit(r, "db_host.update", model.ScopeServer, host.ID, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "db_host.update", model.ScopeServer, host.ID, true, host.Name)
	s.auditHostKeyTrust(r, model.ScopeServer, host.ID, host.Name, host.TrustAnyHostKey, true)
	writeJSON(w, http.StatusOK, host)
}

func (s *Server) handleDeleteDBHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteDBHost(r.Context(), id); err != nil {
		s.audit(r, "db_host.delete", model.ScopeServer, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "db_host.delete", model.ScopeServer, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type dbHostProbeResponse struct {
	Host           *model.DBHost             `json:"host"`
	Engines        []model.DBEngineInfo      `json:"engines"`
	Errors         map[model.DBEngine]string `json:"errors,omitempty"`
	RestoreEnabled bool                      `json:"restore_enabled"`
}

func (s *Server) handleProbeDBHost(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDBDump(); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	host, res, err := s.dbDump.Probe(r.Context(), id)
	if err != nil {
		s.audit(r, "db_host.probe", model.ScopeServer, id, false, err.Error())
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	s.audit(r, "db_host.probe", model.ScopeServer, id, true, host.Name)
	writeJSON(w, http.StatusOK, dbHostProbeResponse{Host: host, Engines: res.Engines, Errors: res.Errors,
		RestoreEnabled: res.RestoreEnabled})
}

func (s *Server) handleListDBHostDatabases(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDBDump(); err != nil {
		s.writeError(w, r, err)
		return
	}
	engine := model.DBEngine(r.URL.Query().Get("engine"))
	if !engine.Valid() {
		s.writeError(w, r, badRequest("укажите engine=postgresql или engine=mysql"))
		return
	}
	names, err := s.dbDump.ListDatabases(r.Context(), r.PathValue("id"), engine)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	writeList(w, names)
}

type dbDumpJobPayload struct {
	Name             string                `json:"name"`
	Enabled          *bool                 `json:"enabled"`
	HostID           string                `json:"host_id"`
	Engine           model.DBEngine        `json:"engine"`
	Databases        []string              `json:"databases"`
	IncludeGlobals   bool                  `json:"include_globals"`
	StorageTargetIDs []string              `json:"storage_target_ids"`
	Encrypt          *bool                 `json:"encrypt"`
	VerifyAfter      *bool                 `json:"verify_after"`
	Schedule         string                `json:"schedule"`
	Retention        model.RetentionPolicy `json:"retention"`
}

func (p dbDumpJobPayload) apply(j *model.DBDumpJob) {
	j.Name = strings.TrimSpace(p.Name)
	j.HostID = strings.TrimSpace(p.HostID)
	j.Engine = p.Engine
	j.Databases = trimNonEmpty(p.Databases)
	j.IncludeGlobals = p.IncludeGlobals
	j.StorageTargetIDs = trimNonEmpty(p.StorageTargetIDs)
	if p.Encrypt != nil {
		j.Encrypt = *p.Encrypt
	}
	if p.VerifyAfter != nil {
		j.VerifyAfter = *p.VerifyAfter
	}
	j.Schedule = strings.TrimSpace(p.Schedule)
	j.Retention = p.Retention
	if p.Enabled != nil {
		j.Enabled = *p.Enabled
	}
}

func (s *Server) validateDBDumpJob(ctx context.Context, job *model.DBDumpJob) error {
	if err := job.Validate(); err != nil {
		return badRequest("%v", err)
	}
	if _, err := s.store.GetDBHost(ctx, job.HostID); err != nil {
		return badRequest("хост СУБД %s не найден", job.HostID)
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
		location := s.cfg.Location()
		if s.scheduler != nil {
			location = s.scheduler.Location()
		}
		if _, err := scheduler.ValidateSchedule(job.Schedule, location); err != nil {
			return badRequest("%v", err)
		}
	}
	return nil
}

func (s *Server) reloadSchedule(r *http.Request) {
	if s.scheduler == nil {
		return
	}
	if err := s.scheduler.Reload(r.Context()); err != nil {
		s.log.Warn().Err(err).Msg("не удалось перечитать расписание дампов СУБД")
	}
}

func (s *Server) handleListDBDumpJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListDBDumpJobs(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, jobs)
}

func (s *Server) handleCreateDBDumpJob(w http.ResponseWriter, r *http.Request) {
	var payload dbDumpJobPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	// Шифрование по умолчанию включено: дамп — это все данные базы открытым
	// текстом, в отличие от образа диска с файловой системой поверх.
	job := &model.DBDumpJob{Enabled: true, Encrypt: true, VerifyAfter: true}
	payload.apply(job)
	if job.Retention.Empty() {
		job.Retention = model.DefaultRetention()
	}
	if err := s.validateDBDumpJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.CreateDBDumpJob(r.Context(), job); err != nil {
		s.audit(r, "db_dump.job.create", model.ScopeBackup, job.Name, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "db_dump.job.create", model.ScopeBackup, job.ID, true, job.Name)
	s.reloadSchedule(r)
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleUpdateDBDumpJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetDBDumpJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var payload dbDumpJobPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.apply(job)
	if err := s.validateDBDumpJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.UpdateDBDumpJob(r.Context(), job); err != nil {
		s.audit(r, "db_dump.job.update", model.ScopeBackup, job.ID, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "db_dump.job.update", model.ScopeBackup, job.ID, true, job.Name)
	s.reloadSchedule(r)
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleDeleteDBDumpJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	runs, err := s.store.ListDBDumpRuns(r.Context(), id, 1)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(runs) != 0 {
		s.writeError(w, r, badRequest("у задания есть точки восстановления: сначала удалите их или дождитесь ретенции"))
		return
	}
	if err := s.store.DeleteDBDumpJob(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "db_dump.job.delete", model.ScopeBackup, id, true, "")
	s.reloadSchedule(r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRunDBDumpJob(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDBDump(); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	run, err := s.dbDump.Start(r.Context(), id)
	if err != nil {
		s.audit(r, "db_dump.run", model.ScopeBackup, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "db_dump.run", model.ScopeBackup, id, true, run.ID)
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleListDBDumpRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListDBDumpRuns(r.Context(), r.URL.Query().Get("job_id"), queryInt(r, "limit", 100))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, runs)
}

func (s *Server) handleGetDBDumpRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetDBDumpRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// dbDumpRunTarget описывает удаление точки для заявки на согласование.
func (s *Server) dbDumpRunTarget(r *http.Request) guardedTarget {
	id := r.PathValue("id")
	what := guardedTarget{ID: id, Name: id, Summary: "удаление дампа СУБД " + id}
	if run, err := s.store.GetDBDumpRun(r.Context(), id); err == nil {
		name := run.JobID
		if job, err := s.store.GetDBDumpJob(r.Context(), run.JobID); err == nil {
			name = job.Name
		}
		what.Name = name
		what.Summary = fmt.Sprintf("удаление дампа СУБД задания «%s» от %s, баз: %d",
			name, run.CreatedAt.Format("2006-01-02 15:04 MST"), len(run.Entries))
	}
	return what
}

func (s *Server) handleDeleteDBDumpRun(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDBDump(); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	if err := s.dbDump.DeleteRun(r.Context(), id); err != nil {
		s.audit(r, "db_dump.delete", model.ScopeBackup, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "db_dump.delete", model.ScopeBackup, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleVerifyDBDump(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDBDump(); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	if err := s.dbDump.StartVerify(r.Context(), id); err != nil {
		s.audit(r, "db_dump.verify", model.ScopeBackup, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "db_dump.verify", model.ScopeBackup, id, true, "")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "running"})
}

type dbRestorePayload struct {
	Database string `json:"database"`
	NewName  string `json:"new_name"`
}

func (s *Server) handleRestoreDBDump(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDBDump(); err != nil {
		s.writeError(w, r, err)
		return
	}
	var payload dbRestorePayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	req := dbdump.RestoreRequest{RunID: r.PathValue("id"), Database: strings.TrimSpace(payload.Database),
		NewName: strings.TrimSpace(payload.NewName)}
	detail := req.Database + " → " + req.NewName
	actor := "anonymous"
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}
	remoteIP := s.clientIP(r)
	status, err := s.dbDump.StartRestore(r.Context(), req, func(done *dbdump.RestoreStatus) {
		// Итог фиксируется отдельной записью: запрос уже закрыт, а вопрос
		// «чем кончилось восстановление» задают именно журналу аудита.
		outcome := done.Status
		if done.Error != "" {
			outcome += ": " + done.Error
		}
		s.auditDetached(model.AuditEntry{
			Actor: actor, Action: "db_dump.restore.finished", Scope: model.ScopeBackup, ObjectID: req.RunID,
			Success: done.Status == "succeeded", Detail: detail + " — " + outcome, RemoteIP: remoteIP,
		})
	})
	if err != nil {
		s.audit(r, "db_dump.restore", model.ScopeBackup, req.RunID, false, err.Error())
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	s.audit(r, "db_dump.restore", model.ScopeBackup, req.RunID, true, detail)
	writeJSON(w, http.StatusAccepted, status)
}

func (s *Server) handleListDBRestores(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDBDump(); err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, s.dbDump.Restores())
}

// auditDetached пишет событие, которое завершилось уже после ответа на запрос,
// в те же два места, что и audit: базу и файл для внешнего сборщика.
func (s *Server) auditDetached(entry model.AuditEntry) {
	if err := s.store.Audit(context.Background(), entry); err != nil {
		s.log.Debug().Err(err).Str("действие", entry.Action).Msg("не удалось записать событие аудита")
	}
	if err := s.auditFile.Write(auditlog.Entry{
		Actor: entry.Actor, Action: entry.Action, Scope: string(entry.Scope),
		ObjectID: entry.ObjectID, Detail: entry.Detail, Success: entry.Success, RemoteIP: entry.RemoteIP,
	}); err != nil {
		s.log.Error().Err(err).Str("действие", entry.Action).Msg("журнал аудита в файл не записан")
	}
}
