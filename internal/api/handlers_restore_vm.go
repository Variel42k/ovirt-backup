package api

import (
	"context"
	"net/http"
	"time"

	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/model"
)

// Восстановление машины целиком: предпросмотр плана и запуск сборки.
//
// Разделено на два обращения намеренно. Восстановление создаёт машину и диски
// в боевом движке, занимает домен хранения и длится десятки минут; между
// намерением и действием должен быть шаг, на котором видно объём и
// последствия. Предпросмотр ничего не создаёт и его можно звать сколько угодно.

// restoreVMRequest is the wire form of model.RestoreVMRequest.
type restoreVMRequest struct {
	CopyID          string                          `json:"copy_id"`
	ServerID        string                          `json:"server_id"`
	Name            string                          `json:"name"`
	ClusterID       string                          `json:"cluster_id"`
	StorageDomainID string                          `json:"storage_domain_id"`
	Network         string                          `json:"network"`
	NetworkMappings []model.RestoreVMNetworkMapping `json:"network_mappings"`
	Start           bool                            `json:"start"`
	Confirm         bool                            `json:"confirm"`
}

func (r *restoreVMRequest) toModel(runID string) *model.RestoreVMRequest {
	return &model.RestoreVMRequest{
		RunID:           runID,
		CopyID:          r.CopyID,
		ServerID:        r.ServerID,
		Name:            r.Name,
		ClusterID:       r.ClusterID,
		StorageDomainID: r.StorageDomainID,
		Network:         model.RestoreVMNetwork(r.Network),
		NetworkMappings: r.NetworkMappings,
		Start:           r.Start,
		Confirm:         r.Confirm,
	}
}

// handlePlanRestoreVM returns what a full VM restore would do.
func (s *Server) handlePlanRestoreVM(w http.ResponseWriter, r *http.Request) {
	var req restoreVMRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	plan, err := s.engine.PlanRestoreVM(r.Context(), req.toModel(r.PathValue("id")))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// План с запретами отдаётся кодом 200, а не ошибкой: это полноценный ответ
	// на вопрос «что будет», и в нём перечислено, что именно мешает. Ошибка
	// вместо него заставила бы интерфейс догадываться о причинах.
	writeJSON(w, http.StatusOK, plan)
}

// handleRestoreVM assembles a whole VM from a backup point.
func (s *Server) handleRestoreVM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req restoreVMRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	restoreReq := req.toModel(id)

	// План строится до ответа: невыполнимый запрос должен отказать сразу, а не
	// в фоновой горутине, за которой никто не следит.
	plan, err := s.engine.PlanRestoreVM(r.Context(), restoreReq)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !plan.Ready() {
		s.audit(r, "backup.restore_vm", model.ScopeBackup, id, false, plan.Blockers[0])
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "восстановление невозможно",
			"code":  "restore_blocked",
			"plan":  plan,
		})
		return
	}

	s.audit(r, "backup.restore_vm", model.ScopeBackup, id, true, plan.NewName)
	restore := &model.RestoreRun{
		RunID: id, CopyID: restoreReq.CopyID, Target: model.RestoreToNewVM,
		Status: model.RunPending, TargetServerID: plan.ServerID,
		TargetDomainID: restoreReq.StorageDomainID, TargetVMName: plan.NewName,
		Phase: "queued", CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateRestoreRun(r.Context(), restore); err != nil {
		s.writeError(w, r, err)
		return
	}
	restoreReq.RestoreID = restore.ID
	if s.bus != nil {
		s.bus.Publish(events.Event{Kind: events.KindRestoreRun, ServerID: plan.ServerID,
			ObjectID: restore.ID, Message: "VM restore queued", Payload: restore})
	}

	go func() {
		ctx := context.WithoutCancel(r.Context())
		result, err := s.engine.RestoreVM(ctx, restoreReq)
		if err != nil {
			s.log.Error().Err(err).Str("копия", id).Msg("восстановление машины не выполнено")
		}
		// Остаток в движке называется в журнале отдельно: он требует действий
		// оператора, а не просто фиксирует неудачу.
		if result != nil {
			for _, left := range result.CleanupFailed {
				s.log.Error().Str("копия", id).Msg(left)
			}
		}
		if s.bus != nil {
			latest, _ := s.store.GetRestoreRun(ctx, restore.ID)
			s.bus.Publish(events.Event{Kind: events.KindRestoreRun, ServerID: plan.ServerID,
				ObjectID: restore.ID, Message: "восстановление VM завершено", Payload: latest})
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":     "queued",
		"restore_id": restore.ID,
		"plan":       plan,
		"message":    "сборка машины запущена; следите за прогрессом в истории восстановлений",
	})
}
