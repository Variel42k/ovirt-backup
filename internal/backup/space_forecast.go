package backup

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Прогноз места под горячий бэкап.
//
// Пока бэкап открыт, гость пишет как обычно, а место под это нужно рядом с
// дисками: на oVirt записи или отложенные блоки копятся на домене хранения
// (scratch-диски, слой снапшота), на KVM — в scratch-файлах на хосте.
// Сторожа места закрывают бэкап раньше, чем место кончится: ВМ продолжает
// работать, но копии не будет. Прогноз предупреждает об этом заранее — на
// странице ВМ и в форме задания.
//
// Оценка — сколько гость записал на диски, пока шли прошлые бэкапы, по
// замерам мониторинга запуска. Это верхняя граница: блок, переписанный
// дважды, откладывается один раз, а уже прочитанный — не откладывается вовсе.
//
// Прогноз только советует. Он не запрещает запуск и ничего в нём не меняет:
// до старта и по ходу решают те же сторожа, что и без него.

// SpaceStatus — чем грозит место горячему бэкапу.
type SpaceStatus string

const (
	// SpaceOK — записи прошлых бэкапов помещаются с запасом.
	SpaceOK SpaceStatus = "ok"
	// SpaceTight — записи занимали больше половины доступного места.
	SpaceTight SpaceStatus = "tight"
	// SpaceShort — записи прошлых бэкапов не поместились бы: сторож, скорее
	// всего, закроет бэкап раньше конца.
	SpaceShort SpaceStatus = "short"
	// SpaceNoStart — свободно меньше порога: бэкап не начнётся.
	SpaceNoStart SpaceStatus = "no_start"
	// SpaceUnknown — неизвестно свободное место или сколько пишет гость.
	SpaceUnknown SpaceStatus = "unknown"
)

// Где копятся записи гостя, пока бэкап открыт.
const (
	SpaceStorageDomain = "storage_domain"
	SpaceScratch       = "scratch"
)

const (
	// forecastRuns — по скольким последним бэкапам ВМ ищется пик записи.
	forecastRuns = 20
	// forecastPeriod — насколько давние бэкапы ещё учитываются. Замеры
	// всё равно живут не дольше срока хранения мониторинга.
	forecastPeriod = 30 * 24 * time.Hour
)

// SpaceForecast — прогноз по всем местам, где копятся записи гостя.
type SpaceForecast struct {
	// Runs — по скольким прошлым бэкапам есть замеры записи гостя.
	Runs int `json:"runs"`
	// VMRunning — выключенная ВМ не пишет, и места бэкапу не нужно:
	// предупреждений для неё нет.
	VMRunning bool         `json:"vm_running"`
	Places    []SpacePlace `json:"places"`
	Warnings  []string     `json:"warnings,omitempty"`
}

// SpacePlace — одно место: домен хранения oVirt или каталог scratch на хосте KVM.
type SpacePlace struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	// Need — больше всего байт, записанных гостем на диски этого места за один
	// прошлый бэкап; -1 — замеров нет.
	Need int64 `json:"need"`
	// Free — свободно сейчас; -1 — неизвестно.
	Free int64 `json:"free"`
	// Reserve — запас: когда свободного остаётся меньше, сторож закрывает бэкап.
	Reserve int64 `json:"reserve"`
	// StartMin — при меньшем свободном месте бэкап не начнётся.
	StartMin int64 `json:"start_min"`
	// MeasuredAt — когда замерено свободное место.
	MeasuredAt *time.Time  `json:"measured_at,omitempty"`
	Status     SpaceStatus `json:"status"`
}

// Room — сколько места бэкап может занять, прежде чем сработает сторож.
func (p SpacePlace) Room() int64 { return p.Free - p.Reserve }

func (p *SpacePlace) evaluate() {
	switch {
	case p.Free < 0:
		p.Status = SpaceUnknown
	case p.Free < p.StartMin:
		p.Status = SpaceNoStart
	case p.Need < 0:
		p.Status = SpaceUnknown
	case p.Need >= p.Room():
		p.Status = SpaceShort
	case p.Need*2 >= p.Room():
		p.Status = SpaceTight
	default:
		p.Status = SpaceOK
	}
}

func (p SpacePlace) label() string {
	if p.Kind == SpaceScratch {
		return fmt.Sprintf("каталог scratch «%s» на хосте", p.Name)
	}
	return fmt.Sprintf("домен хранения «%s»", p.Name)
}

func (p SpacePlace) warning() string {
	switch p.Status {
	case SpaceNoStart:
		return fmt.Sprintf("%s: свободно %s, а горячий бэкап начинается, только если свободно не меньше %s. "+
			"Запуск будет отклонён до начала копирования, ВМ это не затронет. Освободите место",
			p.label(), humanBytes(p.Free), humanBytes(p.StartMin))
	case SpaceShort:
		return fmt.Sprintf("%s: пока шли прошлые бэкапы, гость записывал на диски до %s за запуск, "+
			"и столько же места может понадобиться, пока бэкап открыт. Сверх запаса сторожа свободно %s — "+
			"сторож, скорее всего, закроет бэкап раньше конца. ВМ продолжит работать, но копии не будет. "+
			"Освободите место или снимайте копию, когда ВМ пишет меньше",
			p.label(), humanBytes(p.Need), humanBytes(p.Room()))
	case SpaceTight:
		return fmt.Sprintf("%s: пока шли прошлые бэкапы, гость записывал на диски до %s за запуск — "+
			"больше половины свободного сверх запаса сторожа (%s). Если ВМ станет писать больше, "+
			"сторож может закрыть бэкап раньше конца",
			p.label(), humanBytes(p.Need), humanBytes(p.Room()))
	}
	return ""
}

// finish оценивает все места и собирает предупреждения.
func (f *SpaceForecast) finish() {
	f.Warnings = nil
	for i := range f.Places {
		f.Places[i].evaluate()
		if !f.VMRunning {
			continue
		}
		if w := f.Places[i].warning(); w != "" {
			f.Warnings = append(f.Warnings, w)
		}
	}
}

// SetPlaceFree дописывает в прогноз свободное место, которого движок бэкапов
// сам не видит (каталог scratch на хосте KVM), и пересчитывает предупреждения
// оценки.
func (a *Assessment) SetPlaceFree(kind string, free, reserve, startMin int64, at time.Time) {
	if a.Space == nil {
		return
	}
	stale := map[string]bool{}
	for _, w := range a.Space.Warnings {
		stale[w] = true
	}
	kept := make([]string, 0, len(a.Warnings))
	for _, w := range a.Warnings {
		if !stale[w] {
			kept = append(kept, w)
		}
	}
	at = at.UTC()
	for i := range a.Space.Places {
		p := &a.Space.Places[i]
		if p.Kind != kind {
			continue
		}
		p.Free, p.Reserve, p.StartMin = free, reserve, startMin
		p.MeasuredAt = &at
	}
	a.Space.finish()
	a.Warnings = append(kept, a.Space.Warnings...)
}

// forecastDisk — диск, который попадёт в копию, и место, где копятся его записи.
type forecastDisk struct {
	place string
	size  int64
}

// peakWrites — для каждого места наибольший объём записи гостя за один
// прошлый бэкап и число бэкапов с замерами. Диски не из disks не считаются:
// в копию они не попадают, и места бэкапу не занимают. Вклад диска не больше
// его размера: отложить больше, чем весь диск, нельзя.
func peakWrites(writes []store.RunDiskWrites, disks map[string]forecastDisk) (map[string]int64, int) {
	perRun := map[string]map[string]int64{}
	for _, w := range writes {
		d, ok := disks[w.Disk]
		if !ok {
			continue
		}
		b := max(w.Bytes, 0)
		if d.size > 0 && b > d.size {
			b = d.size
		}
		if perRun[w.RunID] == nil {
			perRun[w.RunID] = map[string]int64{}
		}
		perRun[w.RunID][d.place] += b
	}
	peak := map[string]int64{}
	for _, byPlace := range perRun {
		for place, b := range byPlace {
			if cur, ok := peak[place]; !ok || b > cur {
				peak[place] = b
			}
		}
	}
	return peak, len(perRun)
}

// forecastSpace строит прогноз места для горячего бэкапа ВМ. Свободное место
// доменов oVirt берётся из инвентаря; место под scratch на хосте KVM движок
// бэкапов не видит, его дописывает диспетчер (Assessment.SetPlaceFree).
// Ошибка чтения истории прогноз не отменяет: он просто останется без оценки.
func (e *Engine) forecastSpace(ctx context.Context, srv *model.Server, vm *model.VM, disks []*model.Disk) *SpaceForecast {
	libvirt := srv.Kind.UsesLibvirt()
	if !libvirt && !srv.Kind.UsesOVirtAPI() {
		return nil
	}
	f := &SpaceForecast{VMRunning: vm.Running()}

	backed := map[string]forecastDisk{}
	places := map[string]*SpacePlace{}
	for _, d := range disks {
		if (d.ContentType != "" && d.ContentType != "data") || d.StorageType == "lun" || d.Shareable {
			continue
		}
		key, kind, name := d.StorageDomainID, SpaceStorageDomain, d.StorageDomain
		if libvirt {
			key, kind, name = SpaceScratch, SpaceScratch, srv.ScratchDirOrDefault()
		}
		if key == "" {
			continue
		}
		if name == "" {
			name = key
		}
		backed[d.Alias] = forecastDisk{place: key, size: d.ProvisionedSize}
		if places[key] == nil {
			places[key] = &SpacePlace{Kind: kind, Name: name, Need: -1, Free: -1}
			if kind == SpaceStorageDomain {
				places[key].ID = key
			}
		}
	}
	if len(places) == 0 {
		return nil
	}

	since := time.Now().Add(-forecastPeriod)
	runs, err := e.store.ListBackupRuns(ctx, store.RunFilter{
		ServerID: srv.ID,
		VMID:     vm.ID,
		Types:    []model.BackupType{model.BackupFull, model.BackupIncremental, model.BackupDifferential, model.BackupSnapshot},
		Statuses: []model.RunStatus{model.RunSucceeded, model.RunPartial, model.RunFailed, model.RunCanceled},
		Since:    &since,
		Limit:    forecastRuns,
	})
	if err != nil {
		e.log.Debug().Err(err).Str("vm", vm.Name).Msg("прогноз места: не удалось прочитать прошлые бэкапы")
	} else if len(runs) > 0 {
		ids := make([]string, 0, len(runs))
		for _, r := range runs {
			ids = append(ids, r.ID)
		}
		writes, err := e.store.GuestWritesDuringRuns(ctx, srv.ID, ids)
		if err != nil {
			e.log.Debug().Err(err).Str("vm", vm.Name).Msg("прогноз места: не удалось прочитать замеры записи")
		}
		var peak map[string]int64
		peak, f.Runs = peakWrites(writes, backed)
		for key, need := range peak {
			places[key].Need = need
		}
	}

	if !libvirt {
		domains, err := e.store.ListStorageDomains(ctx, srv.ID)
		if err != nil {
			e.log.Debug().Err(err).Str("vm", vm.Name).Msg("прогноз места: не удалось прочитать домены хранения")
		}
		for _, sd := range domains {
			p := places[sd.ID]
			total := sd.AvailableSize + sd.UsedSize
			if p == nil || total <= 0 {
				continue
			}
			p.Free = sd.AvailableSize
			// Те же пороги, что у проверки перед бэкапом и сторожа домена.
			p.Reserve = DomainReserve(total)
			p.StartMin = 2 * p.Reserve
			if !sd.SeenAt.IsZero() {
				seen := sd.SeenAt.UTC()
				p.MeasuredAt = &seen
			}
			if sd.Name != "" {
				p.Name = sd.Name
			}
		}
	}

	for _, p := range places {
		f.Places = append(f.Places, *p)
	}
	sort.Slice(f.Places, func(i, j int) bool { return f.Places[i].Name < f.Places[j].Name })
	f.finish()
	return f
}
