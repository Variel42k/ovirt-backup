package api

import (
	"context"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"net/http"
)

func (s *Server) handleVMTransferSource(w http.ResponseWriter, r *http.Request) {
	serverID, vmID := r.URL.Query().Get("server_id"), r.URL.Query().Get("vm_id")
	server, err := s.store.GetServer(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !server.Kind.UsesOVirtAPI() {
		s.writeError(w, r, badRequest("поддерживаются подключения семейства oVirt"))
		return
	}
	client, err := s.pool.Get(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	nics, err := client.TransferNICs(r.Context(), vmID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	disks, err := client.ListVMDisks(r.Context(), vmID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	warm := server.SupportsCBT && len(disks) > 0
	items := []model.VMTransferNIC{}
	for _, n := range nics {
		items = append(items, model.VMTransferNIC{ID: n.ID, Name: n.Name, MAC: n.MAC.Address})
	}
	for _, d := range disks {
		if d.Backup != "incremental" {
			warm = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nics": items, "warm_available": warm, "warm_reason": "Нужны Backup API и включённый incremental на всех дисках"})
}

func (s *Server) handleListVMTransfers(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListVMTransfers(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}
func (s *Server) handleGetVMTransfer(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.GetVMTransfer(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
func (s *Server) handlePlanVMTransfer(w http.ResponseWriter, r *http.Request) {
	var req model.VMTransferRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.vmTransfers == nil {
		s.writeError(w, r, badRequest("служба миграции не инициализирована"))
		return
	}
	p, err := s.vmTransfers.Plan(r.Context(), req)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	writeJSON(w, http.StatusOK, p)
}
func (s *Server) handleCreateVMTransfer(w http.ResponseWriter, r *http.Request) {
	var req model.VMTransferRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.vmTransfers == nil {
		s.writeError(w, r, badRequest("служба миграции не инициализирована"))
		return
	}
	actor := "api"
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}
	t, err := s.vmTransfers.Create(r.Context(), req, actor)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	s.audit(r, "vm_transfer.create", model.ScopeVM, req.SourceVMID, true, t.ID)
	writeJSON(w, http.StatusAccepted, t)
}
func (s *Server) handleVMTransferAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action  string `json:"action"`
		Confirm bool   `json:"confirm"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.vmTransfers == nil {
		s.writeError(w, r, badRequest("служба миграции не инициализирована"))
		return
	}
	t, err := s.vmTransfers.Action(context.WithoutCancel(r.Context()), r.PathValue("id"), req.Action, req.Confirm)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	s.audit(r, "vm_transfer."+req.Action, model.ScopeVM, t.Request.SourceVMID, true, t.ID)
	writeJSON(w, http.StatusAccepted, t)
}
