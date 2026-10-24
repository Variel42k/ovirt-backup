// Package vmtransfer coordinates moves between independent oVirt engines.
package vmtransfer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

type Engine interface {
	Execute(context.Context, backup.RunRequest) (*model.BackupRun, error)
	Verify(context.Context, string, model.VerifyMode, model.VerifyOptions) (*model.VerifyRun, error)
	RestoreVM(context.Context, *model.RestoreVMRequest) (*backup.RestoreVMResult, error)
	LoadVMChainCopy(context.Context, string, string) (*backup.ChainSet, error)
	LoadChain(context.Context, string) (*backup.ChainSet, error)
	Restore(context.Context, backup.RestoreRequest) (*model.RestoreRun, error)
}

type Service struct {
	BackupRunner func(context.Context, backup.RunRequest) (*model.BackupRun, error)
	st           *store.Store
	pool         *ovirt.Pool
	engine       Engine
	ctx          context.Context
	log          zerolog.Logger
	mu           sync.Mutex
	active       map[string]context.CancelFunc
	slots        chan struct{}
}

func New(ctx context.Context, st *store.Store, pool *ovirt.Pool, engine Engine, workers int, log zerolog.Logger) *Service {
	if workers < 1 {
		workers = 1
	}
	return &Service{st: st, pool: pool, engine: engine, ctx: ctx, log: log, active: map[string]context.CancelFunc{}, slots: make(chan struct{}, workers)}
}

func (s *Service) Recover(ctx context.Context) error {
	items, err := s.st.ListVMTransfers(ctx)
	if err != nil {
		return err
	}
	for _, t := range items {
		if t.Status == "running" || t.Status == "queued" {
			t.Status = "interrupted"
			t.Error = "Служба перезапущена. Проверьте состояние источника и цели; продолжите подготовленную миграцию или выполните откат."
			if err = s.st.UpdateVMTransfer(ctx, t); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameEngine(a, b string) bool {
	normalize := func(raw string) string {
		if apiURL, err := ovirt.APIURL(raw); err == nil {
			raw = apiURL
		}
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return raw
		}
		return strings.ToLower(u.Host) + strings.TrimSuffix(u.Path, "/")
	}
	return normalize(a) == normalize(b)
}

func (s *Service) Plan(ctx context.Context, r model.VMTransferRequest) (*model.VMTransferPlan, error) {
	return s.plan(ctx, r, "")
}
func (s *Service) plan(ctx context.Context, r model.VMTransferRequest, ownID string) (*model.VMTransferPlan, error) {
	p := &model.VMTransferPlan{Request: r, Blockers: []string{}, Warnings: []string{}, NICs: []model.VMTransferNIC{}}
	if r.Mode != "cold" && r.Mode != "warm" {
		p.Blockers = append(p.Blockers, "Выберите режим миграции: cold или warm")
	}
	source, err := s.st.GetServer(ctx, r.SourceServerID)
	if err != nil {
		return nil, fmt.Errorf("исходное подключение: %w", err)
	}
	target, err := s.st.GetServer(ctx, r.TargetServerID)
	if err != nil {
		return nil, fmt.Errorf("целевое подключение: %w", err)
	}
	if !source.Kind.UsesOVirtAPI() || !target.Kind.UsesOVirtAPI() {
		p.Blockers = append(p.Blockers, "Поддерживаются только подключения семейства oVirt")
	}
	if source.ID == target.ID || sameEngine(source.EngineURL, target.EngineURL) {
		p.Blockers = append(p.Blockers, "Выберите другой независимый oVirt Engine")
	}
	if !source.Enabled || !target.Enabled {
		p.Blockers = append(p.Blockers, "Оба подключения должны быть включены")
	}
	vm, err := s.st.GetVM(ctx, r.SourceServerID, r.SourceVMID)
	if err != nil {
		return nil, err
	}
	p.SourceName = vm.Name
	if strings.TrimSpace(r.Name) == "" {
		p.Request.Name = vm.Name
	}
	if len(p.Request.Name) > 255 || strings.ContainsAny(p.Request.Name, "\r\n\t") {
		p.Blockers = append(p.Blockers, "Недопустимое имя целевой ВМ")
	}
	if owner, err := s.st.VMTransferOwner(ctx, source.ID, vm.ID); err != nil {
		return nil, err
	} else if owner != "" && owner != ownID {
		p.Blockers = append(p.Blockers, "ВМ уже участвует в миграции "+owner)
	}
	repository, err := s.st.GetStorageTarget(ctx, r.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("репозиторий: %w", err)
	}
	if !repository.Enabled || repository.ReadOnly {
		p.Blockers = append(p.Blockers, "Репозиторий должен быть доступен для записи")
	}
	clusters, err := s.st.ListClusters(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	clusterFound := false
	for _, c := range clusters {
		if c.ID == r.ClusterID {
			clusterFound = true
		}
	}
	if !clusterFound {
		p.Blockers = append(p.Blockers, "Выберите существующий целевой кластер")
	}
	domains, err := s.st.ListStorageDomains(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	var free int64
	domainFound := false
	for _, d := range domains {
		if d.ID == r.StorageDomainID && d.Type == "data" && d.Status == "active" {
			domainFound = true
			free = d.AvailableSize
		}
	}
	if !domainFound {
		p.Blockers = append(p.Blockers, "Выберите активный целевой домен данных")
	}
	if len(p.Blockers) > 0 {
		return p, nil
	}
	src, err := s.pool.Get(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	dst, err := s.pool.Get(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	sourceState, err := src.GetVMState(ctx, vm.ID)
	if err != nil {
		return nil, fmt.Errorf("источник недоступен: %w", err)
	}
	// Live inventory verifies the destination before any source shutdown.
	existing, err := dst.SearchVMs(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("цель недоступна: %w", err)
	}
	for _, v := range existing {
		if strings.EqualFold(strings.TrimSpace(v.Name), p.Request.Name) {
			p.Blockers = append(p.Blockers, "На цели уже существует ВМ с таким именем")
		}
	}
	if sourceState.Status != "up" && sourceState.Status != "down" {
		p.Blockers = append(p.Blockers, "Дождитесь устойчивого состояния исходной ВМ: "+sourceState.Status)
	}
	rawConfig, err := src.VMConfiguration(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	profile := backup.ProfileFromOVirtConfig([]byte(rawConfig), vm, nil)
	if profile.Architecture != "x86_64" {
		p.Blockers = append(p.Blockers, "Миграция этой архитектуры ещё не поддерживается: "+profile.Architecture)
	}
	if profile.SecureBoot {
		p.Blockers = append(p.Blockers, "Перенос Secure Boot требует отдельной проверки ключей и NVRAM и пока заблокирован")
	}
	hosts, err := dst.ListHosts(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	targetHost := ""
	for _, h := range hosts {
		if h.ClusterID == r.ClusterID && h.Status == "up" && (r.HostID == "" || r.HostID == h.ID) {
			targetHost = h.ID
			break
		}
	}
	if targetHost == "" {
		p.Blockers = append(p.Blockers, "Целевой хост должен работать и принадлежать выбранному кластеру")
	} else {
		storage, err := dst.HostStorageDomains(ctx, target.ID, targetHost)
		if err != nil {
			return nil, err
		}
		attached := false
		for _, d := range storage.Domains {
			if d.ID == r.StorageDomainID {
				attached = true
				free = d.AvailableSize
			}
		}
		if !attached {
			p.Blockers = append(p.Blockers, "Домен недоступен выбранному целевому хосту")
		}
	}
	disks, err := src.ListVMDisks(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	p.WarmAvailable = source.SupportsCBT && len(disks) > 0
	p.WarmReason = "Для предварительного копирования нужны Backup API и incremental на всех дисках"
	if len(disks) == 0 {
		p.Blockers = append(p.Blockers, "У ВМ нет дисков для переноса")
	}
	for _, d := range disks {
		p.TotalBytes += d.ProvisionedSize.Int64()
		if d.Backup != "incremental" {
			p.WarmAvailable = false
		}
		if d.Inactive || bool(d.Shareable) || d.LunStorage != nil || d.ContentType != "" && d.ContentType != "data" {
			p.Blockers = append(p.Blockers, "Диск "+d.Alias+": требуется обычный активный диск данных без общего доступа")
		}
	}
	if r.Mode == "warm" && !p.WarmAvailable {
		p.Blockers = append(p.Blockers, p.WarmReason)
	}
	if free < p.TotalBytes {
		p.Blockers = append(p.Blockers, "В целевом домене недостаточно места под виртуальный размер дисков")
	}
	p.AvailableBytes = free
	nics, err := src.TransferNICs(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	networks, err := dst.ListVNICProfiles(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	valid := map[string]bool{}
	for _, n := range networks {
		valid[n.ID] = true
	}
	mappings := map[string]model.RestoreVMNetworkMapping{}
	for _, m := range r.NetworkMappings {
		if _, ok := mappings[m.NICID]; ok {
			p.Blockers = append(p.Blockers, "Повторное сопоставление сетевого интерфейса")
		}
		mappings[m.NICID] = m
	}
	for _, n := range nics {
		p.NICs = append(p.NICs, model.VMTransferNIC{ID: n.ID, Name: n.Name, MAC: n.MAC.Address})
		m, ok := mappings[n.ID]
		if !ok || !m.Exclude && (!valid[m.TargetID] || m.TargetKind != "vnic_profile") {
			p.Blockers = append(p.Blockers, "Выберите целевую сеть или исключите интерфейс "+n.Name)
		}
	}
	if r.PreserveMAC {
		macs := map[string]bool{}
		for _, n := range nics {
			macs[strings.ToLower(n.MAC.Address)] = true
		}
		for _, v := range existing {
			ns, e := dst.TransferNICs(ctx, v.ID)
			if e != nil {
				return nil, e
			}
			for _, n := range ns {
				if macs[strings.ToLower(n.MAC.Address)] {
					p.Blockers = append(p.Blockers, "На цели уже используется MAC "+n.MAC.Address)
				}
			}
		}
	}
	p.Warnings = append(p.Warnings, "Задания резервного копирования источника автоматически не переназначаются: после переноса настройте защиту целевой ВМ")
	p.Warnings = append(p.Warnings, "UUID целевой ВМ будет новым. Проверьте привязанные к UUID, serial и MAC лицензии.", "Исходная ВМ сохраняется выключенной для отката. Автодействия и конфликтующие операции будут заблокированы.")
	if !r.PreserveMAC {
		p.Warnings = append(p.Warnings, "Целевой Engine выделит новые MAC-адреса; проверьте сетевые настройки гостя")
	}
	return p, nil
}

func (s *Service) Create(ctx context.Context, r model.VMTransferRequest, actor string) (*model.VMTransfer, error) {
	if !r.Confirm {
		return nil, fmt.Errorf("требуется подтверждение остановки источника и изменений идентичности")
	}
	plan, err := s.Plan(ctx, r)
	if err != nil {
		return nil, err
	}
	if len(plan.Blockers) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(plan.Blockers, "; "))
	}
	vm, err := s.st.GetVM(ctx, r.SourceServerID, r.SourceVMID)
	if err != nil {
		return nil, err
	}
	t := &model.VMTransfer{ID: uuid.NewString(), Request: plan.Request, SourceName: vm.Name, Status: "queued", Phase: "queued", DiskMap: map[string]string{}, RunIDs: []string{}, TotalBytes: plan.TotalBytes, OriginalDesired: vm.DesiredState, OriginalOptOut: vm.RemediationOptOut, WasRunning: vm.Status != "down", CreatedBy: actor}
	if err = s.st.CreateVMTransfer(ctx, t); err != nil {
		return nil, err
	}
	result := *t
	result.DiskMap = map[string]string{}
	if err = s.launch(t, "prepare"); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *Service) save(t *model.VMTransfer) error {
	return s.st.UpdateVMTransfer(context.WithoutCancel(s.ctx), t)
}
func (s *Service) phase(t *model.VMTransfer, phase string, progress int) error {
	t.Phase = phase
	t.Progress = progress
	return s.save(t)
}

func (s *Service) launch(t *model.VMTransfer, action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked(t, action)
}

func (s *Service) startLocked(t *model.VMTransfer, action string) error {
	expected := t.Status
	t.Status = "queued"
	t.Phase = "queued"
	t.PendingAction = action
	t.Error = ""
	if err := s.st.ClaimVMTransfer(s.ctx, t, expected); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.active[t.ID] = cancel
	go func() {
		defer func() { cancel(); s.mu.Lock(); delete(s.active, t.ID); s.mu.Unlock() }()
		if err := s.acquireSlot(ctx, t); err != nil {
			t.Status = "interrupted"
			t.Error = err.Error()
			_ = s.save(t)
			return
		}
		defer func() { <-s.slots }()
		var err error
		switch action {
		case "prepare":
			err = s.prepare(ctx, t)
		case "sync":
			err = s.synchronize(ctx, t, false)
		case "cutover":
			err = s.cutover(ctx, t)
		case "cancel", "rollback":
			err = s.rollback(ctx, t, action)
		}
		if err != nil {
			t.Status = "failed"
			if errors.Is(err, context.Canceled) {
				t.Status = "interrupted"
			}
			t.Error = err.Error()
			if saveErr := s.save(t); saveErr != nil {
				s.log.Error().Err(saveErr).Str("migration", t.ID).Msg("сохранение ошибки миграции")
			}
		}
	}()
	return nil
}

// Queue admission is serialized with action requests. Waiting tasks do not
// occupy backup/transfer workers; batch order and limits live in the database.
func (s *Service) acquireSlot(ctx context.Context, t *model.VMTransfer) error {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		items, err := s.st.ListVMTransfers(ctx)
		admit := err == nil && len(s.slots) < cap(s.slots)
		if admit && t.BatchID != "" {
			running := 0
			limit := t.BatchParallelism
			if limit < 1 {
				limit = 1
			}
			for _, other := range items {
				if other.BatchID != t.BatchID || other.ID == t.ID {
					continue
				}
				if other.Status == "running" {
					running++
				}
				if limit == 1 && other.BatchIndex < t.BatchIndex && t.PendingAction != "rollback" && t.PendingAction != "cancel" {
					switch other.Status {
					case "ready", "awaiting_confirmation", "completed", "canceled", "rolled_back":
					default:
						admit = false
					}
				}
			}
			admit = admit && running < limit
		}
		if admit {
			t.Status = "running"
			t.PendingAction = ""
			err = s.st.ClaimVMTransfer(ctx, t, "queued")
			if err == nil {
				s.slots <- struct{}{}
			}
		}
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if admit {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (s *Service) Action(ctx context.Context, id, action string, confirm bool) (*model.VMTransfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.st.GetVMTransfer(ctx, id)
	if err != nil {
		return nil, err
	}
	if cancel, ok := s.active[id]; ok {
		if action == "pause" {
			cancel()
			return t, nil
		}
		return nil, fmt.Errorf("операция ещё выполняется; дождитесь завершения или приостановите её")
	}
	allowed := false
	switch action {
	case "resume":
		if t.Status != "interrupted" || t.PendingAction == "" {
			return nil, fmt.Errorf("возобновить можно приостановленную задачу очереди")
		}
		action = t.PendingAction
		allowed = true
	case "forget":
		if !confirm {
			return nil, fmt.Errorf("подтвердите удаление записи")
		}
		return t, s.st.DeleteReleasedVMTransfer(ctx, id)
	case "sync", "cutover":
		allowed = t.Request.Mode == "warm" && t.TargetVMID != "" && t.AppliedRunID != "" && !t.TargetStarted && (t.Status == "ready" || t.Status == "interrupted" || t.Status == "failed")
	case "cancel", "rollback":
		allowed = t.Status != "canceled" && t.Status != "rolled_back"
	case "complete":
		if t.Status != "awaiting_confirmation" {
			return nil, fmt.Errorf("сначала проверьте запуск целевой ВМ")
		}
		if !confirm {
			return nil, fmt.Errorf("подтвердите результат")
		}
		source, err := s.pool.Get(ctx, t.Request.SourceServerID)
		if err != nil {
			return nil, err
		}
		target, err := s.pool.Get(ctx, t.Request.TargetServerID)
		if err != nil {
			return nil, err
		}
		sourceState, _, err := source.VMStatus(ctx, t.Request.SourceVMID)
		if err != nil {
			return nil, err
		}
		targetState, _, err := target.VMStatus(ctx, t.TargetVMID)
		if err != nil {
			return nil, err
		}
		if sourceState != "down" || targetState != "up" {
			return nil, fmt.Errorf("для завершения источник должен быть выключен, а цель работать; сейчас источник %s, цель %s", sourceState, targetState)
		}
		previous := t.Status
		t.Status = "completed"
		t.Phase = "completed"
		t.Progress = 100
		return t, s.st.ClaimVMTransfer(ctx, t, previous)
	}
	if !allowed {
		return nil, fmt.Errorf("действие %s недоступно в состоянии %s", action, t.Status)
	}
	if !confirm {
		return nil, fmt.Errorf("подтвердите действие")
	}
	result := *t
	if err := s.startLocked(t, action); err != nil {
		return nil, err
	}
	return &result, nil
}
