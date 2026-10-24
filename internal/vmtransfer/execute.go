package vmtransfer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/google/uuid"
)

// Run refreshes standby targets until an operator requests final cutover.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			items, err := s.st.ListVMTransfers(ctx)
			if err != nil {
				continue
			}
			for _, t := range items {
				if t.Status == "ready" && t.Request.Mode == "warm" {
					// Preparing the rest of a group has priority over periodic
					// re-syncs, otherwise a long sequential group can starve.
					waiting := false
					if t.BatchID != "" {
						for _, other := range items {
							if other.BatchID == t.BatchID && (other.Status == "queued" || other.Status == "running") {
								waiting = true
								break
							}
						}
					}
					if waiting {
						continue
					}
					_, _ = s.Action(ctx, t.ID, "sync", true)
				}
			}
		}
	}
}

func (s *Service) stopSource(ctx context.Context, t *model.VMTransfer) error {
	if err := s.phase(t, "stopping_source", 5); err != nil {
		return err
	}
	src, err := s.pool.Get(ctx, t.Request.SourceServerID)
	if err != nil {
		return err
	}
	state, _, err := src.VMStatus(ctx, t.Request.SourceVMID)
	if err != nil {
		return err
	}
	if state != "down" {
		if state != "up" {
			return fmt.Errorf("источник в состоянии %s; требуется штатное выключение", state)
		}
		if err = src.ShutdownVM(ctx, t.Request.SourceVMID); err != nil {
			return err
		}
		if _, err = src.WaitVMStatus(ctx, t.Request.SourceVMID, []string{"down"}, 10*time.Minute); err != nil {
			return err
		}
	}
	t.SourceStopped = true
	return s.save(t)
}

func (s *Service) backup(ctx context.Context, t *model.VMTransfer, incremental bool) (*model.BackupRun, error) {
	typ := model.BackupFull
	if incremental {
		typ = model.BackupIncremental
	}
	var saveErr error
	runner := s.BackupRunner
	if runner == nil {
		runner = s.engine.Execute
	}
	run, err := runner(ctx, backup.RunRequest{ServerID: t.Request.SourceServerID, VMID: t.Request.SourceVMID, StorageTargetID: t.Request.StorageTargetID, Type: typ, TransferID: t.ID, TriggeredBy: "vm-transfer:" + t.ID,
		OnRunCreated: func(r *model.BackupRun) { t.RunIDs = append(t.RunIDs, r.ID); saveErr = s.save(t) },
	})
	if err != nil {
		return run, err
	}
	if saveErr != nil {
		return run, saveErr
	}
	if run == nil || run.Status != model.RunSucceeded || len(run.SkippedDisks) > 0 {
		return run, fmt.Errorf("миграция требует полной успешной копии всех дисков")
	}
	if incremental && (run.Type != model.BackupIncremental) {
		return run, fmt.Errorf("цепочка изменений изменилась: требуется повторная подготовка цели; полный бэкап не применяется вместо финальной дельты")
	}
	_, err = s.engine.Verify(ctx, run.ID, model.VerifyQuick, model.VerifyOptions{})
	return run, err
}

func (s *Service) prepare(ctx context.Context, t *model.VMTransfer) error {
	plan, err := s.plan(ctx, t.Request, t.ID)
	if err != nil {
		return err
	}
	if len(plan.Blockers) > 0 {
		return fmt.Errorf("проверка перед запуском: %s", strings.Join(plan.Blockers, "; "))
	}
	src, err := s.pool.Get(ctx, t.Request.SourceServerID)
	if err != nil {
		return err
	}
	state, _, err := src.VMStatus(ctx, t.Request.SourceVMID)
	if err != nil {
		return err
	}
	t.WasRunning = state == "up"

	shape, err := s.sourceShape(ctx, t)
	if err != nil {
		return err
	}
	t.SourceShape = shape
	if err = s.save(t); err != nil {
		return err
	}
	if err := s.st.SetVMDesiredState(ctx, t.Request.SourceServerID, t.Request.SourceVMID, model.DesiredAsIs, true); err != nil {
		return err
	}
	if t.Request.Mode == "cold" {
		if err := s.stopSource(ctx, t); err != nil {
			return err
		}
	}
	if err := s.phase(t, "backup", 10); err != nil {
		return err
	}
	run, err := s.backup(ctx, t, false)
	if err != nil {
		return err
	}
	if err = s.checkSourceShape(ctx, t); err != nil {
		return err
	}
	if err = s.phase(t, "restoring_target", 30); err != nil {
		return err
	}
	t.RestoreID = uuid.NewString()
	if err = s.save(t); err != nil {
		return err
	}
	mappings := append([]model.RestoreVMNetworkMapping(nil), t.Request.NetworkMappings...)
	for i := range mappings {
		mappings[i].Connected = false
	}
	result, err := s.engine.RestoreVM(ctx, &model.RestoreVMRequest{
		TransferID: t.ID, RestoreID: t.RestoreID, RunID: run.ID, ServerID: t.Request.TargetServerID, ClusterID: t.Request.ClusterID, StorageDomainID: t.Request.StorageDomainID, Name: t.Request.Name,
		Network: model.RestoreNetworkDetached, NetworkMappings: mappings, PreserveMAC: t.Request.PreserveMAC, Description: "jhvirt-transfer:" + t.ID,
		OnVMCreated:    func(id string) error { t.TargetVMID = id; return s.save(t) },
		OnDiskRestored: func(source, target string) error { t.DiskMap[source] = target; return s.save(t) },
	})
	if err != nil {
		return err
	}
	if result == nil || result.VMID == "" || len(t.DiskMap) != run.DiskCount {
		return fmt.Errorf("целевая ВМ или комплект дисков не сохранены")
	}
	t.AppliedRunID = run.ID
	if err = s.save(t); err != nil {
		return err
	}
	if t.Request.Mode == "warm" {
		return s.synchronize(ctx, t, false)
	}
	return s.startTarget(ctx, t)
}

func (s *Service) synchronize(ctx context.Context, t *model.VMTransfer, final bool) error {
	started := time.Now()
	if err := s.checkSourceShape(ctx, t); err != nil {
		return err
	}
	dst, err := s.pool.Get(ctx, t.Request.TargetServerID)
	if err != nil {
		return err
	}
	state, _, err := dst.VMStatus(ctx, t.TargetVMID)
	if err != nil {
		return err
	}
	if state != "down" {
		return fmt.Errorf("целевая ВМ должна быть выключена, сейчас %s", state)
	}
	if err = s.phase(t, "copying_changes", 65); err != nil {
		return err
	}
	run, err := s.backup(ctx, t, true)
	if err != nil {
		return err
	}
	if err = s.checkSourceShape(ctx, t); err != nil {
		return err
	}
	set, err := s.engine.LoadChain(ctx, run.ID)
	if err != nil {
		return err
	}
	if len(set.DiskOrder) != len(t.DiskMap) {
		set.Close()
		return fmt.Errorf("состав дисков изменился во время миграции")
	}
	var deltaTotal int64
	// Validate the entire set before changing even the first target disk.
	for _, id := range set.DiskOrder {
		targetID := t.DiskMap[id]
		if targetID == "" {
			set.Close()
			return fmt.Errorf("нет соответствия целевому диску %s", id)
		}
		chain := set.Manifests[id]
		reader, e := backup.NewChainReader(set.Backend, nil, chain)
		if e != nil {
			set.Close()
			return e
		}
		indices, deltaErr := reader.ChangedSince(t.AppliedRunID)
		e = deltaErr
		for _, index := range indices {
			deltaTotal += reader.ChunkLength(index)
		}
		reader.Close()
		if e != nil {
			set.Close()
			return e
		}
		d, e := dst.GetDisk(ctx, targetID)
		if e != nil {
			set.Close()
			return e
		}
		if d.ProvisionedSize.Int64() != chain[len(chain)-1].VirtualSize {
			set.Close()
			return fmt.Errorf("размер диска %s изменился", id)
		}
	}
	ids := append([]string(nil), set.DiskOrder...)
	set.Close()
	t.TotalBytes = deltaTotal
	t.TransferredBytes = 0
	var completed int64
	lastSave := time.Now()
	for _, id := range ids {
		var diskDone int64
		_, err = s.engine.Restore(ctx, backup.RestoreRequest{TransferID: t.ID, RunID: run.ID, DiskIDs: []string{id}, Target: model.RestoreToDisk, TargetServerID: t.Request.TargetServerID, TargetDiskID: t.DiskMap[id], DeltaAfterRunID: t.AppliedRunID, TriggeredBy: "vm-transfer:" + t.ID,
			Progress: func(done, total, speed int64) {
				diskDone = done
				t.TransferredBytes = completed + done
				t.BytesPerSecond = speed
				if time.Since(lastSave) > 2*time.Second {
					_ = s.save(t)
					lastSave = time.Now()
				}
			},
		})
		if err != nil {
			return err
		}
		completed += diskDone
	}
	t.TransferredBytes = completed
	t.AppliedRunID = run.ID
	t.LastSyncSeconds = int64(time.Since(started).Seconds())
	t.Status = "ready"
	t.Phase = "ready"
	t.Progress = 85
	t.BytesPerSecond = 0
	if final {
		t.Status = "running"
		t.Phase = "final_delta_applied"
	}
	return s.save(t)
}

func (s *Service) cutover(ctx context.Context, t *model.VMTransfer) error {
	if err := s.stopSource(ctx, t); err != nil {
		return err
	}
	if err := s.synchronize(ctx, t, true); err != nil {
		return err
	}
	return s.startTarget(ctx, t)
}

func (s *Service) startTarget(ctx context.Context, t *model.VMTransfer) error {
	if err := s.checkSourceShape(ctx, t); err != nil {
		return err
	}
	if err := s.phase(t, "starting_target", 90); err != nil {
		return err
	}
	src, err := s.pool.Get(ctx, t.Request.SourceServerID)
	if err != nil {
		return err
	}
	state, _, err := src.VMStatus(ctx, t.Request.SourceVMID)
	if err != nil {
		return err
	}
	if state != "down" {
		return fmt.Errorf("источник не выключен; запуск цели запрещён")
	}
	dst, err := s.pool.Get(ctx, t.Request.TargetServerID)
	if err != nil {
		return err
	}
	nics, err := dst.TransferNICs(ctx, t.TargetVMID)
	if err != nil {
		return err
	}
	// Mapping by source NIC name remains stable even when the destination assigns
	// fresh NIC IDs and MAC addresses.
	set, err := s.engine.LoadVMChainCopy(ctx, t.AppliedRunID, "")
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	if set.RunManifest == nil || set.RunManifest.VMProfile == nil {
		set.Close()
		return fmt.Errorf("не сохранён переносимый профиль ВМ")
	}
	for _, nic := range set.RunManifest.VMProfile.NICs {
		for _, m := range t.Request.NetworkMappings {
			if m.NICID == nic.ID && !m.Exclude {
				wanted[nic.Name] = m.Connected
			}
		}
	}
	set.Close()
	for _, nic := range nics {
		if err = dst.SetTransferNIC(ctx, t.TargetVMID, nic.ID, wanted[nic.Name]); err != nil {
			return err
		}
	}
	if t.Request.HostID != "" {
		hosts, e := dst.ListHosts(ctx, t.Request.TargetServerID)
		if e != nil {
			return e
		}
		available := false
		for _, h := range hosts {
			if h.ID == t.Request.HostID && h.ClusterID == t.Request.ClusterID && h.Status == "up" {
				available = true
			}
		}
		if !available {
			return fmt.Errorf("выбранный целевой хост недоступен; запуск цели остановлен")
		}
	}
	if err = dst.StartVMOnHost(ctx, t.TargetVMID, t.Request.HostID); err != nil {
		return err
	}
	t.TargetStarted = true
	if err = s.save(t); err != nil {
		return err
	}
	if _, err = dst.WaitVMStatus(ctx, t.TargetVMID, []string{"up"}, 10*time.Minute); err != nil {
		return err
	}
	t.Status = "awaiting_confirmation"
	t.Phase = "verify_application"
	t.Progress = 95
	return s.save(t)
}

func (s *Service) rollback(ctx context.Context, t *model.VMTransfer, action string) error {
	if err := s.phase(t, "rolling_back", 5); err != nil {
		return err
	}
	dst, err := s.pool.Get(ctx, t.Request.TargetServerID)
	if err != nil {
		return err
	}
	// Recover a VM created just before a process crash, before its ID was saved.
	if t.TargetVMID == "" {
		vms, e := dst.SearchVMs(ctx, "")
		if e != nil {
			return e
		}
		for _, v := range vms {
			if v.Description == "jhvirt-transfer:"+t.ID {
				t.TargetVMID = v.ID
				break
			}
		}
	}
	if t.TargetVMID != "" {
		state, _, e := dst.VMStatus(ctx, t.TargetVMID)
		if e != nil && !ovirt.IsNotFound(e) {
			return e
		}
		if e == nil {
			if state != "down" {
				if err = dst.ShutdownVM(ctx, t.TargetVMID); err != nil {
					return err
				}
				if _, err = dst.WaitVMStatus(ctx, t.TargetVMID, []string{"down"}, 10*time.Minute); err != nil {
					return err
				}
			}
			if err = dst.DeleteVM(ctx, t.TargetVMID, false); err != nil {
				return err
			}
			// Deletion is asynchronous; do not restart the source until absence is proven.
			if err = dst.WaitTransferVMDeleted(ctx, t.TargetVMID, 5*time.Minute); err != nil {
				return err
			}
		}
	}
	src, err := s.pool.Get(ctx, t.Request.SourceServerID)
	if err != nil {
		return err
	}
	state, _, err := src.VMStatus(ctx, t.Request.SourceVMID)
	if err != nil {
		return err
	}
	if t.WasRunning && state == "down" {
		if err = src.StartVM(ctx, t.Request.SourceVMID); err != nil {
			return err
		}
		if _, err = src.WaitVMStatus(ctx, t.Request.SourceVMID, []string{"up"}, 10*time.Minute); err != nil {
			return err
		}
	}
	if err = s.st.SetVMDesiredState(ctx, t.Request.SourceServerID, t.Request.SourceVMID, t.OriginalDesired, t.OriginalOptOut); err != nil {
		return err
	}
	t.SourceStopped = !t.WasRunning
	t.TargetStarted = false
	t.Status = "rolled_back"
	if action == "cancel" {
		t.Status = "canceled"
	}
	t.Phase = t.Status
	t.Error = ""
	t.Progress = 100
	return s.save(t)
}

func (s *Service) sourceShape(ctx context.Context, t *model.VMTransfer) (string, error) {
	src, err := s.pool.Get(ctx, t.Request.SourceServerID)
	if err != nil {
		return "", err
	}
	raw, err := src.VMConfiguration(ctx, t.Request.SourceVMID)
	if err != nil {
		return "", err
	}
	profile := backup.ProfileFromOVirtConfig([]byte(raw), nil, nil)
	sort.Slice(profile.NICs, func(i, j int) bool { return profile.NICs[i].ID < profile.NICs[j].ID })
	disks, err := src.ListVMDisks(ctx, t.Request.SourceVMID)
	if err != nil {
		return "", err
	}
	type shape struct {
		ID        string
		Size      int64
		Interface string
		Bootable  bool
		Inactive  bool
	}
	ds := make([]shape, 0, len(disks))
	for _, d := range disks {
		ds = append(ds, shape{d.ID, d.ProvisionedSize.Int64(), d.Interface, bool(d.Bootable), d.Inactive})
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].ID < ds[j].ID })
	encoded, err := json.Marshal(struct {
		Profile *backup.VMProfile
		Disks   []shape
	}{profile, ds})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}
func (s *Service) checkSourceShape(ctx context.Context, t *model.VMTransfer) error {
	shape, err := s.sourceShape(ctx, t)
	if err != nil {
		return err
	}
	if shape != t.SourceShape {
		return fmt.Errorf("конфигурация ВМ, сетей или дисков изменилась после начала миграции; требуется повторная подготовка цели")
	}
	return nil
}
