package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/proxmox"
)

func (d *Dispatcher) planProxmoxRestoreVM(ctx context.Context, req *model.RestoreVMRequest,
	target *model.Server) (*model.RestoreVMPlan, error) {
	if d.proxmox == nil {
		return nil, errors.New("драйвер Proxmox не инициализирован")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	set, err := d.Engine.LoadVMChainCopy(ctx, req.RunID, req.CopyID)
	if err != nil {
		return nil, err
	}
	defer set.Close()
	plan := &model.RestoreVMPlan{RunID: set.Leaf.ID, VMName: set.Leaf.VMName,
		NewName: strings.TrimSpace(req.Name), ServerID: target.ID, Created: set.Leaf.CreatedAt,
		FreeBytes: -1, Network: req.NetworkOrDefault(), Start: req.Start, Provider: "proxmox"}
	if plan.NewName == "" {
		plan.NewName = model.RestoredVMName(set.Leaf.VMName, time.Now().UTC())
	}
	provider := set.RunManifest
	if provider == nil || provider.Provider == nil || provider.Provider.Name != "proxmox" {
		plan.Blockers = append(plan.Blockers, "нативное восстановление Proxmox возможно только из копии Proxmox")
		return plan, nil
	}
	plan.GuestKind = provider.Provider.GuestKind
	artifact := nativeProxmoxArtifact(provider)
	if artifact == nil {
		plan.Blockers = append(plan.Blockers, "в точке восстановления нет нативного vzdump-архива")
		return plan, nil
	}
	plan.TotalBytes = provider.Provider.ProvisionedBytes
	if plan.TotalBytes == 0 {
		plan.TotalBytes = artifact.SizeBytes
		plan.Warnings = append(plan.Warnings, "виртуальный размер дисков неизвестен; проверка места использует размер сжатого архива")
	}
	plan.Disks = []model.RestoreVMPlanDisk{{DiskID: artifact.DiskID, Alias: "Нативный vzdump-архив",
		Target: "Proxmox storage", VirtualSize: plan.TotalBytes, Bootable: true}}

	storageNode, storageName, storageErr := proxmoxStorage(req.StorageDomainID)
	if storageErr != nil {
		plan.Blockers = append(plan.Blockers, storageErr.Error())
	}
	hosts, listErr := d.store.ListHosts(ctx, target.ID)
	if listErr != nil {
		plan.Blockers = append(plan.Blockers, "не удалось прочитать узлы целевого кластера: "+listErr.Error())
		return plan, nil
	}
	wantedHost := strings.TrimPrefix(strings.TrimSpace(req.HostID), "node/")
	if wantedHost == "" {
		wantedHost = storageNode
	}
	var selected *model.Host
	for _, host := range hosts {
		if wantedHost != "" && host.Name != wantedHost && host.ID != req.HostID {
			continue
		}
		if selected == nil || (!selected.HostHealthy() && host.HostHealthy()) {
			selected = host
		}
	}
	if selected == nil && wantedHost == "" {
		for _, host := range hosts {
			if host.HostHealthy() {
				selected = host
				break
			}
		}
	}
	if selected == nil {
		plan.Blockers = append(plan.Blockers, "не найден доступный целевой узел Proxmox")
	} else {
		plan.HostID, plan.HostName = selected.ID, selected.Name
		if !selected.HostHealthy() {
			plan.Blockers = append(plan.Blockers, "целевой узел Proxmox не находится в состоянии up")
		}
		if storageNode != "" && storageNode != selected.Name {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("локальное хранилище %s принадлежит узлу %s", storageName, storageNode))
		}
	}

	if req.StorageDomainID == "" {
		plan.Blockers = append(plan.Blockers, "не выбрано целевое хранилище Proxmox")
	} else if domains, domainErr := d.store.ListStorageDomains(ctx, target.ID); domainErr == nil {
		found := false
		for _, domain := range domains {
			if domain.ID != req.StorageDomainID {
				continue
			}
			found, plan.FreeBytes = true, domain.AvailableSize
			if domain.Status != "active" {
				plan.Blockers = append(plan.Blockers, "целевое хранилище Proxmox неактивно")
			}
		}
		if !found {
			plan.Blockers = append(plan.Blockers, "целевое хранилище Proxmox не найдено")
		}
	}
	if plan.FreeBytes >= 0 && plan.TotalBytes > plan.FreeBytes {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("в хранилище не хватает места: нужно %s, свободно %s",
			humanBytes(plan.TotalBytes), humanBytes(plan.FreeBytes)))
	}
	if req.Start && plan.Network != model.RestoreNetworkAttached {
		plan.Blockers = append(plan.Blockers, "автозапуск требует явно подключённой сети; безопасный режим detached оставляет гостя выключенным")
	}
	if vms, vmErr := d.store.ListVMs(ctx, target.ID); vmErr == nil {
		for _, vm := range vms {
			if strings.EqualFold(strings.TrimSpace(vm.Name), plan.NewName) {
				plan.Blockers = append(plan.Blockers, "в целевом кластере уже есть гость с таким именем")
				break
			}
		}
	}
	if !target.HasProxmoxDataPlane() {
		plan.Blockers = append(plan.Blockers, "на целевом подключении не настроен SSH-канал данных Proxmox")
	} else if selected != nil && selected.HostHealthy() {
		plane, planeErr := proxmox.NewDataPlane(target, 20*time.Second)
		if planeErr == nil {
			host := selected.Address
			if host == "" {
				host = selected.Name
			}
			if probeErr := plane.Probe(ctx, host); probeErr != nil {
				plan.Blockers = append(plan.Blockers, "канал данных целевого узла недоступен: "+probeErr.Error())
			}
		} else {
			plan.Blockers = append(plan.Blockers, planeErr.Error())
		}
	}
	plan.Warnings = append(plan.Warnings,
		"будет восстановлен нативный архив Proxmox с новыми MAC-адресами",
		"по умолчанию все сетевые интерфейсы восстановленного гостя будут отключены")
	return plan, nil
}

func (d *Dispatcher) restoreProxmoxVM(ctx context.Context, req *model.RestoreVMRequest,
	target *model.Server) (*backup.RestoreVMResult, error) {
	if d.proxmox == nil {
		return nil, errors.New("драйвер Proxmox не инициализирован")
	}
	plan, err := d.planProxmoxRestoreVM(ctx, req, target)
	if err != nil {
		return nil, err
	}
	result := &backup.RestoreVMResult{Plan: plan, VMName: plan.NewName}
	if !plan.Ready() {
		return result, fmt.Errorf("восстановление невозможно: %s", strings.Join(plan.Blockers, "; "))
	}
	record := &model.RestoreRun{ID: req.RestoreID, RunID: req.RunID, CopyID: req.CopyID,
		Target: model.RestoreToNewVM, Status: model.RunPending, TargetServerID: target.ID,
		TargetDomainID: req.StorageDomainID, TargetVMName: plan.NewName, Phase: "queued", CreatedAt: time.Now().UTC()}
	if record.ID == "" {
		if err := d.store.CreateRestoreRun(ctx, record); err != nil {
			return nil, err
		}
	} else if existing, getErr := d.store.GetRestoreRun(ctx, record.ID); getErr == nil {
		record = existing
	}
	result.Restore = record
	started := time.Now().UTC()
	record.Status, record.StartedAt, record.Phase, record.Progress = model.RunRunning, &started, "opening_archive", 3
	_ = d.store.UpdateRestoreRun(ctx, record)
	fail := func(cause error) (*backup.RestoreVMResult, error) {
		ended := time.Now().UTC()
		record.Status, record.Error, record.EndedAt, record.Phase = model.RunFailed, cause.Error(), &ended, "failed"
		record.CleanupErrors = append([]string(nil), result.CleanupFailed...)
		_ = d.store.UpdateRestoreRun(context.WithoutCancel(ctx), record)
		return result, cause
	}

	set, err := d.Engine.LoadVMChainCopy(ctx, req.RunID, req.CopyID)
	if err != nil {
		return fail(err)
	}
	defer set.Close()
	artifact := nativeProxmoxArtifact(set.RunManifest)
	if artifact == nil {
		return fail(fmt.Errorf("нативный архив Proxmox не найден"))
	}
	manifestStream, err := set.Backend.Get(ctx, artifact.ManifestKey)
	if err != nil {
		return fail(err)
	}
	var manifest backup.DiskManifest
	err = backup.DecodeManifest(manifestStream, &manifest)
	closeErr := manifestStream.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fail(err)
	}
	if err := backup.ValidateArtifactManifest(set.Leaf.ID, artifact.DiskID, artifact.Kind, artifact.DataKey, &manifest); err != nil {
		return fail(err)
	}
	reader, err := backup.NewChainReader(set.Backend, d.cipher, []*backup.DiskManifest{&manifest})
	if err != nil {
		return fail(err)
	}
	defer reader.Close()

	client, err := d.proxmox.ForServer(target)
	if err != nil {
		return fail(err)
	}
	plane, err := proxmox.NewDataPlane(target, 30*time.Second)
	if err != nil {
		return fail(err)
	}
	host, err := d.proxmoxNodeAddress(ctx, target.ID, plan.HostID, plan.HostName)
	if err != nil {
		return fail(err)
	}
	_, storage, err := proxmoxStorage(req.StorageDomainID)
	if err != nil {
		return fail(err)
	}

	// nextid is not a reservation. Serialising restores prevents two workers of
	// this service from choosing the same id; Proxmox still rejects a collision
	// caused by an external creator without overwriting anything.
	d.proxmoxRestore.Lock()
	defer d.proxmoxRestore.Unlock()
	vmID, err := client.NextVMID(ctx)
	if err != nil {
		return fail(err)
	}
	result.VMID, record.TargetVMID = vmID, vmID
	record.Phase, record.Progress = "restoring_native_archive", 8
	_ = d.store.UpdateRestoreRun(ctx, record)

	pipeReader, pipeWriter := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		lastUpdate := time.Now()
		err := reader.Stream(ctx, func(_ context.Context, _ int64, data []byte, zeroLength int64) error {
			if data != nil {
				_, writeErr := pipeWriter.Write(data)
				return writeErr
			}
			return writeZeros(pipeWriter, zeroLength)
		}, func(done int64) {
			if time.Since(lastUpdate) < time.Second || reader.VirtualSize() <= 0 {
				return
			}
			record.Progress = 8 + int(done*82/reader.VirtualSize())
			_ = d.store.UpdateRestoreRun(ctx, record)
			lastUpdate = time.Now()
		})
		_ = pipeWriter.CloseWithError(err)
		produced <- err
	}()
	provider := set.RunManifest.Provider
	restoreErr := plane.Restore(ctx, host, provider.GuestKind, vmID, storage,
		plan.NewName, plan.Network, plan.Start, pipeReader)
	_ = pipeReader.CloseWithError(restoreErr)
	producerErr := <-produced
	if restoreErr == nil {
		restoreErr = producerErr
	}
	if restoreErr != nil {
		return fail(restoreErr)
	}
	ended := time.Now().UTC()
	record.Status, record.Phase, record.Progress, record.EndedAt = model.RunSucceeded, "completed", 100, &ended
	_ = d.store.UpdateRestoreRun(ctx, record)
	return result, nil
}

func nativeProxmoxArtifact(doc *backup.RunManifest) *backup.RunManifestArtifact {
	if doc == nil {
		return nil
	}
	for i := range doc.Artifacts {
		if doc.Artifacts[i].Kind == backup.ArtifactProxmoxVZDUMP {
			return &doc.Artifacts[i]
		}
	}
	return nil
}

func proxmoxStorage(id string) (node, storage string, err error) {
	parts := strings.Split(strings.TrimSpace(id), "/")
	switch {
	case len(parts) == 2 && parts[0] == "storage" && proxmox.ValidStorageID(parts[1]):
		return "", parts[1], nil
	case len(parts) == 3 && parts[0] == "storage" && parts[1] != "" && proxmox.ValidStorageID(parts[2]):
		return parts[1], parts[2], nil
	default:
		return "", "", fmt.Errorf("неверный идентификатор хранилища Proxmox %q", id)
	}
}

func writeZeros(w io.Writer, length int64) error {
	zero := make([]byte, 1<<20)
	for length > 0 {
		part := int64(len(zero))
		if length < part {
			part = length
		}
		if _, err := w.Write(zero[:part]); err != nil {
			return err
		}
		length -= part
	}
	return nil
}
