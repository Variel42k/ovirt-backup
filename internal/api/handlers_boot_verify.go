package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Предварительный анализ для проверки загрузкой через движок: куда в движке
// можно поставить проверочную ВМ и хватит ли там места. Домены хранения
// часто заняты боевыми ВМ, поэтому оператор видит оценку до того, как
// выберет домен, а не ошибку проверки ночью.

type bootCluster struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type bootTargetsResponse struct {
	EngineID   string                   `json:"engine_id"`
	EngineName string                   `json:"engine_name"`
	Clusters   []bootCluster            `json:"clusters"`
	Domains    []backup.BootDomainCheck `json:"domains"`
	// NeedData и NeedFull — объём данных и полный размер дисков проверочной
	// ВМ; -1 — неизвестно.
	NeedData int64 `json:"need_data"`
	NeedFull int64 `json:"need_full"`
	// Basis — на чём основана оценка объёма.
	Basis string `json:"basis"`
}

// handleBootTargets: GET /boot-verify/engines/{id}/targets
//
//	?run_id=…&copy_id=… — оценка по данным конкретной копии;
//	?server_id=…&vm_ids=a,b — по дискам ВМ из инвентаря (форма задания);
//	без параметров — только кластеры и место доменов.
func (s *Server) handleBootTargets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	engine, err := s.store.GetServer(ctx, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !engine.Kind.UsesOVirtAPI() {
		s.writeError(w, r, badRequest("проверочную ВМ через движок можно поднять только в oVirt и его производных"))
		return
	}
	resp, err := s.bootTargets(ctx, engine, r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// bootTargets собирает кластеры и домены движка с оценкой места под
// проверочную ВМ; объём берётся по копии (run_id, copy_id) или по ВМ из
// инвентаря (server_id, vm_ids).
func (s *Server) bootTargets(ctx context.Context, engine *model.Server, q url.Values) (bootTargetsResponse, error) {
	resp := bootTargetsResponse{EngineID: engine.ID, EngineName: engine.Name, NeedData: -1, NeedFull: -1,
		Clusters: []bootCluster{}, Domains: []backup.BootDomainCheck{}}

	switch {
	case q.Get("run_id") != "":
		data, full, sizeErr := s.engine.RestoreSizeEstimate(ctx, q.Get("run_id"), q.Get("copy_id"))
		if sizeErr != nil {
			resp.Basis = "объём копии прочитать не удалось: " + sizeErr.Error()
		} else {
			resp.NeedData, resp.NeedFull = data, full
			resp.Basis = "по данным выбранной копии"
		}
	case q.Get("server_id") != "":
		data, full, n, sizeErr := s.vmDiskEstimate(ctx, q.Get("server_id"), splitIDs(q.Get("vm_ids")))
		switch {
		case sizeErr != nil:
			resp.Basis = "диски ВМ прочитать не удалось: " + sizeErr.Error()
		case n == 0:
			resp.Basis = "у выбранных ВМ нет дисков с данными в инвентаре"
		default:
			resp.NeedData, resp.NeedFull = data, full
			resp.Basis = fmt.Sprintf("по самой большой из ВМ задания (всего ВМ: %d), по занятому месту дисков "+
				"в инвентаре — это оценка", n)
		}
	}

	clusters, err := s.store.ListClusters(ctx, engine.ID)
	if err != nil {
		return resp, err
	}
	for _, c := range clusters {
		resp.Clusters = append(resp.Clusters, bootCluster{ID: c.ID, Name: c.Name})
	}
	domains, err := s.store.ListStorageDomains(ctx, engine.ID)
	if err != nil {
		return resp, err
	}
	for _, d := range domains {
		if d.Type != "" && d.Type != "data" {
			continue
		}
		need := resp.NeedData
		if backup.RestoreAllocatesFull(engine.SupportsCBT, d.Storage) {
			// На блочном домене движка без Backup API диск восстанавливается
			// raw и полным: занимает весь свой размер.
			need = resp.NeedFull
		}
		resp.Domains = append(resp.Domains, backup.CheckBootDomain(d, need, resp.NeedFull))
	}
	rank := map[string]int{backup.BootSpaceOK: 0, backup.BootSpaceTight: 1, backup.BootSpaceUnknown: 2,
		backup.BootSpaceShort: 3, backup.BootSpaceInactive: 4}
	sort.SliceStable(resp.Domains, func(i, j int) bool {
		a, b := resp.Domains[i], resp.Domains[j]
		if rank[a.Verdict] != rank[b.Verdict] {
			return rank[a.Verdict] < rank[b.Verdict]
		}
		return a.Available > b.Available
	})
	return resp, nil
}

// vmDiskEstimate — объём самой большой из ВМ по инвентарю: данные (занятое
// место, не больше размера диска) и полный размер дисков. Пустой vmIDs —
// все ВМ подключения. Возвращает и число учтённых ВМ.
func (s *Server) vmDiskEstimate(ctx context.Context, serverID string, vmIDs []string) (int64, int64, int, error) {
	disks, err := s.store.ListDisks(ctx, serverID)
	if err != nil {
		return 0, 0, 0, err
	}
	wanted := map[string]bool{}
	for _, id := range vmIDs {
		wanted[id] = true
	}
	data, full := map[string]int64{}, map[string]int64{}
	for _, d := range disks {
		if (d.ContentType != "" && d.ContentType != "data") || d.StorageType == "lun" || d.Shareable {
			continue
		}
		for _, vmID := range d.VMIDs {
			if len(wanted) > 0 && !wanted[vmID] {
				continue
			}
			// Цепочка qcow2 с метаданными бывает больше диска, а данных в
			// восстановленном диске больше его размера не будет.
			used := d.ActualSize
			if d.ProvisionedSize > 0 && used > d.ProvisionedSize {
				used = d.ProvisionedSize
			}
			data[vmID] += used
			full[vmID] += d.ProvisionedSize
		}
	}
	var maxData, maxFull int64
	for vmID := range full {
		maxData = max(maxData, data[vmID])
		maxFull = max(maxFull, full[vmID])
	}
	return maxData, maxFull, len(full), nil
}

func splitIDs(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
