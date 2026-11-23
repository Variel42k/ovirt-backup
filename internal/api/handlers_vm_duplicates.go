package api

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

type duplicateVM struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ServerID    string    `json:"server_id"`
	ServerName  string    `json:"server_name"`
	ClusterID   string    `json:"cluster_id,omitempty"`
	ClusterName string    `json:"cluster_name,omitempty"`
	Status      string    `json:"status"`
	SeenAt      time.Time `json:"seen_at"`
}

func (s *Server) handleVMDuplicates(w http.ResponseWriter, r *http.Request) {
	servers, err := s.store.ListEnabledServers(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	vms, err := s.store.ListVMs(r.Context(), "")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, findVMDuplicates(servers, vms))
}

func findVMDuplicates(servers []*model.Server, vms []*model.VM) []duplicateVM {
	byServer := make(map[string]*model.Server, len(servers))
	for _, server := range servers {
		byServer[server.ID] = server
	}
	type location struct{ serverID, clusterID string }
	type matches struct {
		firstLocation location
		crossCluster  bool
		vms           []duplicateVM
	}
	byName := make(map[string]*matches)
	for _, vm := range vms {
		name := strings.ToLower(strings.TrimSpace(vm.Name))
		server := byServer[vm.ServerID]
		// У автономного подключения KVM свой хост. Для остальных платформ
		// ВМ без сведений о кластере нельзя отнести к месту размещения.
		if name == "" || server == nil || (vm.ClusterID == "" && !server.Kind.UsesLibvirt()) {
			continue
		}
		current := location{vm.ServerID, vm.ClusterID}
		group := byName[name]
		if group == nil {
			group = &matches{firstLocation: current}
			byName[name] = group
		}
		group.crossCluster = group.crossCluster || current != group.firstLocation
		group.vms = append(group.vms, duplicateVM{
			ID: vm.ID, Name: vm.Name, ServerID: vm.ServerID, ServerName: server.Name,
			ClusterID: vm.ClusterID, ClusterName: vm.ClusterName, Status: vm.Status, SeenAt: vm.SeenAt,
		})
	}
	names := make([]string, 0, len(byName))
	for name, group := range byName {
		if group.crossCluster {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	out := make([]duplicateVM, 0)
	for _, name := range names {
		items := byName[name].vms
		slices.SortFunc(items, func(a, b duplicateVM) int {
			return cmp.Or(cmp.Compare(a.ServerName, b.ServerName), cmp.Compare(a.ClusterName, b.ClusterName),
				cmp.Compare(a.ServerID, b.ServerID), cmp.Compare(a.ClusterID, b.ClusterID), cmp.Compare(a.ID, b.ID))
		})
		out = append(out, items...)
	}
	return out
}
