package dispatch

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	googleuuid "github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/kvm"
	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// PlanRestoreVM routes full-VM planning to the target platform. The embedded
// engine retains the oVirt path; libvirt adds live pool/network validation.
func (d *Dispatcher) PlanRestoreVM(ctx context.Context, req *model.RestoreVMRequest) (*model.RestoreVMPlan, error) {
	target, err := d.restoreTargetServer(ctx, req)
	if err != nil {
		return nil, err
	}
	if !target.Kind.UsesLibvirt() {
		return d.Engine.PlanRestoreVM(ctx, req)
	}
	return d.planLibvirtRestoreVM(ctx, req, target)
}

func (d *Dispatcher) planLibvirtRestoreVM(ctx context.Context, req *model.RestoreVMRequest,
	target *model.Server) (*model.RestoreVMPlan, error) {
	plan, profile, err := d.Engine.PrepareRestoreVM(ctx, req)
	if err != nil {
		return nil, err
	}
	sourceRun, err := d.store.GetBackupRun(ctx, req.RunID)
	if err != nil {
		return nil, err
	}
	sourceServer, err := d.store.GetServer(ctx, sourceRun.ServerID)
	if err != nil {
		plan.Blockers = append(plan.Blockers, "исходный сервер не найден")
		return plan, nil
	}
	if !sourceServer.Kind.UsesLibvirt() {
		plan.Blockers = append(plan.Blockers, "межплатформенное восстановление не поддерживается")
		return plan, nil
	}
	conn, err := d.libvirt.ForServer(ctx, target)
	if err != nil {
		plan.Blockers = append(plan.Blockers, "целевой KVM-хост недоступен: "+err.Error())
		return plan, nil
	}

	if _, lookupErr := conn.Libvirt().DomainLookupByName(plan.NewName); lookupErr == nil {
		plan.Blockers = append(plan.Blockers, "на целевом сервере уже есть VM с таким именем")
	}
	if profile != nil {
		arch := golibvirt.OptString{profile.Architecture}
		machine := golibvirt.OptString{backup.PortableMachine(profile.Architecture, profile.Machine)}
		virtType := golibvirt.OptString{"kvm"}
		if _, capabilityErr := conn.Libvirt().ConnectGetDomainCapabilities(nil, arch, machine, virtType, 0); capabilityErr != nil {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("хост не поддерживает профиль %s/%s: %v",
				profile.Architecture, machine[0], capabilityErr))
		}
	}

	if len(plan.Disks) > 0 {
		if strings.TrimSpace(req.StorageDomainID) == "" {
			plan.Blockers = append(plan.Blockers, "не выбран целевой storage pool")
		} else if _, pool, poolErr := conn.StoragePool(ctx, req.StorageDomainID); poolErr != nil {
			plan.Blockers = append(plan.Blockers, poolErr.Error())
		} else {
			plan.FreeBytes = pool.Available
			plan.Warnings = withoutPrefix(plan.Warnings, "движок не сообщил свободное место")
			if plan.TotalBytes > pool.Available {
				plan.Blockers = append(plan.Blockers, fmt.Sprintf("storage pool: нужно %s, свободно %s",
					humanBytes(plan.TotalBytes), humanBytes(pool.Available)))
			} else if plan.TotalBytes > 0 && pool.Available-plan.TotalBytes < plan.TotalBytes/10 {
				plan.Warnings = append(plan.Warnings, "после восстановления в storage pool останется менее 10% запаса")
			}
		}
	}
	for _, nic := range plan.NICs {
		if nic.Excluded || nic.TargetID == "" {
			continue
		}
		switch nic.TargetKind {
		case "network":
			if networkErr := conn.NetworkExists(ctx, nic.TargetID); networkErr != nil {
				plan.Blockers = append(plan.Blockers, networkErr.Error())
			}
		case "bridge":
			if bridgeErr := conn.BridgeExists(ctx, nic.TargetID); bridgeErr != nil {
				plan.Blockers = append(plan.Blockers, bridgeErr.Error())
			}
		default:
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("NIC %q: KVM принимает только network или bridge", nic.Name))
		}
	}
	return plan, nil
}

// RestoreVM executes a full restore on the target platform.
func (d *Dispatcher) RestoreVM(ctx context.Context, req *model.RestoreVMRequest) (*backup.RestoreVMResult, error) {
	target, err := d.restoreTargetServer(ctx, req)
	if err != nil {
		return nil, err
	}
	if !target.Kind.UsesLibvirt() {
		return d.Engine.RestoreVM(ctx, req)
	}
	return d.restoreLibvirtVM(ctx, req, target)
}

func (d *Dispatcher) restoreLibvirtVM(ctx context.Context, req *model.RestoreVMRequest,
	target *model.Server) (*backup.RestoreVMResult, error) {
	plan, err := d.planLibvirtRestoreVM(ctx, req, target)
	if err != nil {
		return nil, err
	}
	result := &backup.RestoreVMResult{Plan: plan, VMName: plan.NewName}
	if !plan.Ready() {
		return result, fmt.Errorf("восстановление невозможно: %s", strings.Join(plan.Blockers, "; "))
	}
	record := &model.RestoreRun{
		ID: req.RestoreID, RunID: req.RunID, CopyID: req.CopyID, Target: model.RestoreToNewVM,
		Status: model.RunPending, TargetServerID: plan.ServerID, TargetDomainID: req.StorageDomainID,
		TargetVMName: plan.NewName, Phase: "queued", CreatedAt: time.Now().UTC(),
	}
	if record.ID == "" {
		if err := d.store.CreateRestoreRun(ctx, record); err != nil {
			return nil, err
		}
	} else if existing, getErr := d.store.GetRestoreRun(ctx, record.ID); getErr == nil {
		record = existing
	}
	result.Restore = record
	started := time.Now().UTC()
	record.Status, record.StartedAt, record.Phase, record.Progress = model.RunRunning, &started, "creating_volumes", 3
	_ = d.store.UpdateRestoreRun(ctx, record)
	fail := func(cause error) (*backup.RestoreVMResult, error) {
		ended := time.Now().UTC()
		record.Status, record.Error, record.EndedAt = model.RunFailed, cause.Error(), &ended
		if record.Phase != "rollback" {
			record.Phase = "failed"
		}
		record.CleanupErrors = append([]string(nil), result.CleanupFailed...)
		_ = d.store.UpdateRestoreRun(context.WithoutCancel(ctx), record)
		return result, cause
	}

	conn, err := d.libvirt.ForServer(ctx, target)
	if err != nil {
		return fail(err)
	}
	set, err := d.Engine.LoadVMChainCopy(ctx, req.RunID, req.CopyID)
	if err != nil {
		return fail(err)
	}
	defer set.Close()
	var profile *backup.VMProfile
	if set.RunManifest != nil {
		profile = set.RunManifest.VMProfile
	}
	driver := kvm.NewDriver(conn, kvm.Config{
		ScratchDir: target.ScratchDir, ChunkSize: int64(d.cfg.ChunkSize),
		Compression: d.Engine.Compression(), CompressionLevel: d.cfg.CompressionLevel,
		MaxParallelDisks: 1, RangeRetries: d.cfg.Transfer.RangeRetries,
	}, d.cipher, d.log)

	var (
		volumes []*libvirtx.ManagedVolume
		disks   []kvm.RestoreDisk
		domain  golibvirt.Domain
		defined bool
	)
	rollback := func() {
		record.Phase = "rollback"
		_ = d.store.UpdateRestoreRun(context.WithoutCancel(ctx), record)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		if defined {
			_ = conn.Libvirt().DomainDestroy(domain)
			flags := golibvirt.DomainUndefineManagedSave | golibvirt.DomainUndefineSnapshotsMetadata |
				golibvirt.DomainUndefineNvram | golibvirt.DomainUndefineCheckpointsMetadata | golibvirt.DomainUndefineTpm
			if undefErr := conn.Libvirt().DomainUndefineFlags(domain, flags); undefErr != nil {
				if plainErr := conn.Libvirt().DomainUndefine(domain); plainErr != nil {
					result.CleanupFailed = append(result.CleanupFailed,
						fmt.Sprintf("VM %s осталась на KVM-хосте: %v; %v", plan.NewName, undefErr, plainErr))
				}
			}
		}
		for i := len(volumes) - 1; i >= 0; i-- {
			if deleteErr := conn.DeleteStorageVolume(cleanupCtx, volumes[i]); deleteErr != nil {
				result.CleanupFailed = append(result.CleanupFailed, deleteErr.Error())
			}
		}
	}

	for i, planned := range plan.Disks {
		volumeName := fmt.Sprintf("%s-%02d-%s.raw", plan.NewName, i+1, shortID(record.ID))
		volume, createErr := conn.CreateStorageVolume(ctx, req.StorageDomainID, volumeName, planned.VirtualSize)
		if createErr != nil {
			rollback()
			return fail(createErr)
		}
		volumes = append(volumes, volume)
		reader, readerErr := d.Engine.ReaderFor(set, planned.DiskID)
		if readerErr != nil {
			rollback()
			return fail(readerErr)
		}
		record.Phase = "restoring_disks"
		record.Progress = 5 + i*80/maxInt(len(plan.Disks), 1)
		_ = d.store.UpdateRestoreRun(ctx, record)
		uploadErr := uploadRestoreImage(ctx, driver, reader, volume.Path)
		reader.Close()
		if uploadErr != nil {
			rollback()
			return fail(fmt.Errorf("диск %s: %w", planned.Alias, uploadErr))
		}
		disks = append(disks, kvm.RestoreDisk{Path: volume.Path, DeviceType: volume.DeviceType,
			Target: planned.Target, Bus: planned.Bus, BootOrder: planned.BootOrder})
	}

	nics := make([]kvm.RestoreNIC, 0, len(plan.NICs))
	for _, nic := range plan.NICs {
		if nic.Excluded || nic.TargetID == "" {
			continue
		}
		nics = append(nics, kvm.RestoreNIC{Name: nic.Name, Model: nic.Model,
			TargetKind: nic.TargetKind, TargetID: nic.TargetID, Connected: nic.Connected})
	}
	record.Phase, record.Progress = "defining_vm", 90
	_ = d.store.UpdateRestoreRun(ctx, record)
	domainXML, err := kvm.RestoreDomainXML(kvm.RestoreDomain{Name: plan.NewName, Profile: profile, Disks: disks, NICs: nics})
	if err != nil {
		rollback()
		return fail(err)
	}
	domain, err = conn.Libvirt().DomainDefineXML(domainXML)
	if err != nil {
		rollback()
		return fail(fmt.Errorf("создание VM в libvirt: %w", err))
	}
	defined = true
	result.VMID = googleuuid.UUID(domain.UUID).String()
	record.TargetVMID, record.TargetVMName = result.VMID, plan.NewName
	if plan.Start {
		record.Phase, record.Progress = "starting_vm", 97
		_ = d.store.UpdateRestoreRun(ctx, record)
		if err := conn.Libvirt().DomainCreate(domain); err != nil {
			rollback()
			return fail(fmt.Errorf("запуск восстановленной VM: %w", err))
		}
	}
	ended := time.Now().UTC()
	record.Status, record.Phase, record.Progress, record.EndedAt = model.RunSucceeded, "completed", 100, &ended
	_ = d.store.UpdateRestoreRun(ctx, record)
	return result, nil
}

func (d *Dispatcher) restoreTargetServer(ctx context.Context, req *model.RestoreVMRequest) (*model.Server, error) {
	if req == nil {
		return nil, fmt.Errorf("нет запроса на восстановление")
	}
	run, err := d.store.GetBackupRun(ctx, req.RunID)
	if err != nil {
		return nil, err
	}
	targetID := strings.TrimSpace(req.ServerID)
	if targetID == "" {
		targetID = run.ServerID
	}
	target, err := d.store.GetServer(ctx, targetID)
	if err == store.ErrNotFound {
		return nil, fmt.Errorf("целевой сервер не найден")
	}
	return target, err
}

func uploadRestoreImage(ctx context.Context, driver *kvm.Driver, reader *backup.ChainReader, remote string) error {
	compress := driver.RemoteHasGzip(ctx)
	pr, pw := io.Pipe()
	producer := make(chan error, 1)
	go func() {
		var sink io.WriteCloser = pw
		if compress {
			gz, err := gzip.NewWriterLevel(pw, gzip.BestSpeed)
			if err != nil {
				_ = pw.CloseWithError(err)
				producer <- err
				return
			}
			sink = gz
		}
		_, err := writeImage(ctx, reader, sink, nil)
		if err == nil && compress {
			err = sink.Close()
		}
		_ = pw.CloseWithError(err)
		producer <- err
	}()
	if err := driver.UploadImage(ctx, pr, remote, kvm.UploadOptions{Compressed: compress, KeepOnFailure: true}); err != nil {
		_ = pr.CloseWithError(err)
		<-producer
		return err
	}
	return <-producer
}

func withoutPrefix(values []string, prefix string) []string {
	out := values[:0]
	for _, value := range values {
		if !strings.HasPrefix(value, prefix) {
			out = append(out, value)
		}
	}
	return out
}

func shortID(value string) string {
	value = strings.ReplaceAll(value, "-", "")
	if len(value) > 10 {
		return value[:10]
	}
	if value == "" {
		return "restore"
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
