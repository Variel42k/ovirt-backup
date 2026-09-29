package backup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// The planner answers the question an operator actually has in front of a VM:
// "what are my options here, and which one should I pick?" — with the reasons
// spelled out, because a recommendation nobody understands gets ignored.

// Assessment is the set of facts the recommendations are derived from.
type Assessment struct {
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	VMID       string `json:"vm_id"`
	VMName     string `json:"vm_name"`
	VMStatus   string `json:"vm_status"`
	VMRunning  bool   `json:"vm_running"`

	EngineSupportsCBT bool `json:"engine_supports_cbt"`
	GuestAgent        bool `json:"guest_agent"`

	DiskCount   int         `json:"disk_count"`
	Disks       []DiskFacts `json:"disks"`
	TotalSize   int64       `json:"total_provisioned"`
	TotalUsed   int64       `json:"total_actual"`
	CBTEnabled  int         `json:"cbt_enabled_disks"`
	CBTPossible int         `json:"cbt_possible_disks"`
	// RawDisks — сколько дисков не могут отслеживать изменённые блоки из-за
	// формата. Они бэкапятся полностью и на горячую; это не «незащищённые»
	// диски, и интерфейс должен уметь сказать об этом прямо.
	RawDisks int `json:"raw_disks"`
	// Libvirt — ВМ на KVM. Там инкремент требует карты изменений на всех
	// дисках ВМ: смешанного бэкапа, как в oVirt 4.4.5+, у libvirt нет, и один
	// raw-диск делает каждый запуск полным.
	Libvirt bool `json:"libvirt"`

	// Наблюдаемая пропускная способность, байт/с. 0 — истории ещё нет.
	ObservedThroughput int64 `json:"observed_throughput"`
	// Средний объём инкремента по истории.
	AverageIncrement int64 `json:"average_increment"`
	// LastFullBytes — сколько прочитал последний полный запуск. Это точнее
	// любой оценки по инвентарю: движок отдаёт занятое место томов, а не то,
	// что реально видит гость.
	LastFullBytes  int64            `json:"last_full_bytes,omitempty"`
	LastBackupAt   *time.Time       `json:"last_backup_at,omitempty"`
	LastBackupType model.BackupType `json:"last_backup_type,omitempty"`
	BackupCount    int              `json:"backup_count"`

	QemuImgAvailable bool `json:"qemu_img_available"`

	// Space — хватит ли места под записи гостя, пока идёт горячий бэкап.
	// Только совет: запуск по-прежнему решают сторожа места.
	Space *SpaceForecast `json:"space_forecast,omitempty"`

	// Замечания — то, что стоит починить до настройки расписания.
	Warnings []string `json:"warnings,omitempty"`
}

// backupAPI — движок умеет горячую полную копию с точкой отсчёта: Backup API
// в oVirt, pull-бэкап в libvirt.
func (a Assessment) backupAPI() bool { return a.EngineSupportsCBT && a.DiskCount > 0 }

// incrementalReady — инкременты возможны. В oVirt хватает одного диска с
// режимом incremental: остальные движок отдаёт в том же бэкапе целиком. На
// KVM карта изменений нужна на всех дисках, иначе драйвер снимает полную.
func (a Assessment) incrementalReady() bool {
	if !a.backupAPI() || a.CBTEnabled == 0 {
		return false
	}
	return !a.Libvirt || a.CBTEnabled == a.DiskCount
}

// rawDiskNames — диски без карты изменений, для подсказок.
func (a Assessment) rawDiskNames() (string, int64) {
	var names []string
	var used int64
	for _, d := range a.Disks {
		if d.CBTBlocker == "" || d.NotBackedUp != "" {
			continue
		}
		names = append(names, d.Alias)
		used += d.ActualSize
	}
	return strings.Join(names, ", "), used
}

// FullBytes — ожидаемый объём полной копии: по последнему полному запуску,
// пока его нет — по занятому месту из инвентаря.
func (a Assessment) FullBytes() int64 {
	if a.LastFullBytes > 0 {
		return a.LastFullBytes
	}
	return a.TotalUsed
}

// DiskFacts is what matters about one disk when choosing a strategy.
type DiskFacts struct {
	ID              string `json:"id"`
	Alias           string `json:"alias"`
	ProvisionedSize int64  `json:"provisioned_size"`
	ActualSize      int64  `json:"actual_size"`
	Format          string `json:"format"`
	BackupMode      string `json:"backup_mode"`
	Sparse          bool   `json:"sparse"`
	Shareable       bool   `json:"shareable"`
	StorageDomain   string `json:"storage_domain"`
	// CanEnableCBT — можно ли включить инкрементальный режим прямо сейчас.
	CanEnableCBT bool   `json:"can_enable_cbt"`
	CBTBlocker   string `json:"cbt_blocker,omitempty"`
	// NotBackedUp — почему диск не попадёт в копию ВМ вовсе. Такой диск не
	// входит в оценки объёма: иначе они обещали бы данные, которых в копии не будет.
	NotBackedUp string `json:"not_backed_up,omitempty"`
}

// Option is one offered backup strategy.
type Option struct {
	Type        model.BackupType `json:"type"`
	Title       string           `json:"title"`
	Available   bool             `json:"available"`
	Recommended bool             `json:"recommended"`
	// Rationale — почему стоит (или не стоит) выбирать этот вариант.
	Rationale string `json:"rationale"`
	// Blocker заполняется, когда вариант недоступен.
	Blocker string `json:"blocker,omitempty"`
	// Impact описывает влияние на работу ВМ.
	Impact string `json:"impact"`

	EstimatedBytes    int64  `json:"estimated_bytes"`
	EstimatedDuration string `json:"estimated_duration"`

	// Prerequisites — что нужно сделать, чтобы вариант стал доступен.
	Prerequisites []string `json:"prerequisites,omitempty"`
	// SuggestedVerify — какая проверка уместна для этого типа.
	SuggestedVerify model.VerifyMode `json:"suggested_verify"`
}

// SchedulePreset is a ready-made schedule an operator can accept as-is.
type SchedulePreset struct {
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Type        model.BackupType      `json:"type"`
	Schedule    string                `json:"schedule"`
	FullEvery   int                   `json:"full_every"`
	Retention   model.RetentionPolicy `json:"retention"`
	VerifyAfter model.VerifyMode      `json:"verify_after"`
	Quiesce     bool                  `json:"quiesce"`
	Recommended bool                  `json:"recommended"`
	// EstimatedFootprint — сколько места займёт хранилище на горизонте политики.
	EstimatedFootprint int64 `json:"estimated_footprint"`
}

// Recommendation bundles everything the UI shows on the "как бэкапить эту ВМ"
// screen.
type Recommendation struct {
	Assessment Assessment       `json:"assessment"`
	Options    []Option         `json:"options"`
	Presets    []SchedulePreset `json:"presets"`
}

// ProxmoxRecommendation describes the one data path Proxmox can expose
// without staging an archive on its own backup storage: a complete native
// vzdump stream. The dispatcher turns legacy full/snapshot jobs into this
// format, while new jobs see only the honest supported choice here.
func ProxmoxRecommendation(srv *model.Server, vm *model.VM) *Recommendation {
	ready := srv != nil && srv.HasProxmoxDataPlane()
	a := Assessment{ServerID: srv.ID, ServerName: srv.Name, VMID: vm.ID, VMName: vm.Name,
		VMStatus: vm.Status, VMRunning: vm.Running(), EngineSupportsCBT: false, DiskCount: 1}
	if !ready {
		a.Warnings = append(a.Warnings, "SSH-канал данных не настроен: закрепите ключи всех узлов и установите helper")
	}
	unsupported := func(t model.BackupType, reason string) Option {
		return Option{Type: t, Title: t.Title(), Available: false, Blocker: reason}
	}
	full := Option{Type: model.BackupFull, Title: "Нативный полный Proxmox (vzdump)", Available: ready,
		Recommended: ready, Rationale: "самодостаточный архив с конфигурацией и всеми дисками гостя",
		Impact:            "snapshot-mode; гость продолжает работать, Proxmox кратко фиксирует согласованную точку",
		EstimatedDuration: "зависит от занятого объёма", SuggestedVerify: model.VerifyChain}
	if !ready {
		full.Blocker = "настройте SSH-канал данных Proxmox на всех узлах кластера"
		full.Prerequisites = []string{"установить jhvirt-pve-data-plane", "закрепить SSH-ключ каждого узла"}
	}
	return &Recommendation{Assessment: a, Options: []Option{
		full,
		unsupported(model.BackupIncremental, "vzdump выдаёт только полный нативный архив; CBT-поток через API Proxmox отсутствует"),
		unsupported(model.BackupDifferential, "vzdump выдаёт только полный нативный архив"),
		unsupported(model.BackupSnapshot, "этот режим заменён нативным полным vzdump в snapshot-mode"),
		unsupported(model.BackupConfig, "конфигурация без данных не является восстанавливаемой точкой Proxmox"),
		unsupported(model.BackupOVA, "Proxmox использует нативный формат vzdump, а не OVA"),
	}, Presets: []SchedulePreset{{Name: "Ежедневный нативный Proxmox", Description: "Полный vzdump каждую ночь",
		Type: model.BackupFull, Schedule: "0 1 * * *", Retention: model.RetentionPolicy{KeepLast: 3, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6},
		VerifyAfter: model.VerifyChain, Recommended: ready}}}
}

// defaultThroughput is the fallback estimate before any run has happened.
// Deliberately conservative: an estimate that turns out optimistic erodes
// trust faster than one that turns out generous.
const defaultThroughput = 120 << 20 // 120 МиБ/с

// Recommend assesses a VM and produces the offered options.
func (e *Engine) Recommend(ctx context.Context, serverID, vmID, storageTargetID string) (*Recommendation, error) {
	srv, err := e.store.GetServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	vm, err := e.store.GetVM(ctx, serverID, vmID)
	if err != nil {
		return nil, err
	}
	disks, err := e.store.ListDisksForVM(ctx, serverID, vmID)
	if err != nil {
		return nil, err
	}

	a := Assessment{
		ServerID:          srv.ID,
		ServerName:        srv.Name,
		VMID:              vm.ID,
		VMName:            vm.Name,
		VMStatus:          vm.Status,
		VMRunning:         vm.Running(),
		EngineSupportsCBT: srv.SupportsCBT,
		Libvirt:           srv.Kind.UsesLibvirt(),
		GuestAgent:        vm.GuestAgent,
		QemuImgAvailable:  QemuImgAvailable(e.cfg.QemuImgPath),
	}

	for _, d := range disks {
		if d.ContentType != "" && d.ContentType != "data" {
			continue
		}
		f := DiskFacts{
			ID:              d.ID,
			Alias:           d.Alias,
			ProvisionedSize: d.ProvisionedSize,
			ActualSize:      d.ActualSize,
			Format:          d.Format,
			BackupMode:      d.BackupMode,
			Sparse:          d.Sparse,
			Shareable:       d.Shareable,
			StorageDomain:   d.StorageDomain,
		}
		if d.StorageType == "lun" {
			f.NotBackedUp = "Direct LUN: движок отдаёт в бэкап только образы из доменов хранения — " +
				"этот диск в копию ВМ не попадёт"
			a.Disks = append(a.Disks, f)
			a.Warnings = append(a.Warnings, fmt.Sprintf(
				"диск %s (%s) подключён как Direct LUN и в копию ВМ не попадёт: защищайте его средствами СХД "+
					"или изнутри гостя (например, логическим дампом СУБД)", d.Alias, humanBytes(d.ProvisionedSize)))
			continue
		}
		switch {
		case d.SupportsIncremental():
			a.CBTEnabled++
			a.CBTPossible++
		case d.CanEnableIncremental():
			f.CanEnableCBT = true
			a.CBTPossible++
		default:
			// oVirt only tracks changed blocks for qcow2 volumes.
			//
			// The wording matters here. "Отслеживание недоступно" alone reads
			// as "этот диск нельзя защитить", which is wrong and frightening:
			// the disk is backed up hot like any other, it just cannot be
			// copied incrementally. Saying both halves stops an operator from
			// concluding that raw disks are unprotected.
			f.CBTBlocker = fmt.Sprintf(
				"формат %s: карта изменённых блоков хранится в заголовке qcow2, у %s его нет. "+
					"Диск бэкапится полностью и без остановки ВМ; недоступны только инкременты",
				d.Format, d.Format)
			a.RawDisks++
		}
		a.Disks = append(a.Disks, f)
		a.TotalSize += d.ProvisionedSize
		// Цепочка qcow2 с метаданными бывает больше самого диска, а копия
		// больше выделенного объёма не прочитает.
		used := d.ActualSize
		if d.ProvisionedSize > 0 && used > d.ProvisionedSize {
			used = d.ProvisionedSize
		}
		a.TotalUsed += used
	}
	for _, d := range a.Disks {
		if d.NotBackedUp == "" {
			a.DiskCount++
		}
	}

	e.enrichFromHistory(ctx, &a, storageTargetID)

	if a.DiskCount == 0 {
		a.Warnings = append(a.Warnings, "у ВМ нет дисков с данными — бэкапить нечего, кроме конфигурации")
	}
	if !srv.SupportsCBT {
		a.Warnings = append(a.Warnings,
			"движок не поддерживает инкрементальный бэкап: доступны только полные копии через снапшот")
	}
	if a.CBTEnabled > 0 && a.CBTEnabled < a.CBTPossible {
		a.Warnings = append(a.Warnings, fmt.Sprintf(
			"режим incremental включён на %d дисках из %d: остальные в каждом инкременте копируются целиком. "+
				"Включите режим и на них, чтобы копировались только изменения",
			a.CBTEnabled, a.CBTPossible))
	}
	if a.Libvirt && a.RawDisks > 0 && a.DiskCount > 0 {
		names, used := a.rawDiskNames()
		a.Warnings = append(a.Warnings, fmt.Sprintf(
			"на KVM инкременты невозможны, пока у ВМ есть raw-диски (%s, занято %s): каждый запуск — полная "+
				"копия на горячую. Переведите их в qcow2, чтобы копировались только изменения",
			names, humanBytes(used)))
	}
	// Движок без Backup API (oVirt 4.3) отдаёт том qcow2 файлом, и образ
	// собирается через qemu-img. Без него бэкап такой ВМ не выполнится.
	if !a.Libvirt && !srv.SupportsCBT && !a.QemuImgAvailable {
		for _, d := range a.Disks {
			if d.Format == "cow" && d.NotBackedUp == "" {
				a.Warnings = append(a.Warnings, fmt.Sprintf(
					"диск %s в формате qcow2, а движок без Backup API отдаёт такой том файлом: образ собирается "+
						"через qemu-img, которого на сервере службы нет. Установите qemu-img (backup.qemu_img_path), "+
						"иначе бэкап этой ВМ не выполнится", d.Alias))
				break
			}
		}
	}
	if a.VMRunning && !a.GuestAgent {
		a.Warnings = append(a.Warnings,
			"гостевой агент не отвечает: заморозка файловых систем недоступна, копия будет crash-consistent")
	}
	for _, d := range a.Disks {
		if d.Shareable {
			a.Warnings = append(a.Warnings, fmt.Sprintf(
				"диск %s общий (shareable) и в бэкап не попадёт — такие диски нужно защищать отдельно", d.Alias))
		}
	}
	if a.Space = e.forecastSpace(ctx, srv, vm, disks); a.Space != nil {
		a.Warnings = append(a.Warnings, a.Space.Warnings...)
	}

	return &Recommendation{
		Assessment: a,
		Options:    buildOptions(a),
		Presets:    buildPresets(a),
	}, nil
}

// enrichFromHistory derives throughput and typical increment size from what
// actually happened, which beats any built-in constant.
func (e *Engine) enrichFromHistory(ctx context.Context, a *Assessment, storageTargetID string) {
	runs, err := e.store.ListBackupRuns(ctx, store.RunFilter{
		ServerID: a.ServerID,
		VMID:     a.VMID,
		TargetID: storageTargetID,
		Statuses: []model.RunStatus{model.RunSucceeded, model.RunPartial},
		Limit:    30,
	})
	if err != nil || len(runs) == 0 {
		return
	}
	a.BackupCount = len(runs)
	a.LastBackupAt = &runs[0].CreatedAt
	a.LastBackupType = runs[0].Type

	var throughputSum, throughputN int64
	var incSum, incN int64
	for _, r := range runs {
		if d := r.Duration(); d > 5*time.Second && r.ReadBytes > 0 {
			throughputSum += int64(float64(r.ReadBytes) / d.Seconds())
			throughputN++
		}
		if r.Type == model.BackupIncremental && r.ReadBytes > 0 {
			incSum += r.ReadBytes
			incN++
		}
		// Запуски идут от новых к старым: первый полный — самый свежий.
		if a.LastFullBytes == 0 && r.ReadBytes > 0 &&
			(r.Type == model.BackupFull || r.Type == model.BackupSnapshot) {
			a.LastFullBytes = r.ReadBytes
		}
	}
	if throughputN > 0 {
		a.ObservedThroughput = throughputSum / throughputN
	}
	if incN > 0 {
		a.AverageIncrement = incSum / incN
	}
}

// buildOptions turns the assessment into the offered strategies.
func buildOptions(a Assessment) []Option {
	throughput := a.ObservedThroughput
	if throughput <= 0 {
		throughput = defaultThroughput
	}
	estimate := func(bytes int64) string {
		if bytes <= 0 {
			return "меньше минуты"
		}
		d := time.Duration(float64(bytes)/float64(throughput)) * time.Second
		if d < time.Minute {
			return "меньше минуты"
		}
		return d.Round(time.Minute).String()
	}

	// Backup API снимает полную копию любых дисков — raw и qcow2 — без
	// временного снапшота. Инкремент нужен хотя бы одному диску с режимом
	// incremental: остальные движок (4.4.5+) отдаёт в том же бэкапе целиком.
	backupAPI := a.backupAPI()
	incrementalReady := a.incrementalReady()
	// oVirt 4.3 has no native Backup API, but the service can still build an
	// incremental chain from ordinary snapshots. The operator chooses between
	// reading and comparing the full image and retaining a QCOW2 snapshot base.
	legacyIncremental := !a.Libvirt && !a.EngineSupportsCBT && a.DiskCount > 0
	mixed := incrementalReady && !a.Libvirt && a.CBTEnabled < a.DiskCount
	hasHistory := a.BackupCount > 0
	rawNames, rawUsed := a.rawDiskNames()
	kvmRaw := a.Libvirt && a.RawDisks > 0

	fullEstimate := a.FullBytes()

	// Without measurements, assume a working day changes a few percent of the
	// allocated data — the figure most storage teams plan around.
	incrementEstimate := a.AverageIncrement
	if incrementEstimate <= 0 {
		incrementEstimate = fullEstimate / 20
	}

	options := []Option{
		{
			Type:            model.BackupFull,
			Title:           model.BackupFull.Title(),
			Available:       backupAPI,
			Impact:          "ВМ продолжает работать; движок держит точку согласованности на время чтения",
			EstimatedBytes:  fullEstimate,
			SuggestedVerify: model.VerifyManifest,
		},
		{
			Type:            model.BackupIncremental,
			Title:           model.BackupIncremental.Title(),
			Available:       incrementalReady || legacyIncremental,
			Impact:          "ВМ продолжает работать; читаются только изменённые блоки",
			EstimatedBytes:  incrementEstimate,
			SuggestedVerify: model.VerifyChain,
		},
		{
			Type:            model.BackupDifferential,
			Title:           model.BackupDifferential.Title(),
			Available:       incrementalReady || legacyIncremental,
			Impact:          "ВМ продолжает работать; читается всё, что изменилось с последнего полного",
			EstimatedBytes:  incrementEstimate * 5,
			SuggestedVerify: model.VerifyChain,
		},
		{
			Type:            model.BackupSnapshot,
			Title:           model.BackupSnapshot.Title(),
			Available:       a.DiskCount > 0,
			Impact:          "ВМ продолжает работать; после копирования снапшот сливается обратно — это нагружает СХД",
			EstimatedBytes:  fullEstimate,
			SuggestedVerify: model.VerifyManifest,
		},
		{
			Type:            model.BackupConfig,
			Title:           model.BackupConfig.Title(),
			Available:       true,
			Impact:          "никакого влияния на ВМ",
			EstimatedBytes:  64 << 10,
			SuggestedVerify: model.VerifyQuick,
		},
		{
			Type:            model.BackupOVA,
			Title:           model.BackupOVA.Title(),
			Available:       true,
			Impact:          "самый долгий вариант; файл остаётся на хосте гипервизора, а не в хранилище бэкапов",
			EstimatedBytes:  fullEstimate,
			SuggestedVerify: model.VerifyQuick,
		},
	}

	for i := range options {
		o := &options[i]
		o.EstimatedDuration = estimate(o.EstimatedBytes)

		switch o.Type {
		case model.BackupFull:
			switch {
			case !a.EngineSupportsCBT:
				o.Blocker = "движок не поддерживает Backup API — используйте полный через снапшот"
			case a.DiskCount == 0:
				o.Blocker = "у ВМ нет дисков с данными"
			case kvmRaw:
				o.Rationale = "полная копия на горячую через pull-бэкап libvirt; checkpoint не создаётся, " +
					"пока у ВМ есть raw-диски, поэтому каждый запуск читает весь занятый объём"
			case a.CBTEnabled == 0:
				o.Rationale = "полная копия через Backup API без временного снапшота — для дисков любого формата, " +
					"в том числе raw; ВМ продолжает работать"
			default:
				o.Rationale = "база для инкрементов: создаёт checkpoint, от которого считаются все последующие копии"
			}
		case model.BackupIncremental:
			switch {
			case o.Blocker != "":
			case legacyIncremental:
				o.Rationale = "совместимый режим для oVirt без Backup API: сравнение читает весь снимок, " +
					"а цепочка QCOW2 передаёт только новый слой; способ выбирается в параметрах задания"
				o.Impact = "ВМ продолжает работать; нагрузка зависит от выбранного режима: полное чтение или передача слоя QCOW2"
			case a.CBTPossible == 0:
				o.Blocker = fmt.Sprintf(
					"инкремент опирается на карту изменённых блоков, а её негде хранить: " +
						"диски в формате, где нет заголовка qcow2. Полная копия при этом доступна и снимается на горячую")
				o.Prerequisites = append(o.Prerequisites,
					"перевести диски в qcow2 (тонкое выделение) — иначе инкременты невозможны в принципе")
			case kvmRaw:
				o.Blocker = kvmRawBlocker(a.RawDisks, rawNames)
				o.Prerequisites = append(o.Prerequisites, kvmRawPrerequisite(rawNames, rawUsed, incrementEstimate))
			case a.CBTEnabled == 0:
				o.Blocker = "ни на одном диске не включён режим incremental — каждый запуск был бы полным"
				o.Prerequisites = append(o.Prerequisites, "включить режим incremental на qcow2-дисках ВМ")
			case mixed:
				o.Rationale = fmt.Sprintf("смешанный бэкап (oVirt 4.4.5+): %d из %d дисков копируются инкрементом, "+
					"остальные — целиком в каждом запуске", a.CBTEnabled, a.DiskCount)
				if a.CBTEnabled < a.CBTPossible {
					o.Prerequisites = append(o.Prerequisites,
						"включить режим incremental на остальных qcow2-дисках, чтобы и они копировались изменениями")
				}
			case !hasHistory:
				o.Rationale = "первый запуск автоматически станет полным, дальше будут копироваться только изменения"
			default:
				o.Rationale = fmt.Sprintf("самый дешёвый вариант: по истории в среднем %s за запуск",
					humanBytes(incrementEstimate))
			}
		case model.BackupDifferential:
			switch {
			case legacyIncremental:
				o.Rationale = "совместимый разностный режим для oVirt без Backup API сравнивает полный снимок " +
					"с последней полной основой; цепочка QCOW2 для разностной копии недоступна"
				o.Impact = "ВМ продолжает работать; временный снимок читается целиком и сравнивается с полной основой"
			case a.CBTPossible == 0:
				o.Blocker = "разностный бэкап тоже опирается на карту изменённых блоков, " +
					"а формат дисков её не поддерживает"
				o.Prerequisites = append(o.Prerequisites,
					"перевести диски в qcow2 (тонкое выделение)")
			case kvmRaw:
				o.Blocker = kvmRawBlocker(a.RawDisks, rawNames)
				o.Prerequisites = append(o.Prerequisites, kvmRawPrerequisite(rawNames, rawUsed, incrementEstimate))
			case a.CBTEnabled == 0:
				o.Blocker = "ни на одном диске не включён режим incremental — каждый запуск был бы полным"
				o.Prerequisites = append(o.Prerequisites, "включить режим incremental на qcow2-дисках ВМ")
			default:
				o.Rationale = "компромисс: копий больше, чем при инкрементах, зато восстановление всегда из двух точек"
			}
		case model.BackupSnapshot:
			switch {
			case a.DiskCount == 0:
				o.Blocker = "у ВМ нет дисков с данными"
			case !a.EngineSupportsCBT:
				o.Rationale = "единственный горячий способ на движке без Backup API: копия снимается без остановки ВМ, " +
					"платой идёт чтение всего занятого объёма и слияние снапшота после каждого запуска"
			default:
				o.Rationale = "запасной вариант: «полный» через Backup API делает то же без временного снапшота " +
					"и его слияния после копирования"
			}
		case model.BackupConfig:
			o.Rationale = "секунды на выполнение; защищает от потери описания ВМ, но не от потери данных"
		case model.BackupOVA:
			o.Rationale = "переносимый самодостаточный артефакт для передачи ВМ в другую инсталляцию"
			o.Prerequisites = append(o.Prerequisites, "указать хост и каталог на нём, где движок создаст файл")
		}

		if o.Blocker != "" {
			o.Available = false
		}
	}

	// Exactly one recommendation: a list where everything is recommended is a
	// list where nothing is.
	switch {
	case incrementalReady:
		markRecommended(options, model.BackupIncremental,
			"ежедневные инкременты с периодическим полным — минимум нагрузки и минимум места")
	case backupAPI && a.Libvirt:
		markRecommended(options, model.BackupFull, "полная копия на горячую — ВМ продолжает работать")
	case backupAPI:
		markRecommended(options, model.BackupFull,
			"полная копия через Backup API без временного снапшота — ВМ продолжает работать")
	case a.DiskCount > 0:
		markRecommended(options, model.BackupSnapshot,
			"единственный способ снять полную копию этой ВМ без остановки на этом движке")
	default:
		markRecommended(options, model.BackupConfig, "у ВМ нет данных для копирования")
	}
	return options
}

// kvmRawBlocker — почему на KVM нет инкрементов, пока у ВМ есть raw-диски.
func kvmRawBlocker(raw int, names string) string {
	return fmt.Sprintf("на KVM инкремент требует qcow2 на всех дисках ВМ, а в raw — %d (%s): карту изменённых "+
		"блоков хранит только заголовок qcow2, и libvirt снимает такую ВМ полной копией. "+
		"Полная копия при этом доступна и снимается на горячую", raw, names)
}

// kvmRawPrerequisite — что сделать и что это даст.
func kvmRawPrerequisite(names string, used, increment int64) string {
	s := fmt.Sprintf("перевести в qcow2 диски %s (qemu-img convert на остановленной ВМ или перенос данных "+
		"на новый qcow2-диск)", names)
	if used > 0 && increment > 0 && increment < used {
		s += fmt.Sprintf(": сейчас каждый запуск читает их целиком — около %s, "+
			"а инкремент читал бы порядка %s", humanBytes(used), humanBytes(increment))
	}
	return s
}

func markRecommended(options []Option, typ model.BackupType, why string) {
	for i := range options {
		if options[i].Type == typ && options[i].Available {
			options[i].Recommended = true
			if options[i].Rationale == "" {
				options[i].Rationale = why
			} else {
				options[i].Rationale += "; " + why
			}
			return
		}
	}
}

// buildPresets offers complete schedules rather than individual settings, so
// an operator who does not want to think about retention does not have to.
//
// Готовые расписания гостя не замораживают: горячий бэкап идёт без остановки
// записи, а копия получается как после сбоя питания — журналируемые ФС и СУБД
// переживают это штатно. Заморозку и её цену администратор выбирает в задании
// сам, когда она нужна.
func buildPresets(a Assessment) []SchedulePreset {
	// Когда возможны инкременты и горячая полная копия — см. incrementalReady.
	backupAPI := a.backupAPI()
	incrementalReady := a.incrementalReady()

	increment := a.AverageIncrement
	if increment <= 0 {
		increment = a.FullBytes() / 20
	}

	presets := []SchedulePreset{
		{
			Name:        "Ежедневный инкремент, полный по воскресеньям",
			Description: "Инкремент каждую ночь в 01:00, полная копия раз в неделю. Хранение: 7 суточных, 4 недельных, 6 месячных.",
			Type:        model.BackupIncremental,
			Schedule:    "0 1 * * *",
			FullEvery:   7,
			Retention:   model.RetentionPolicy{KeepLast: 3, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6},
			VerifyAfter: model.VerifyChain,
			Recommended: incrementalReady,
			// 7 инкрементов + полная копия в неделю, на горизонте месяца.
			EstimatedFootprint: (increment*7 + a.FullBytes()) * 4,
		},
		{
			Name:               "Каждые 4 часа",
			Description:        "Инкремент каждые 4 часа, полная копия раз в сутки. Для систем, где терять больше нескольких часов нельзя.",
			Type:               model.BackupIncremental,
			Schedule:           "0 */4 * * *",
			FullEvery:          6,
			Retention:          model.RetentionPolicy{KeepLast: 6, KeepHourly: 12, KeepDaily: 7, KeepWeekly: 4},
			VerifyAfter:        model.VerifyQuick,
			Recommended:        false,
			EstimatedFootprint: (increment*6 + a.FullBytes()) * 7,
		},
		{
			Name:               "Еженедельная полная копия",
			Description:        "Одна полная копия в неделю по воскресеньям в 02:00. Просто и предсказуемо, места занимает больше всего.",
			Type:               pickFullType(backupAPI),
			Schedule:           "0 2 * * 0",
			FullEvery:          1,
			Retention:          model.RetentionPolicy{KeepLast: 2, KeepWeekly: 4, KeepMonthly: 3},
			VerifyAfter:        model.VerifyManifest,
			Recommended:        !incrementalReady,
			EstimatedFootprint: a.FullBytes() * 4,
		},
		{
			Name:               "Только конфигурация, ежедневно",
			Description:        "Описание ВМ каждую ночь. Дополнение к копиям данных, а не замена им.",
			Type:               model.BackupConfig,
			Schedule:           "30 0 * * *",
			FullEvery:          0,
			Retention:          model.RetentionPolicy{KeepDaily: 30},
			VerifyAfter:        model.VerifyQuick,
			Recommended:        false,
			EstimatedFootprint: 2 << 20,
		},
	}
	return presets
}

// pickFullType — полная копия через Backup API, если он есть, иначе через
// временный снапшот.
func pickFullType(backupAPI bool) model.BackupType {
	if backupAPI {
		return model.BackupFull
	}
	return model.BackupSnapshot
}
