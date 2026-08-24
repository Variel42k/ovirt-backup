package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListClusters(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListHosts(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListVMs(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// Filters are applied here rather than in SQL: the inventory is small and
	// keeping the query layer simple is worth more than the microseconds.
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	status := r.URL.Query().Get("status")
	cluster := r.URL.Query().Get("cluster_id")

	filtered := make([]*model.VM, 0, len(items))
	for _, vm := range items {
		if query != "" && !strings.Contains(strings.ToLower(vm.Name), query) {
			continue
		}
		if status != "" && vm.Status != status {
			continue
		}
		if cluster != "" && vm.ClusterID != cluster {
			continue
		}
		filtered = append(filtered, vm)
	}
	writeList(w, filtered)
}

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	vm, err := s.store.GetVM(r.Context(), r.PathValue("id"), r.PathValue("vmID"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

func (s *Server) handleListVMDisks(w http.ResponseWriter, r *http.Request) {
	disks, err := s.store.ListDisksForVM(r.Context(), r.PathValue("id"), r.PathValue("vmID"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, disks)
}

func (s *Server) handleListDisks(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListDisks(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleListStorageDomains(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	srv, err := s.store.GetServer(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if srv.Kind.UsesLibvirt() {
		conn, connErr := s.libvirt.ForServer(r.Context(), srv)
		if connErr != nil {
			s.writeError(w, r, connErr)
			return
		}
		pools, poolErr := conn.ListStoragePools(r.Context())
		if poolErr != nil {
			s.writeError(w, r, poolErr)
			return
		}
		items := make([]*model.StorageDomain, 0, len(pools))
		for _, pool := range pools {
			items = append(items, &model.StorageDomain{ID: pool.Name, ServerID: serverID,
				Name: pool.Name, Type: "data", Storage: pool.Kind, Status: "active",
				AvailableSize: pool.Available, UsedSize: pool.Allocation,
				CommittedSize: pool.Allocation, SeenAt: time.Now().UTC()})
		}
		writeList(w, items)
		return
	}
	items, err := s.store.ListStorageDomains(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleListRestoreNetworks(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	srv, err := s.store.GetServer(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if srv.Kind.UsesLibvirt() {
		conn, connErr := s.libvirt.ForServer(r.Context(), srv)
		if connErr != nil {
			s.writeError(w, r, connErr)
			return
		}
		networks, networkErr := conn.ListNetworks(r.Context())
		if networkErr != nil {
			s.writeError(w, r, networkErr)
			return
		}
		items := make([]*model.RestoreNetworkTarget, 0, len(networks))
		for _, network := range networks {
			status := "inactive"
			if network.Active {
				status = "active"
			}
			items = append(items, &model.RestoreNetworkTarget{ID: network.Name, ServerID: serverID,
				Name: network.Name, Kind: "network", Network: network.Name, Status: status})
		}
		writeList(w, items)
		return
	}
	client, err := s.pool.Get(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := client.ListVNICProfiles(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

// vmActionRequest is the body of a power-management call.
type vmActionRequest struct {
	// start | shutdown | stop | suspend | reboot | reset | migrate
	Action string `json:"action"`
	HostID string `json:"host_id,omitempty"`
	// Confirm требуется для действий, способных повредить данные в гостевой ОС.
	Confirm bool `json:"confirm"`
}

func (s *Server) handleVMAction(w http.ResponseWriter, r *http.Request) {
	serverID, vmID := r.PathValue("id"), r.PathValue("vmID")

	var req vmActionRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	// Сброс обрывает работу гостя без остановки его ОС — для этого своё право.
	// Проверка здесь, а не на маршруте: адрес у всех действий над ВМ один, и
	// какое из них разрушительное, видно только из тела запроса.
	if model.DisruptiveVMAction(req.Action) && !s.allowedDisruptive(r) {
		s.writeError(w, r, forbiddenDisruptive("сброс виртуальной машины"))
		return
	}

	vm, err := s.store.GetVM(r.Context(), serverID, vmID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	srv, err := s.store.GetServer(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	// "stop" and "reset" cut power under a running guest; requiring an explicit
	// confirmation keeps a mis-click from corrupting a filesystem.
	if (req.Action == "stop" || req.Action == "reset") && !req.Confirm {
		s.writeError(w, r, badRequest(
			"действие %q прерывает работу гостевой ОС и требует подтверждения (confirm=true)", req.Action))
		return
	}

	if srv.Kind.UsesLibvirt() {
		err = s.libvirtVMAction(ctx, srv, vm, req)
		s.audit(r, "vm."+req.Action, model.ScopeVM, vmID, err == nil, vm.Name)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		go s.refreshServer(context.WithoutCancel(r.Context()), serverID)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":  "accepted",
			"message": "команда отправлена гипервизору",
		})
		return
	}

	client, err := s.pool.Get(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	switch req.Action {
	case "start":
		err = client.StartVM(ctx, vmID)
	case "shutdown":
		err = client.ShutdownVM(ctx, vmID)
	case "stop":
		err = client.StopVM(ctx, vmID)
	case "suspend":
		err = client.SuspendVM(ctx, vmID)
	case "reboot":
		err = client.RebootVM(ctx, vmID)
	case "reset":
		err = client.ResetVM(ctx, vmID)
	case "migrate":
		err = client.MigrateVM(ctx, vmID, req.HostID)
	default:
		err = badRequest("неизвестное действие: %q", req.Action)
	}

	s.audit(r, "vm."+req.Action, model.ScopeVM, vmID, err == nil, vm.Name)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// Refresh in the background so the UI sees the new state on its next poll
	// without this call blocking on the engine's state machine.
	go s.refreshServer(context.WithoutCancel(r.Context()), serverID)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "accepted",
		"message": "команда отправлена движку; состояние обновится после её выполнения",
	})
}

// libvirtVMAction performs a power operation on a libvirt domain.
func (s *Server) libvirtVMAction(ctx context.Context, srv *model.Server, vm *model.VM, req vmActionRequest) error {
	conn, err := s.libvirt.ForServer(ctx, srv)
	if err != nil {
		return err
	}
	dom, _, err := conn.DomainByUUID(ctx, vm.ID)
	if err != nil {
		return err
	}

	switch req.Action {
	case "start":
		return conn.StartDomain(ctx, dom)
	case "shutdown":
		return conn.ShutdownDomain(ctx, dom)
	case "stop":
		return conn.DestroyDomain(ctx, dom)
	case "suspend":
		return conn.SuspendDomain(ctx, dom)
	case "reboot":
		return conn.RebootDomain(ctx, dom)
	case "reset":
		return conn.ResetDomain(ctx, dom)
	case "migrate":
		// Live migration needs a destination host and shared storage, neither
		// of which a single bare hypervisor has.
		return badRequest("миграция недоступна для одиночного хоста libvirt: " +
			"переносить ВМ некуда, а общего хранилища у него нет")
	default:
		return badRequest("неизвестное действие: %q", req.Action)
	}
}

// vmPolicyRequest declares what the VM should be doing, which is the input the
// auto-revive engine compares reality against.
type vmPolicyRequest struct {
	DesiredState      model.VMDesiredState `json:"desired_state"`
	RemediationOptOut bool                 `json:"remediation_opt_out"`
}

func (s *Server) handleVMPolicy(w http.ResponseWriter, r *http.Request) {
	serverID, vmID := r.PathValue("id"), r.PathValue("vmID")

	var req vmPolicyRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	switch req.DesiredState {
	case model.DesiredAsIs, model.DesiredUp, model.DesiredDown:
	default:
		s.writeError(w, r, badRequest("недопустимое требуемое состояние: %q", req.DesiredState))
		return
	}

	if err := s.store.SetVMDesiredState(r.Context(), serverID, vmID, req.DesiredState, req.RemediationOptOut); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "vm.policy", model.ScopeVM, vmID, true,
		string(req.DesiredState)+" opt_out="+boolText(req.RemediationOptOut))

	vm, err := s.store.GetVM(r.Context(), serverID, vmID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

type vmTagsRequest struct {
	Tags []string `json:"tags"`
}

func (s *Server) handleVMTags(w http.ResponseWriter, r *http.Request) {
	serverID, vmID := r.PathValue("id"), r.PathValue("vmID")
	var req vmTagsRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(req.Tags) > 64 {
		s.writeError(w, r, badRequest("у ВМ не может быть больше 64 локальных тегов"))
		return
	}
	for _, tag := range req.Tags {
		if len(strings.TrimSpace(tag)) > 128 {
			s.writeError(w, r, badRequest("тег ВМ длиннее 128 символов"))
			return
		}
	}
	if err := s.store.SetVMLocalTags(r.Context(), serverID, vmID, req.Tags); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "vm.tags", model.ScopeVM, vmID, true, strings.Join(req.Tags, ","))
	vm, err := s.store.GetVM(r.Context(), serverID, vmID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, vm)
}

// hostActionRequest is the body of a host management call.
type hostActionRequest struct {
	// activate | deactivate | fence
	Action string `json:"action"`
	// FenceType: start | stop | restart | status
	FenceType string `json:"fence_type,omitempty"`
	Confirm   bool   `json:"confirm"`
}

func (s *Server) handleHostAction(w http.ResponseWriter, r *http.Request) {
	serverID, hostID := r.PathValue("id"), r.PathValue("hostID")

	var req hostActionRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	// Фенсинг уносит все машины на хосте разом — самое разрушительное, что
	// служба умеет. Право на него отдельное от обычного управления.
	if model.DisruptiveHostAction(req.Action, req.FenceType) && !s.allowedDisruptive(r) {
		s.writeError(w, r, forbiddenDisruptive("перезагрузка хоста по питанию"))
		return
	}

	if srv, err := s.store.GetServer(r.Context(), serverID); err == nil && srv.Kind.UsesLibvirt() {
		// There is no engine above a bare libvirt host: maintenance mode and
		// fencing are engine concepts, and the hypervisor's own operating
		// system is what manages it.
		s.writeError(w, r, badRequest(
			"хостом libvirt управляет его операционная система, а не движок — "+
				"действия обслуживания и перезагрузки по питанию отсюда недоступны"))
		return
	}

	client, err := s.pool.Get(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	switch req.Action {
	case "activate":
		err = client.ActivateHost(ctx, hostID)
	case "deactivate":
		err = client.DeactivateHost(ctx, hostID)
	case "fence":
		fenceType := req.FenceType
		if fenceType == "" {
			fenceType = "restart"
		}
		// Fencing a host kills every VM on it. Nothing about that should be
		// possible from a single accidental request.
		if fenceType != "status" && !req.Confirm {
			s.writeError(w, r, badRequest(
				"перезагрузка хоста по питанию остановит все его ВМ и требует подтверждения (confirm=true)"))
			return
		}
		err = client.FenceHost(ctx, hostID, fenceType)
	default:
		err = badRequest("неизвестное действие: %q", req.Action)
	}

	s.audit(r, "host."+req.Action, model.ScopeHost, hostID, err == nil, req.FenceType)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	go s.refreshServer(context.WithoutCancel(r.Context()), serverID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// diskBackupModeRequest toggles changed block tracking on a disk, which is the
// prerequisite for hot incremental backups.
type diskBackupModeRequest struct {
	Incremental bool `json:"incremental"`
}

func (s *Server) handleDiskBackupMode(w http.ResponseWriter, r *http.Request) {
	serverID, diskID := r.PathValue("id"), r.PathValue("diskID")

	var req diskBackupModeRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	if srv, err := s.store.GetServer(r.Context(), serverID); err == nil && srv.Kind.UsesLibvirt() {
		// In libvirt, changed block tracking is not a switch: a bitmap lives
		// inside the qcow2 header, so the format alone decides. Pretending
		// there is a toggle would record a change that changes nothing.
		s.writeError(w, r, badRequest(
			"для libvirt отслеживание изменённых блоков определяется форматом диска: "+
				"оно доступно для qcow2 и невозможно для raw. Отдельно включать нечего"))
		return
	}

	disk, err := s.store.GetDisk(r.Context(), serverID, diskID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Incremental && disk.Format != "cow" {
		s.writeError(w, r, badRequest(
			"диск %s в формате %s: отслеживание изменённых блоков возможно только для qcow2",
			disk.Alias, disk.Format))
		return
	}

	client, err := s.pool.Get(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()

	if err := client.SetDiskBackupMode(ctx, diskID, req.Incremental); err != nil {
		s.audit(r, "disk.backup_mode", model.ScopeVM, diskID, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "disk.backup_mode", model.ScopeVM, diskID, true,
		disk.Alias+" incremental="+boolText(req.Incremental))

	go s.refreshServer(context.WithoutCancel(r.Context()), serverID)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"incremental": req.Incremental,
		"message":     "режим изменён; изменённые блоки начнут отслеживаться со следующего полного бэкапа",
	})
}

func boolText(v bool) string {
	if v {
		return "да"
	}
	return "нет"
}
