package vmtransfer

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/google/uuid"
)

func (s *Service) PlanBatch(ctx context.Context, r model.VMTransferBatchRequest) (*model.VMTransferBatchPlan, error) {
	p := &model.VMTransferBatchPlan{Items: []*model.VMTransferPlan{}, Blockers: []string{}}
	if len(r.Items) < 1 || len(r.Items) > 100 {
		return nil, fmt.Errorf("выберите от 1 до 100 ВМ")
	}
	if r.Execution != "sequential" && r.Execution != "parallel" {
		return nil, fmt.Errorf("выберите последовательное или параллельное выполнение")
	}
	if r.Execution == "parallel" && (r.Parallelism < 2 || r.Parallelism > 8) {
		return nil, fmt.Errorf("параллелизм должен быть от 2 до 8")
	}
	seen, names := map[string]bool{}, map[string]bool{}
	needed, available := map[string]int64{}, map[string]int64{}
	macs := map[string]bool{}
	for _, item := range r.Items {
		key := item.SourceServerID + ":" + item.SourceVMID
		if seen[key] {
			return nil, fmt.Errorf("исходная ВМ повторяется в группе")
		}
		seen[key] = true
		plan, err := s.Plan(ctx, item)
		if err != nil {
			return nil, err
		}
		p.Items = append(p.Items, plan)
		p.TotalBytes += plan.TotalBytes
		for _, blocker := range plan.Blockers {
			p.Blockers = append(p.Blockers, plan.SourceName+": "+blocker)
		}
		key = item.TargetServerID + ":" + strings.ToLower(strings.TrimSpace(plan.Request.Name))
		if names[key] {
			p.Blockers = append(p.Blockers, "Повторное имя целевой ВМ: "+plan.Request.Name)
		}
		names[key] = true
		if item.PreserveMAC {
			for _, n := range plan.NICs {
				key = item.TargetServerID + ":" + strings.ToLower(n.MAC)
				if macs[key] {
					p.Blockers = append(p.Blockers, "В группе повторяется MAC "+n.MAC)
				}
				macs[key] = true
			}
		}
		key = item.TargetServerID + ":" + item.StorageDomainID
		needed[key] += plan.TotalBytes
		if _, ok := available[key]; !ok {
			available[key] = plan.AvailableBytes
		}
	}
	for key, size := range needed {
		if size > available[key] {
			p.Blockers = append(p.Blockers, "Недостаточно места в целевом домене для всей группы ВМ")
		}
	}
	return p, nil
}

func (s *Service) CreateBatch(ctx context.Context, r model.VMTransferBatchRequest, actor string) (*model.VMTransferBatchResult, error) {
	if !r.Confirm {
		return nil, fmt.Errorf("подтвердите план группы и остановку источников")
	}
	plan, err := s.PlanBatch(ctx, r)
	if err != nil {
		return nil, err
	}
	if len(plan.Blockers) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(plan.Blockers, "; "))
	}
	result := &model.VMTransferBatchResult{ID: uuid.NewString(), Items: []*model.VMTransfer{}, Errors: []string{}}
	parallelism := r.Parallelism
	if r.Execution == "sequential" {
		parallelism = 1
	}
	for i, p := range plan.Items {
		vm, err := s.st.GetVM(ctx, p.Request.SourceServerID, p.Request.SourceVMID)
		if err != nil {
			return nil, err
		}
		p.Request.Confirm = true
		result.Items = append(result.Items, &model.VMTransfer{SourceClusterID: p.SourceClusterID, RecoveryPolicy: p.RecoveryPolicy, ID: uuid.NewString(), BatchID: result.ID, BatchIndex: i, BatchParallelism: parallelism, PendingAction: "prepare", Request: p.Request, SourceName: vm.Name, Status: "queued", Phase: "queued", DiskMap: map[string]string{}, RunIDs: []string{}, TotalBytes: p.TotalBytes, OriginalDesired: vm.DesiredState, OriginalOptOut: vm.RemediationOptOut, WasRunning: vm.Status != "down", CreatedBy: actor})
	}
	if err = s.st.CreateVMTransfers(ctx, result.Items); err != nil {
		return nil, err
	}
	// Copy the response before workers mutate their state.
	response := &model.VMTransferBatchResult{ID: result.ID, Items: []*model.VMTransfer{}, Errors: []string{}}
	for _, t := range result.Items {
		clone := *t
		clone.DiskMap = map[string]string{}
		response.Items = append(response.Items, &clone)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range result.Items {
		if err = s.startLocked(t, "prepare"); err != nil {
			t.Status = "interrupted"
			t.Error = err.Error()
			_ = s.save(t)
			response.Errors = append(response.Errors, t.SourceName+": "+err.Error())
		}
	}
	return response, nil
}

func (s *Service) BatchAction(ctx context.Context, id, action string, confirm bool) (*model.VMTransferBatchResult, error) {
	if !confirm {
		return nil, fmt.Errorf("подтвердите действие для группы")
	}
	switch action {
	case "sync", "cutover", "pause", "resume", "rollback", "rollback_keep", "hold", "complete":
	default:
		return nil, fmt.Errorf("недопустимое действие для группы")
	}
	items, err := s.st.ListVMTransfers(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].BatchIndex < items[j].BatchIndex })
	r := &model.VMTransferBatchResult{ID: id, Items: []*model.VMTransfer{}, Errors: []string{}}
	for _, t := range items {
		if t.BatchID != id {
			continue
		}
		if !batchActionApplies(t, action) {
			r.Skipped = append(r.Skipped, t.SourceName+": действие не требуется в текущем состоянии ("+t.Status+")")
			continue
		}
		updated, e := s.Action(ctx, t.ID, action, true)
		if e != nil {
			r.Errors = append(r.Errors, t.SourceName+": "+e.Error())
		} else {
			r.Items = append(r.Items, updated)
		}
	}
	if len(r.Items) == 0 && len(r.Errors) == 0 && len(r.Skipped) == 0 {
		return nil, fmt.Errorf("в группе нет задач для этого действия")
	}
	return r, nil
}

func batchActionApplies(t *model.VMTransfer, action string) bool {
	live := t.Status == "running" || t.Status == "queued" || t.Status == "recovering"
	switch action {
	case "pause":
		return live || t.Status == "ready"
	case "resume":
		return t.Status == "interrupted" || t.Status == "failed" || t.Status == "blocked" || t.Status == "held"
	case "sync", "cutover":
		return t.Request.Mode == "warm" && t.TargetVMID != "" && t.AppliedRunID != "" && !t.TargetStarted && !t.TargetStartRequested && (t.Status == "ready" || t.Status == "interrupted" || t.Status == "failed")
	case "complete":
		return t.Status == "awaiting_confirmation"
	case "hold", "rollback", "rollback_keep":
		return !live && t.Status != "canceled" && t.Status != "rolled_back" && !(action == "rollback_keep" && t.Status == "rolled_back_retained")
	}
	return false
}
