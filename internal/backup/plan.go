package backup

import (
	"context"
	"fmt"
	"time"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store"
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

	// Наблюдаемая пропускная способность, байт/с. 0 — истории ещё нет.
	ObservedThroughput int64 `json:"observed_throughput"`
	// Средний объём инкремента по истории.
	AverageIncrement int64            `json:"average_increment"`
	LastBackupAt     *time.Time       `json:"last_backup_at,omitempty"`
	LastBackupType   model.BackupType `json:"last_backup_type,omitempty"`
	BackupCount      int              `json:"backup_count"`

	QemuImgAvailable bool `json:"qemu_img_available"`

	// Замечания — то, что стоит починить до настройки расписания.
	Warnings []string `json:"warnings,omitempty"`
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
		a.TotalUsed += d.ActualSize
	}
	a.DiskCount = len(a.Disks)

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
			"инкрементальный режим включён только на %d дисках из %d — инкременты для этой ВМ работать не будут, пока не включён на всех",
			a.CBTEnabled, a.CBTPossible))
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

	cbtReady := a.EngineSupportsCBT && a.CBTPossible > 0 && a.CBTEnabled == a.CBTPossible
	hasHistory := a.BackupCount > 0

	// Without measurements, assume a working day changes a few percent of the
	// allocated data — the figure most storage teams plan around.
	incrementEstimate := a.AverageIncrement
	if incrementEstimate <= 0 {
		incrementEstimate = a.TotalUsed / 20
	}

	options := []Option{
		{
			Type:            model.BackupFull,
			Title:           model.BackupFull.Title(),
			Available:       cbtReady,
			Impact:          "ВМ продолжает работать; движок держит точку согласованности на время чтения",
			EstimatedBytes:  a.TotalUsed,
			SuggestedVerify: model.VerifyManifest,
		},
		{
			Type:            model.BackupIncremental,
			Title:           model.BackupIncremental.Title(),
			Available:       cbtReady,
			Impact:          "ВМ продолжает работать; читаются только изменённые блоки",
			EstimatedBytes:  incrementEstimate,
			SuggestedVerify: model.VerifyChain,
		},
		{
			Type:            model.BackupDifferential,
			Title:           model.BackupDifferential.Title(),
			Available:       cbtReady,
			Impact:          "ВМ продолжает работать; читается всё, что изменилось с последнего полного",
			EstimatedBytes:  incrementEstimate * 5,
			SuggestedVerify: model.VerifyChain,
		},
		{
			Type:            model.BackupSnapshot,
			Title:           model.BackupSnapshot.Title(),
			Available:       a.DiskCount > 0,
			Impact:          "ВМ продолжает работать; после копирования снапшот сливается обратно — это нагружает СХД",
			EstimatedBytes:  a.TotalUsed,
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
			EstimatedBytes:  a.TotalUsed,
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
			case a.CBTPossible == 0:
				o.Blocker = fmt.Sprintf(
					"все диски ВМ (%d) в формате, не поддерживающем отслеживание изменённых блоков. "+
						"Это не мешает бэкапу: выберите «%s» — копия будет полной и снимется без остановки ВМ",
					a.RawDisks, model.BackupSnapshot.Title())
				o.Prerequisites = append(o.Prerequisites,
					"для инкрементов диски нужно перевести в qcow2 (тонкое выделение)")
			case a.CBTEnabled < a.CBTPossible:
				o.Blocker = fmt.Sprintf("инкрементальный режим включён на %d дисках из %d", a.CBTEnabled, a.CBTPossible)
				o.Prerequisites = append(o.Prerequisites, "включить режим incremental на всех дисках ВМ")
			default:
				o.Rationale = "база для инкрементов: создаёт checkpoint, от которого считаются все последующие копии"
			}
		case model.BackupIncremental:
			switch {
			case o.Blocker != "":
			case a.CBTPossible == 0:
				o.Blocker = fmt.Sprintf(
					"инкремент опирается на карту изменённых блоков, а её негде хранить: " +
						"диски в формате, где нет заголовка qcow2. Полная копия при этом доступна и снимается на горячую")
				o.Prerequisites = append(o.Prerequisites,
					"перевести диски в qcow2 (тонкое выделение) — иначе инкременты невозможны в принципе")
			case !cbtReady:
				o.Blocker = "требуется включённое отслеживание изменённых блоков"
				o.Prerequisites = append(o.Prerequisites, "включить режим incremental на всех дисках ВМ")
			case !hasHistory:
				o.Rationale = "первый запуск автоматически станет полным, дальше будут копироваться только изменения"
			default:
				o.Rationale = fmt.Sprintf("самый дешёвый вариант: по истории в среднем %s за запуск",
					humanBytes(incrementEstimate))
			}
		case model.BackupDifferential:
			if a.CBTPossible == 0 {
				o.Blocker = "разностный бэкап тоже опирается на карту изменённых блоков, " +
					"а формат дисков её не поддерживает"
				o.Prerequisites = append(o.Prerequisites,
					"перевести диски в qcow2 (тонкое выделение)")
			} else if !cbtReady {
				o.Blocker = "требуется включённое отслеживание изменённых блоков"
				o.Prerequisites = append(o.Prerequisites, "включить режим incremental на всех дисках ВМ")
			} else {
				o.Rationale = "компромисс: копий больше, чем при инкрементах, зато восстановление всегда из двух точек"
			}
		case model.BackupSnapshot:
			if a.DiskCount == 0 {
				o.Blocker = "у ВМ нет дисков с данными"
			} else if a.CBTPossible == 0 {
				o.Rationale = "единственный способ для дисков без qcow2 — и полноценный: " +
					"копия снимается без остановки ВМ и содержит все данные, " +
					"платой идёт чтение всего занятого объёма при каждом запуске"
			} else if !cbtReady {
				o.Rationale = "работает всегда, независимо от формата дисков и версии движка"
			} else {
				o.Rationale = "запасной вариант: полная копия без CBT, но каждый раз читается весь занятый объём"
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
	case cbtReady:
		markRecommended(options, model.BackupIncremental,
			"ежедневные инкременты с периодическим полным — минимум нагрузки и минимум места")
	case a.DiskCount > 0:
		markRecommended(options, model.BackupSnapshot,
			"единственный способ снять полную копию этой ВМ без остановки")
	default:
		markRecommended(options, model.BackupConfig, "у ВМ нет данных для копирования")
	}
	return options
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
func buildPresets(a Assessment) []SchedulePreset {
	cbtReady := a.EngineSupportsCBT && a.CBTPossible > 0 && a.CBTEnabled == a.CBTPossible

	increment := a.AverageIncrement
	if increment <= 0 {
		increment = a.TotalUsed / 20
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
			Quiesce:     a.GuestAgent,
			Recommended: cbtReady,
			// 7 инкрементов + полная копия в неделю, на горизонте месяца.
			EstimatedFootprint: (increment*7 + a.TotalUsed) * 4,
		},
		{
			Name:               "Каждые 4 часа — короткий RPO",
			Description:        "Инкремент каждые 4 часа, полная копия раз в сутки. Для систем, где терять больше нескольких часов нельзя.",
			Type:               model.BackupIncremental,
			Schedule:           "0 */4 * * *",
			FullEvery:          6,
			Retention:          model.RetentionPolicy{KeepLast: 6, KeepHourly: 12, KeepDaily: 7, KeepWeekly: 4},
			VerifyAfter:        model.VerifyQuick,
			Quiesce:            a.GuestAgent,
			Recommended:        false,
			EstimatedFootprint: (increment*6 + a.TotalUsed) * 7,
		},
		{
			Name:               "Еженедельная полная копия",
			Description:        "Одна полная копия в неделю по воскресеньям в 02:00. Просто и предсказуемо, места занимает больше всего.",
			Type:               pickFullType(cbtReady),
			Schedule:           "0 2 * * 0",
			FullEvery:          1,
			Retention:          model.RetentionPolicy{KeepLast: 2, KeepWeekly: 4, KeepMonthly: 3},
			VerifyAfter:        model.VerifyManifest,
			Quiesce:            a.GuestAgent,
			Recommended:        !cbtReady,
			EstimatedFootprint: a.TotalUsed * 4,
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

func pickFullType(cbtReady bool) model.BackupType {
	if cbtReady {
		return model.BackupFull
	}
	return model.BackupSnapshot
}
