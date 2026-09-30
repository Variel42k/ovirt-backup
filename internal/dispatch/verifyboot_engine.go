package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Проверка загрузкой в движке oVirt (РЕД Виртуализация).
//
// Вместо отдельного KVM-хоста проверочная ВМ поднимается в самом движке:
// служба восстанавливает копию новой ВМ на выбранный кластер и домен
// хранения — без сетевых интерфейсов, чтобы копия боевой системы не
// встретилась с оригиналом в сети, — запускает её, ждёт, пока движок сообщит
// ответ гостевого агента, и удаляет ВМ вместе с дисками.
//
// Движок обновляет сведения от агента не сразу, а раз в минуту-другую,
// поэтому ожидание по умолчанию дольше, чем на KVM-хосте.

const (
	engineBootDefaultTimeout = 15 * time.Minute
	engineBootPoll           = 10 * time.Second
)

// engineDeleteRetry — пауза перед повторным удалением проверочной ВМ, которую
// движок ещё держит после выключения.
var engineDeleteRetry = 10 * time.Second

// engineBootResult — чем закончился пробный запуск в движке.
type engineBootResult struct {
	Started      bool
	AgentReplied bool
	Status       string
	GuestOS      string
	Hostname     string
	Elapsed      time.Duration
	// Failure — почему проверка не пройдена; пусто — пройдена.
	Failure string
}

func (d *Dispatcher) verifyBootOnEngine(ctx context.Context, req backup.ExternalVerifyRequest) error {
	set, report, opts := req.Set, req.Report, req.Options

	engineSrv, err := d.store.GetServer(ctx, opts.BootEngineID)
	if err != nil {
		return fmt.Errorf("движок для проверочной ВМ: %w", err)
	}
	if !engineSrv.Kind.UsesOVirtAPI() {
		return fmt.Errorf("подключение %q — не движок oVirt: проверочную ВМ в нём не поднять", engineSrv.Name)
	}
	if !engineSrv.Enabled {
		return fmt.Errorf("подключение %q отключено", engineSrv.Name)
	}
	if source, err := d.store.GetServer(ctx, set.Leaf.ServerID); err == nil && !source.Kind.UsesOVirtAPI() {
		return fmt.Errorf("проверка через движок — только для копий ВМ oVirt; копию с %s проверяйте на KVM-хосте",
			source.Kind.Title())
	}

	// Место сверяется по объёму данных и свежим цифрам движка: инвентарь
	// обновляется с задержкой, а заполнить домен боевых ВМ хуже, чем
	// отказаться от проверки.
	data, full, err := d.Engine.RestoreSizeEstimate(ctx, set.Leaf.ID, req.Record.CopyID)
	if err != nil {
		return fmt.Errorf("оценка объёма копии: %w", err)
	}
	domain, err := d.Engine.LiveStorageDomain(ctx, engineSrv, opts.BootStorageDomainID)
	if err != nil {
		return err
	}
	need := data
	if backup.RestoreAllocatesFull(engineSrv.SupportsCBT, domain.Storage) {
		need = full
	}
	space := backup.CheckBootDomain(domain, need, full)
	switch space.Verdict {
	case backup.BootSpaceShort, backup.BootSpaceInactive:
		return fmt.Errorf("домен хранения «%s»: %s", domain.Name, space.Message)
	}
	clusterName := opts.BootClusterID
	if clusters, listErr := d.store.ListClusters(ctx, engineSrv.ID); listErr == nil {
		for _, cluster := range clusters {
			if cluster.ID == opts.BootClusterID {
				clusterName = cluster.Name
				break
			}
		}
	}

	name := verifyVMName(set.Leaf.VMName, req.Record.ID)
	log := d.log.With().Str("verify", req.Record.ID).Str("backup", set.Leaf.ID).
		Str("исходная-вм", set.Leaf.VMName).Str("движок", engineSrv.Name).
		Str("кластер", clusterName).Str("домен", domain.Name).Str("проверочная-вм", name).Logger()

	restoreReq := &model.RestoreVMRequest{
		RunID: set.Leaf.ID, CopyID: req.Record.CopyID, ServerID: engineSrv.ID, Name: name,
		ClusterID: opts.BootClusterID, StorageDomainID: opts.BootStorageDomainID,
		Network: model.RestoreNetworkDetached, Start: true,
		// Неполная копия проверяется как есть: отчёт скажет, что вышло.
		Confirm:   true,
		MemoryMiB: opts.MemoryMiB, VCPUs: opts.VCPUs, SkipCapacityCheck: true,
		// По метке служба узнаёт свою проверочную ВМ среди остатков.
		Description: fmt.Sprintf("%s%s проверочная ВМ пробного запуска из копии %s; служба удаляет её сама",
			model.VerifyVMMarker, req.Record.ID, set.Leaf.ID),
		DiskSuffix: fmt.Sprintf("-verify-%s-%s", set.Leaf.CreatedAt.Format("20060102-1504"), shortID(req.Record.ID)),
	}
	// Ни одного сетевого интерфейса: копия боевой системы не должна
	// встретиться в сети с оригиналом даже отключённой картой.
	if set.RunManifest != nil && set.RunManifest.VMProfile != nil {
		for _, nic := range set.RunManifest.VMProfile.NICs {
			restoreReq.NetworkMappings = append(restoreReq.NetworkMappings,
				model.RestoreVMNetworkMapping{NICID: nic.ID, Exclude: true})
		}
	}
	restore := &model.RestoreRun{
		RunID: set.Leaf.ID, CopyID: req.Record.CopyID, Target: model.RestoreToNewVM, Status: model.RunPending,
		TargetServerID: engineSrv.ID, TargetServerName: engineSrv.Name,
		TargetClusterID: opts.BootClusterID, TargetClusterName: clusterName,
		TargetDomainID: opts.BootStorageDomainID, TargetDomainName: domain.Name, TargetVMName: name,
		Phase: "queued", CreatedAt: time.Now().UTC(),
	}
	if err := d.store.CreateRestoreRun(ctx, restore); err != nil {
		return err
	}
	restoreReq.RestoreID = restore.ID

	// Прогресс сборки ВМ — первые 80 % проверки; ожидание агента — остальное.
	stopProgress := d.followRestoreProgress(ctx, req, restore.ID)
	log.Info().Str("домен-id", domain.ID).Str("тип-хранилища", domain.Storage).
		Int64("свободно", domain.AvailableSize).Int64("нужно", need).
		Str("суффикс-дисков", restoreReq.DiskSuffix).
		Msg("проверка загрузкой через движок: создаю изолированную ВМ и восстанавливаю диски")
	result, err := d.Engine.RestoreVM(ctx, restoreReq)
	stopProgress()
	if err != nil {
		report.Problems = append(report.Problems, "проверочную ВМ не удалось собрать: "+err.Error())
		notes := []string{err.Error()}
		if result != nil {
			notes = append(notes, result.CleanupFailed...)
		}
		report.Summary = "проверочную ВМ не удалось собрать в движке"
		report.Boot = &backup.BootReport{Host: engineSrv.Name, DomainName: name,
			ClusterName: clusterName, StorageDomainName: domain.Name, VMName: name, Notes: notes,
			Stage: backup.BootStageAssembly}
		markDisks(report, set, false, err.Error())
		return nil
	}
	d.Engine.UpdateVerifyPhase(ctx, req.Record, "waiting_guest", 80)

	client, err := d.Engine.OVirtClient(engineSrv)
	if err != nil {
		return err
	}
	timeout := time.Duration(opts.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = engineBootDefaultTimeout
	}
	log.Info().Str("вм-id", result.VMID).Dur("таймаут", timeout).
		Msg("проверочная ВМ создана и запущена, ожидаю ответ гостевого агента")
	boot := waitGuestOnEngine(ctx, client, result.VMID, timeout, engineBootPoll)

	var notes []string
	if space.Verdict == backup.BootSpaceTight {
		notes = append(notes, space.Message)
	}
	if opts.DiskID != "" {
		notes = append(notes, "в движке восстанавливаются все диски ВМ: выбор диска для запуска не применяется")
	}
	kept := boot.Failure != "" && opts.KeepOnFailure
	if kept {
		notes = append(notes, fmt.Sprintf("проверочная ВМ %s оставлена в движке для разбора — удалите её вместе "+
			"с дисками вручную", name))
	} else {
		d.Engine.UpdateVerifyPhase(ctx, req.Record, "cleanup", 95)
		notes = append(notes, d.removeEngineVerifyVM(ctx, client, result.VMID, name)...)
	}
	notes = append(notes, fmt.Sprintf("движок «%s», кластер «%s», домен хранения «%s», сеть не подключалась",
		engineSrv.Name, clusterName, domain.Name))

	passed := boot.Failure == ""
	summary := "ОС загрузилась: гостевой агент ответил"
	if !passed {
		summary = boot.Failure
		report.Problems = append(report.Problems, boot.Failure)
	}
	markDisks(report, set, passed, boot.Failure)
	report.Summary = fmt.Sprintf("%s (движок %s, ВМ %s)", summary, engineSrv.Name, name)
	report.Boot = &backup.BootReport{
		Host: engineSrv.Name, DomainName: name, Started: boot.Started, AgentReplied: boot.AgentReplied,
		ClusterName: clusterName, StorageDomainName: domain.Name, VMName: name,
		Elapsed: boot.Elapsed.Round(time.Second).String(), GuestOS: boot.GuestOS, Hostname: boot.Hostname,
		ImageBytes: data, Notes: notes,
	}
	log.Info().Bool("загрузилась", passed).Dur("ожидание", boot.Elapsed).Msg("проверка загрузкой через движок завершена")
	return nil
}

// waitGuestOnEngine ждёт, пока движок сообщит ответ гостевого агента
// проверочной ВМ, или пока не станет ясно, что ответа не будет.
func waitGuestOnEngine(ctx context.Context, client *ovirt.Client, vmID string, timeout, poll time.Duration) engineBootResult {
	started := time.Now()
	deadline := started.Add(timeout)
	var res engineBootResult
	for {
		vm, err := client.GetVMState(ctx, vmID)
		if err == nil {
			res.Status = vm.Status
			switch vm.Status {
			case "up", "powering_up", "reboot_in_progress":
				res.Started = true
				if vm.GuestOperatingSystem != nil || vm.Fqdn != "" {
					res.AgentReplied = true
					res.Hostname = vm.Fqdn
					if os := vm.GuestOperatingSystem; os != nil {
						res.GuestOS = strings.TrimSpace(os.Distribution + " " + os.Version.FullVersion)
					}
					res.Elapsed = time.Since(started)
					return res
				}
			case "paused":
				res.Elapsed = time.Since(started)
				res.Failure = fmt.Sprintf("проверочная ВМ встала на паузу (%s): гость не смог работать с дисками",
					firstNonEmptyString(vm.StatusDetail, "причина не сообщена"))
				return res
			case "down":
				if res.Started {
					res.Elapsed = time.Since(started)
					res.Failure = "проверочная ВМ выключилась сама: гость упал или завершил работу при загрузке"
					return res
				}
			}
		}
		if time.Now().After(deadline) {
			res.Elapsed = time.Since(started)
			if res.Started {
				res.Failure = fmt.Sprintf("ВМ запустилась, но гостевой агент не ответил за %s: ОС могла не "+
					"загрузиться, либо в госте нет qemu-guest-agent", timeout)
			} else {
				res.Failure = fmt.Sprintf("движок не запустил проверочную ВМ за %s (состояние: %s)",
					timeout, firstNonEmptyString(res.Status, "неизвестно"))
			}
			return res
		}
		select {
		case <-ctx.Done():
			res.Elapsed = time.Since(started)
			res.Failure = "проверка прервана: " + ctx.Err().Error()
			return res
		case <-time.After(poll):
		}
	}
}

// removeEngineVerifyVM выключает и удаляет проверочную ВМ вместе с дисками.
// Возвращает то, что убрать не удалось: остаток в боевом движке должен быть
// назван в отчёте.
func (d *Dispatcher) removeEngineVerifyVM(ctx context.Context, client *ovirt.Client, vmID, name string) []string {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	defer cancel()
	d.log.Info().Str("вм", name).Str("вм-id", vmID).Msg("удаляю проверочную ВМ вместе с дисками")
	if status, _, err := client.VMStatus(cleanupCtx, vmID); err == nil && status != "down" {
		_ = client.StopVM(cleanupCtx, vmID)
		if _, err := client.WaitVMStatus(cleanupCtx, vmID, []string{"down"}, 5*time.Minute); err != nil {
			d.log.Warn().Err(err).Str("вм", name).Msg("проверочная ВМ не выключилась вовремя")
		}
	}
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if err = client.DeleteVM(cleanupCtx, vmID, false); err == nil || ovirt.IsNotFound(err) {
			d.log.Info().Str("вм", name).Str("вм-id", vmID).Msg("проверочная ВМ и её диски удалены")
			return nil
		}
		if !ovirt.IsConflict(err) {
			break
		}
		// Движок ещё держит ВМ после выключения: повторить чуть позже.
		select {
		case <-cleanupCtx.Done():
			err = cleanupCtx.Err()
		case <-time.After(engineDeleteRetry):
			continue
		}
		break
	}
	d.log.Error().Err(err).Str("вм", name).Msg("проверочная ВМ осталась в движке")
	return []string{fmt.Sprintf("проверочную ВМ %s (%s) удалить не удалось: %v — удалите её вместе с дисками вручную",
		name, vmID, err)}
}

// followRestoreProgress переносит прогресс сборки проверочной ВМ в прогресс
// проверки: сборка — первые 80 %.
func (d *Dispatcher) followRestoreProgress(ctx context.Context, req backup.ExternalVerifyRequest, restoreID string) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	lastLogStep := -1
	publish := func() {
		if restore, err := d.store.GetRestoreRun(ctx, restoreID); err == nil {
			d.Engine.UpdateVerifyTransfer(ctx, req.Record, restore.Phase, restore.Progress*80/100,
				restore.TransferredBytes, restore.TotalBytes, restore.BytesPerSecond)
			if restore.TotalBytes > 0 {
				pct := int(restore.TransferredBytes * 100 / restore.TotalBytes)
				if step := pct / 10; step > lastLogStep {
					d.log.Info().Str("verify", req.Record.ID).Str("restore", restore.ID).
						Str("этап", restore.Phase).Int("процент", pct).
						Int64("передано", restore.TransferredBytes).Int64("всего", restore.TotalBytes).
						Int64("байт-в-секунду", restore.BytesPerSecond).
						Str("движок", restore.TargetServerName).Str("кластер", restore.TargetClusterName).
						Str("домен", restore.TargetDomainName).
						Msg("образ передаётся на площадку проверки через ImageIO")
					lastLogStep = step
				}
			}
		}
	}
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			publish()
		}
	}()
	return func() {
		close(done)
		<-stopped
		publish()
	}
}

// markDisks отмечает в отчёте все диски копии одним итогом: в движке ВМ
// загружается целиком, и отделить диск от диска нельзя.
func markDisks(report *backup.VerifyReport, set *backup.ChainSet, ok bool, problem string) {
	for _, id := range set.DiskOrder {
		chain := set.Manifests[id]
		if len(chain) == 0 {
			continue
		}
		leaf := chain[len(chain)-1]
		entry := backup.DiskReport{DiskID: id, Alias: leaf.Alias, OK: ok}
		if !ok && problem != "" {
			entry.Problems = append(entry.Problems, problem)
		}
		report.Disks = append(report.Disks, entry)
	}
}

// verifyVMName — имя проверочной ВМ: по нему её видно в движке, если убрать
// не удалось. Из имени исходной ВМ остаются только символы, которые движок
// принимает в именах везде.
func verifyVMName(vmName, verifyID string) string {
	var b strings.Builder
	for _, r := range vmName {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)), r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	base := strings.Trim(b.String(), "-.")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-.")
	}
	if base == "" {
		base = "vm"
	}
	return fmt.Sprintf("jhv-verify-%s-%s", base, shortID(verifyID))
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
