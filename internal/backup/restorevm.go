package backup

import (
	"fmt"
	"time"

	"adveng/jh_virt/internal/model"
)

// Построение плана восстановления машины целиком.
//
// План строится до единого обращения к движку и до единого записанного байта.
// Смысл в том, чтобы всё, что можно узнать заранее, было сказано заранее:
// сколько дисков и какого объёма, хватает ли места, чем эта машина будет
// отличаться от исходной. Восстановление терабайта, начатое по ошибке, стоит
// часов работы хранилища, а прерванное на середине оставляет полусобранную
// машину, которую надо убирать руками.

// RestoreVMInput — то, из чего строится план.
type RestoreVMInput struct {
	Run     *model.BackupRun
	Profile *VMProfile
	Disks   []*DiskManifest
	// FreeBytes — свободное место в целевом домене хранения; -1, если движок
	// не сообщил. Отсутствие сведений не повод отказывать, но повод сказать.
	FreeBytes int64
	Request   *model.RestoreVMRequest
	Now       time.Time
}

// BuildRestoreVMPlan describes what a full VM restore would do.
func BuildRestoreVMPlan(in RestoreVMInput) (*model.RestoreVMPlan, error) {
	if in.Run == nil {
		return nil, fmt.Errorf("нет точки восстановления")
	}
	if in.Request == nil {
		return nil, fmt.Errorf("нет запроса на восстановление")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	name := in.Request.Name
	if name == "" {
		name = model.RestoredVMName(in.Run.VMName, now)
	}

	plan := &model.RestoreVMPlan{
		RunID:     in.Run.ID,
		VMName:    in.Run.VMName,
		NewName:   name,
		ServerID:  firstNonEmpty(in.Request.ServerID, in.Run.ServerID),
		Created:   now,
		FreeBytes: in.FreeBytes,
		Network:   in.Request.NetworkOrDefault(),
		Start:     in.Request.Start,
	}

	// Раскладка дисков берётся из профиля: в нём записано, на какой шине и в
	// каком порядке загрузки диск стоял у исходной машины. Без этого система
	// соберёт машину, которая не загрузится, — диски будут на месте, а
	// загрузчик их не найдёт.
	layout := map[string]VMProfileDisk{}
	if in.Profile != nil {
		for _, d := range in.Profile.Disks {
			layout[d.DiskID] = d
		}
	}

	for _, m := range in.Disks {
		if m == nil {
			continue
		}
		entry := model.RestoreVMPlanDisk{
			DiskID:      m.DiskID,
			Alias:       m.Alias,
			VirtualSize: m.VirtualSize,
			Bootable:    m.Bootable,
		}
		if l, ok := layout[m.DiskID]; ok {
			entry.Target, entry.Bus = l.Target, l.Bus
			entry.Bootable = entry.Bootable || l.BootOrder == 1
		}
		plan.Disks = append(plan.Disks, entry)
		plan.TotalBytes += m.VirtualSize
	}

	plan.Blockers = restoreVMBlockers(in, plan)
	plan.Warnings = restoreVMWarnings(in, plan)
	return plan, nil
}

// restoreVMBlockers перечисляет причины, по которым восстановление не начнётся.
func restoreVMBlockers(in RestoreVMInput, plan *model.RestoreVMPlan) []string {
	var out []string

	if len(plan.Disks) == 0 {
		out = append(out, "в точке восстановления нет ни одного диска")
	}

	// Незавершённая копия восстанавливается в машину, у которой часть дисков
	// пуста. Такая машина выглядит собранной и даже может загрузиться — а
	// данных на ней нет. Разрешаем только явным подтверждением.
	if in.Run.Status == model.RunPartial && !in.Request.Confirm {
		out = append(out, "копия неполная: часть дисков не сохранена. "+
			"Восстановление даст машину с пустыми дисками — подтвердите, если это то, что нужно")
	}
	if in.Run.Status == model.RunFailed || in.Run.Status == model.RunCanceled {
		out = append(out, fmt.Sprintf("копия не годится для восстановления: состояние %s", in.Run.Status))
	}

	// Места должно хватить на полный виртуальный размер, а не на сжатый: диски
	// создаются исходного объёма. Отказ здесь дешевле, чем отказ на середине
	// заливки, после которого в домене остаются недоделанные диски.
	if plan.FreeBytes >= 0 && plan.TotalBytes > plan.FreeBytes {
		out = append(out, fmt.Sprintf("в домене хранения не хватает места: нужно %s, свободно %s",
			humanBytes(plan.TotalBytes), humanBytes(plan.FreeBytes)))
	}

	return out
}

// restoreVMWarnings перечисляет то, что не мешает начать, но должно быть
// прочитано до, а не после.
func restoreVMWarnings(in RestoreVMInput, plan *model.RestoreVMPlan) []string {
	var out []string

	if in.Profile == nil {
		out = append(out, "конфигурация исходной машины не сохранена: "+
			"диски будут подключены в порядке по умолчанию, загрузка может потребовать правки")
	}

	if plan.Network == model.RestoreNetworkAttached {
		out = append(out, "сеть будет подключена: если оригинал ещё работает, "+
			"в сети окажутся две машины с одним адресом и именем")
	}

	if plan.Start {
		out = append(out, "машина будет запущена сразу после сборки")
	}

	if plan.FreeBytes < 0 {
		out = append(out, "движок не сообщил свободное место — проверьте домен хранения сами")
	} else if plan.TotalBytes > 0 && plan.FreeBytes-plan.TotalBytes < plan.TotalBytes/10 {
		// Меньше десятой части запаса: место кончится на первом же снапшоте
		// или на росте тонкого диска.
		out = append(out, fmt.Sprintf("после восстановления в домене останется %s — этого мало для работы",
			humanBytes(plan.FreeBytes-plan.TotalBytes)))
	}

	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
