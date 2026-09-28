package discovery

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

var ErrScanRunning = errors.New("поиск сервисов уже выполняется")

type Engine struct {
	store *store.Store
	cfg   config.DiscoveryConfig
	log   zerolog.Logger
	gate  chan struct{}
}

func New(st *store.Store, cfg config.DiscoveryConfig, log zerolog.Logger) *Engine {
	return &Engine{store: st, cfg: cfg, log: log, gate: make(chan struct{}, 1)}
}

func (e *Engine) EffectiveSettings(ctx context.Context) (model.DiscoverySettings, bool, error) {
	stored, overridden, err := e.store.DiscoverySettings(ctx)
	if err != nil {
		return model.DiscoverySettings{}, false, err
	}
	if overridden {
		if len(stored.WebPorts) == 0 {
			stored.WebPorts = append([]int(nil), e.cfg.WebPorts...)
		}
		return stored, true, nil
	}
	defaults := model.DiscoverySettings{WebTargets: append([]string(nil), e.cfg.WebTargets...),
		AddressRanges:         append([]string(nil), e.cfg.WebNetworks...),
		ServerIDs:             append([]string(nil), e.cfg.ServerIDs...),
		WebPorts:              append([]int(nil), e.cfg.WebPorts...),
		ScanAdditionalTargets: e.cfg.ScanAdditionalTargets,
		MaxAddresses:          e.cfg.WebMaxAddresses}
	return defaults, false, defaults.Validate()
}

func (e *Engine) SetSettings(ctx context.Context, settings model.DiscoverySettings, actor string) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	servers, err := e.store.ListServers(ctx)
	if err != nil {
		return err
	}
	if _, err := selectDiscoveryServers(servers, settings.ServerIDs); err != nil {
		return err
	}
	return e.store.SetDiscoverySettings(ctx, settings, actor)
}

func (e *Engine) ResetSettings(ctx context.Context) error {
	return e.store.ResetDiscoverySettings(ctx)
}

func (e *Engine) Run(ctx context.Context) {
	if !e.cfg.Enabled {
		return
	}
	if _, err := e.Scan(ctx); err != nil && !errors.Is(err, context.Canceled) {
		e.log.Error().Err(err).Msg("автоматический поиск сервисов не выполнен")
	}
	ticker := time.NewTicker(e.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := e.Scan(ctx); err != nil {
				e.log.Error().Err(err).Msg("автоматический поиск сервисов не выполнен")
			}
		}
	}
}

func (e *Engine) Scan(ctx context.Context) (*model.DiscoverySnapshot, error) {
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	default:
		return nil, ErrScanRunning
	}
	now := time.Now().UTC()
	scan := &model.DiscoveryScan{ID: uuid.NewString(), Status: model.RunRunning, StartedAt: now, Phase: "Чтение инвентаря"}
	if err := e.store.CreateDiscoveryScan(ctx, scan); err != nil {
		return nil, err
	}
	finish := func(status model.RunStatus, cause error) {
		done := time.Now().UTC()
		scan.Status, scan.CompletedAt = status, &done
		if cause != nil {
			scan.Error = cause.Error()
		}
		if status == model.RunSucceeded || status == model.RunPartial {
			scan.Phase = "Завершено"
		} else {
			scan.Phase = "Ошибка"
		}
		_ = e.store.FinishDiscoveryScan(context.WithoutCancel(ctx), scan)
	}
	settings, _, err := e.EffectiveSettings(ctx)
	if err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	allServers, err := e.store.ListServers(ctx)
	if err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	servers, err := selectDiscoveryServers(allServers, settings.ServerIDs)
	if err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	selectedServerIDs := make(map[string]bool, len(servers))
	for _, server := range servers {
		selectedServerIDs[server.ID] = true
	}
	allVMs, err := e.store.ListVMs(ctx, "")
	if err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	var vms []*model.VM
	for _, vm := range allVMs {
		if selectedServerIDs[vm.ServerID] {
			vms = append(vms, vm)
		}
	}
	scan.VMCount = len(vms)
	if err := e.store.UpdateDiscoveryScanProgress(ctx, scan); err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	hosts, err := e.store.ListHosts(ctx, "")
	if err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	guest, err := newGuestProbe(e.cfg.Guest)
	if err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	scanCfg := e.cfg
	scanCfg.WebTargets, scanCfg.WebNetworks, scanCfg.ServerIDs, scanCfg.WebPorts,
		scanCfg.ScanAdditionalTargets, scanCfg.WebMaxAddresses = settings.WebTargets,
		settings.AddressRanges, settings.ServerIDs, settings.WebPorts,
		settings.ScanAdditionalTargets, settings.MaxAddresses
	virtualizationTargets, err := virtualizationWebTargets(servers, hosts, scanCfg)
	if err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	// Inventory-backed targets are the standard scan. Run and persist them
	// first so a large optional network range cannot hide dynamic VM results.
	services := e.scanVMs(ctx, vms, guest, virtualizationTargets, scanCfg,
		e.phaseProgress(ctx, scan, "Проверка ВМ и платформ виртуализации"))
	if err := e.persistServices(ctx, scan, services); err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	dynamicTargets, truncated := dynamicInventoryTargets(vms, hosts, servers, scanCfg)
	dynamicTargets = webTargetsWithout(dynamicTargets, mergeWebTargets(virtualizationTargets, vmInventoryTargets(vms, scanCfg)))
	if truncated {
		e.log.Warn().Int("предел адресов", scanCfg.WebMaxAddresses).
			Msg("часть автоматически найденных сетей не вошла в область поиска")
	}
	dynamicServices := e.scanVMs(ctx, nil, nil, dynamicTargets, scanCfg,
		e.phaseProgress(ctx, scan, "Сканирование автоматически найденных сетей"))
	if err := e.persistServices(ctx, scan, dynamicServices); err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	services = append(services, dynamicServices...)
	inventoryTargets := mergeWebTargets(virtualizationTargets, vmInventoryTargets(vms, scanCfg), dynamicTargets)
	hostnameTargets := webTargetsWithout(discoveredHostnameTargets(services, scanCfg), inventoryTargets)
	hostnameServices := e.scanVMs(ctx, nil, nil, hostnameTargets, scanCfg,
		e.phaseProgress(ctx, scan, "Проверка DNS-имён из HTTP и TLS"))
	if err := e.persistServices(ctx, scan, hostnameServices); err != nil {
		finish(model.RunFailed, err)
		return nil, err
	}
	services = append(services, hostnameServices...)
	if settings.ScanAdditionalTargets {
		externalTargets, err := expandWebTargets(scanCfg)
		if err != nil {
			finish(model.RunFailed, err)
			return nil, err
		}
		excluded := mergeWebTargets(inventoryTargets, hostnameTargets)
		additional := e.scanVMs(ctx, nil, nil, webTargetsWithout(externalTargets, excluded), scanCfg,
			e.phaseProgress(ctx, scan, "Сканирование дополнительных целей"))
		if err := e.persistServices(ctx, scan, additional); err != nil {
			finish(model.RunFailed, err)
			return nil, err
		}
		services = append(services, additional...)
		additionalHostnameTargets := webTargetsWithout(discoveredHostnameTargets(additional, scanCfg),
			mergeWebTargets(excluded, externalTargets))
		additionalHostnameServices := e.scanVMs(ctx, nil, nil, additionalHostnameTargets, scanCfg,
			e.phaseProgress(ctx, scan, "Проверка DNS-имён дополнительных целей"))
		if err := e.persistServices(ctx, scan, additionalHostnameServices); err != nil {
			finish(model.RunFailed, err)
			return nil, err
		}
		services = append(services, additionalHostnameServices...)
	}
	backupProgress := e.phaseProgress(ctx, scan, "Поиск существующих резервных копий")
	backupProgress(0, 1)
	backups, backupErr := e.scanBackups(ctx, scan.ID, services)
	backupProgress(1, 1)
	for _, backup := range backups {
		if err := e.store.AddDiscoveredBackup(ctx, backup); err != nil {
			finish(model.RunFailed, err)
			return nil, err
		}
		objectID := "discovery:" + backup.Path
		if backup.Stale {
			_ = e.store.RaiseAlert(ctx, &model.Alert{Scope: model.ScopeStorageTarget, ObjectID: objectID,
				ObjectName: backup.Path, Kind: model.AlertBackupStale, Severity: model.SeverityWarning,
				Message: "Копия приложения в BACKUPDATA устарела",
				Details: fmt.Sprintf("Последний файл: %s, изменён %s", backup.LatestObject, backup.LatestAt.Format(time.RFC3339))})
		} else {
			_ = e.store.ResolveAlert(ctx, "", model.ScopeStorageTarget, objectID, model.AlertBackupStale)
		}
	}
	scan.BackupCount = len(backups)
	status := model.RunSucceeded
	if backupErr != nil {
		status = model.RunPartial
	}
	finish(status, backupErr)
	return &model.DiscoverySnapshot{Scan: scan, Services: services, Backups: backups}, nil
}

func (e *Engine) persistServices(ctx context.Context, scan *model.DiscoveryScan, services []*model.DiscoveredService) error {
	for _, service := range services {
		service.ID, service.ScanID = uuid.NewString(), scan.ID
		if err := e.store.AddDiscoveredService(ctx, service); err != nil {
			return err
		}
		scan.ServiceCount++
	}
	return e.store.UpdateDiscoveryScanProgress(ctx, scan)
}

func (e *Engine) phaseProgress(ctx context.Context, scan *model.DiscoveryScan, phase string) func(int, int) {
	var lastWrite time.Time
	return func(completed, total int) {
		scan.Phase, scan.ProbeCompleted, scan.ProbeTotal = phase, completed, total
		if completed == 0 || completed == total || time.Since(lastWrite) >= 500*time.Millisecond {
			_ = e.store.UpdateDiscoveryScanProgress(context.WithoutCancel(ctx), scan)
			lastWrite = time.Now()
		}
	}
}

func scanJobCount(vms []*model.VM, guest *guestProbe, externalTargets []webTarget, scanCfg config.DiscoveryConfig) int {
	total := len(externalTargets)
	for _, vm := range vms {
		if !vm.Running() {
			continue
		}
		addresses := uniqueStrings(vm.IPAddresses)
		if len(addresses) > 0 {
			total += len(addresses) * len(scanCfg.WebPorts)
			if guest != nil {
				total++
			}
		} else if strings.TrimSpace(vm.Name) != "" {
			total += len(scanCfg.WebPorts)
		}
	}
	return total
}

func (e *Engine) scanVMs(ctx context.Context, vms []*model.VM, guest *guestProbe, externalTargets []webTarget, scanCfg config.DiscoveryConfig, progress func(int, int)) []*model.DiscoveredService {
	type result struct{ service *model.DiscoveredService }
	jobs := make(chan func() *model.DiscoveredService)
	results := make(chan result)
	workers := scanCfg.MaxParallel
	if workers < 1 {
		workers = 1
	}
	total := scanJobCount(vms, guest, externalTargets, scanCfg)
	if progress != nil {
		progress(0, total)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				results <- result{job()}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, vm := range vms {
			if !vm.Running() {
				continue
			}
			vm := vm
			addresses := uniqueStrings(vm.IPAddresses)
			for _, address := range addresses {
				address := address
				for _, port := range scanCfg.WebPorts {
					port := port
					jobs <- func() *model.DiscoveredService {
						service, _ := probeWeb(ctx, vm, address, port, scanCfg.WebTimeout)
						return service
					}
				}
			}
			// Some guests do not report an IP. Their inventory name is still a
			// dynamic target when it is resolvable by the service DNS.
			if len(addresses) == 0 && strings.TrimSpace(vm.Name) != "" {
				for _, port := range scanCfg.WebPorts {
					port := port
					jobs <- func() *model.DiscoveredService {
						service, _ := probeWebTarget(ctx, webTarget{VM: vm, Host: vm.Name, Port: port, Source: "web"}, scanCfg.WebTimeout)
						return service
					}
				}
			}
			if guest != nil && len(vm.IPAddresses) > 0 {
				jobs <- func() *model.DiscoveredService { return e.probeGuestVM(ctx, guest, vm) }
			}
		}
		for _, target := range externalTargets {
			target := target
			jobs <- func() *model.DiscoveredService {
				service, _ := probeWebTarget(ctx, target, scanCfg.WebTimeout)
				return service
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	var out []*model.DiscoveredService
	completed := 0
	for item := range results {
		completed++
		if item.service != nil {
			out = append(out, item.service)
		}
		if progress != nil {
			progress(completed, total)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VMName != out[j].VMName {
			return out[i].VMName < out[j].VMName
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (e *Engine) probeGuestVM(ctx context.Context, probe *guestProbe, vm *model.VM) *model.DiscoveredService {
	var report *guestReport
	var address string
	for _, candidate := range uniqueStrings(vm.IPAddresses) {
		r, err := probe.probe(ctx, candidate)
		if err == nil {
			report, address = r, candidate
			break
		}
	}
	if report == nil {
		return nil
	}
	var names, evidence, dataPaths, backupPaths []string
	for _, c := range report.Containers {
		names = append(names, c.Name)
		evidence = append(evidence, c.Image)
	}
	for _, s := range report.Services {
		names = append(names, s.Name)
		evidence = append(evidence, s.Description, s.Exec)
		dataPaths = append(dataPaths, s.DataPaths...)
	}
	for _, m := range report.Mounts {
		if strings.Contains(strings.ToLower(m.Source+" "+m.Target), "backupdata") {
			backupPaths = append(backupPaths, m.Source+" → "+m.Target)
		}
	}
	for _, cron := range report.Cron {
		if strings.Contains(strings.ToLower(cron), "backup") {
			backupPaths = append(backupPaths, cron)
		}
	}
	if len(names) == 0 && len(backupPaths) == 0 {
		return nil
	}
	product, _ := identifyProduct(nil, strings.Join(append(names, evidence...), " "))
	name := strings.Join(uniqueStrings(names), ", ")
	if name == "" {
		name = "Конфигурация резервного копирования"
	}
	return &model.DiscoveredService{ServerID: vm.ServerID, VMID: vm.ID, VMName: vm.Name, Address: address, Hostname: report.Hostname, Hostnames: uniqueStrings([]string{report.Hostname}), Name: name, Product: product, Source: "guest", Evidence: strings.Join(uniqueStrings(evidence), "; "), DataPaths: uniqueStrings(dataPaths), BackupPaths: uniqueStrings(backupPaths), DetectedAt: time.Now().UTC()}
}

func (e *Engine) scanBackups(ctx context.Context, scanID string, services []*model.DiscoveredService) ([]*model.DiscoveredBackup, error) {
	if strings.TrimSpace(e.cfg.BackupStorageTargetID) == "" {
		return nil, nil
	}
	target, err := e.store.GetStorageTarget(ctx, e.cfg.BackupStorageTargetID)
	if err != nil {
		return nil, err
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return nil, err
	}
	defer backend.Close()
	objects, err := backend.List(ctx, e.cfg.BackupPrefix)
	if err != nil {
		return nil, err
	}
	if len(objects) > e.cfg.BackupMaxObjects {
		return nil, fmt.Errorf("в BACKUPDATA %d объектов, предел поиска %d", len(objects), e.cfg.BackupMaxObjects)
	}
	type aggregate struct {
		latest repo.ObjectInfo
		size   int64
	}
	groups := map[string]aggregate{}
	prefix := strings.Trim(strings.ReplaceAll(e.cfg.BackupPrefix, "\\", "/"), "/")
	for _, object := range objects {
		key := strings.Trim(strings.ReplaceAll(object.Key, "\\", "/"), "/")
		if prefix != "" && key != prefix && !strings.HasPrefix(key, prefix+"/") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(key, prefix), "/")
		if rel == "" {
			continue
		}
		folder := strings.Split(rel, "/")[0]
		a := groups[folder]
		a.size += object.Size
		if object.Modified.After(a.latest.Modified) {
			a.latest = object
		}
		groups[folder] = a
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*model.DiscoveredBackup, 0, len(keys))
	for _, folder := range keys {
		a := groups[folder]
		matched := matchService(folder, services)
		out = append(out, &model.DiscoveredBackup{ID: uuid.NewString(), ScanID: scanID, StorageTargetID: target.ID, Path: path.Join(prefix, folder), LatestObject: a.latest.Key, LatestAt: a.latest.Modified, SizeBytes: a.size, MatchedServiceID: matched, DetectedAt: time.Now().UTC()})
		out[len(out)-1].Stale = a.latest.Modified.IsZero() || time.Since(a.latest.Modified) > e.cfg.BackupMaxAge
	}
	return out, nil
}

var nonWord = regexp.MustCompile(`[^a-zа-я0-9]+`)

func matchService(folder string, services []*model.DiscoveredService) string {
	needle := nonWord.ReplaceAllString(strings.ToLower(folder), "")
	if needle == "" {
		return ""
	}
	for _, s := range services {
		candidates := append([]string{s.Name, s.Product, s.Hostname, s.VMName}, s.Hostnames...)
		for _, candidate := range candidates {
			token := nonWord.ReplaceAllString(strings.ToLower(candidate), "")
			if len(token) >= 3 && (strings.Contains(needle, token) || strings.Contains(token, needle)) {
				return s.ID
			}
		}
	}
	return ""
}
