package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Блокировка диска между томами цепочки на oVirt 4.3.
//
// Движок без Backup API отдаёт тома снапшота по одному: каждый том — своя
// передача того же диска, а новую передачу движок открывает только при
// свободном диске. Иначе — HTTP 409 «The following disks are locked». На
// стенде 4.3 видно, что после finalize предыдущего тома передача уже
// сообщает конечную фазу, а диск остаётся занят десятки минут; причину
// движок в статусах может и не показать.
//
// Молча ждать 10 минут, а потом ещё столько же — худшее, что можно сделать:
// оператор видит «зависший» бэкап и не знает, что происходит. Поэтому служба:
//
//   - пока движок отвечает 409, раз в минуту спрашивает его, что держит диск:
//     передачи диска с фазами, статус диска, тома в статусе locked, снапшоты
//     в операции, — и пишет это в журнал и в хронологию запуска;
//   - сама отменяет передачу, если её открыл этот же запуск (том уже скачан
//     или брошен, передача никому не нужна);
//   - по истечении backup.transfer.lock_wait останавливается с диагнозом по
//     тому, что показал движок, и с командами в блоке «Что сделать вручную».

// volumeDisk — чей том скачивается: по нему опрашивается блокировка.
type volumeDisk struct {
	vmID, vmName, diskID, alias string
}

type volumeDiskKey struct{}

// withVolumeDisk помечает контекст диском, тома которого скачиваются.
func withVolumeDisk(ctx context.Context, vm *model.VM, disk ovirt.Disk) context.Context {
	d := volumeDisk{diskID: disk.ID, alias: disk.AliasOrName()}
	if vm != nil {
		d.vmID, d.vmName = vm.ID, vm.Name
	}
	return context.WithValue(ctx, volumeDiskKey{}, d)
}

func volumeDiskFrom(ctx context.Context) (volumeDisk, bool) {
	d, ok := ctx.Value(volumeDiskKey{}).(volumeDisk)
	return d, ok && d.diskID != ""
}

// legacyLockProbeInterval — как часто спрашивать движок о причине 409.
var legacyLockProbeInterval = time.Minute

// diskLockState — что движок показывает о занятости диска.
type diskLockState struct {
	DiskStatus string
	// LockedVolumes — тома диска (image_id) в статусе locked в снапшотах.
	LockedVolumes []string
	// BusySnapshots — снапшоты ВМ в операции (создание, удаление со слиянием).
	BusySnapshots []string
	// Transfers — незавершённые передачи этого диска.
	Transfers []ovirt.ImageTransfer
	// Events — последние события движка по ВМ и диску (его журнал через API):
	// по ним видно, чем закончилась передача, не заходя на хост движка.
	Events []string
	// ProbeError — движок не ответил на часть запросов: картина неполная.
	ProbeError string
}

// visibleCause — движок сам показывает, чем занят диск.
func (s diskLockState) visibleCause() bool {
	return len(s.Transfers) > 0 || len(s.BusySnapshots) > 0 || len(s.LockedVolumes) > 0 || s.DiskStatus == "locked"
}

func (s diskLockState) String() string {
	var parts []string
	if s.DiskStatus != "" {
		parts = append(parts, "статус диска "+s.DiskStatus)
	}
	if len(s.Transfers) > 0 {
		var ts []string
		for _, t := range s.Transfers {
			ts = append(ts, fmt.Sprintf("%s (фаза %s)", t.ID, t.Phase))
		}
		parts = append(parts, "передачи диска: "+strings.Join(ts, ", "))
	} else {
		parts = append(parts, "незавершённых передач диска нет")
	}
	if len(s.LockedVolumes) > 0 {
		parts = append(parts, "тома в статусе locked: "+strings.Join(s.LockedVolumes, ", "))
	}
	if len(s.BusySnapshots) > 0 {
		parts = append(parts, "снапшоты в операции: "+strings.Join(s.BusySnapshots, ", "))
	}
	if s.ProbeError != "" {
		parts = append(parts, "часть сведений недоступна: "+s.ProbeError)
	}
	return strings.Join(parts, "; ")
}

// probeDiskLock спрашивает движок, что сейчас держит диск.
func probeDiskLock(ctx context.Context, client *ovirt.Client, d volumeDisk) diskLockState {
	var st diskLockState
	var errs []string
	if disk, err := client.GetDisk(ctx, d.diskID); err == nil {
		st.DiskStatus = disk.Status
	} else {
		errs = append(errs, "диск: "+err.Error())
	}
	refs := map[string]struct{}{}
	if snaps, err := client.ListSnapshots(ctx, d.vmID); err == nil {
		for _, s := range snaps {
			if s.SnapshotStatus == "locked" {
				st.BusySnapshots = append(st.BusySnapshots, fmt.Sprintf("«%s» (%s)", s.Description, s.ID))
			}
			disks, err := client.ListSnapshotDisks(ctx, d.vmID, s.ID)
			if err != nil {
				continue
			}
			for _, sd := range disks {
				if sd.ID != d.diskID || sd.ImageID == "" {
					continue
				}
				refs[sd.ImageID] = struct{}{}
				if sd.Status == "locked" && !containsString(st.LockedVolumes, sd.ImageID) {
					st.LockedVolumes = append(st.LockedVolumes, sd.ImageID)
				}
			}
		}
	} else {
		errs = append(errs, "снапшоты: "+err.Error())
	}
	if transfers, err := client.ListImageTransfers(ctx); err == nil {
		st.Transfers = transfersOnDisks(transfers, []string{d.diskID}, refs)
	} else {
		errs = append(errs, "передачи: "+err.Error())
	}
	st.Events = recentEngineEvents(ctx, client, legacyLockEvents, d.alias, d.vmName)
	st.ProbeError = strings.Join(errs, "; ")
	return st
}

// legacyLockEvents — сколько последних событий движка показывать.
const legacyLockEvents = 6

// recentEngineEvents — последние события журнала движка, где упомянуты ВМ
// или диск, новые сверху. Журнал движка доступен через REST API с теми же
// правами, что у службы, — доступ к хосту движка не нужен.
func recentEngineEvents(ctx context.Context, client *ovirt.Client, limit int, needles ...string) []string {
	events, err := client.ListEvents(ctx, 300, "")
	if err != nil {
		return nil
	}
	var out []string
	for _, ev := range events {
		match := false
		for _, n := range needles {
			if n != "" && strings.Contains(ev.Description, n) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		line := ev.Description
		if at := ev.Time.Time(); !at.IsZero() {
			line = at.Local().Format("02.01 15:04:05") + " " + line
		}
		if ev.Severity != "" && ev.Severity != "normal" {
			line += " [" + ev.Severity + "]"
		}
		out = append(out, line)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// diskLockedError — движок так и не отпустил диск для следующего тома.
type diskLockedError struct {
	disk   volumeDisk
	waited time.Duration
	state  diskLockState
	cause  error
}

func (e *diskLockedError) Unwrap() error { return e.cause }

func (e *diskLockedError) Error() string {
	msg := fmt.Sprintf("движок %s не отпускал диск %s для передачи следующего тома (HTTP 409 «disks are locked»). "+
		"Что показывал движок: %s. %s",
		humanDuration(e.waited), e.disk.alias, e.state, e.diagnosis())
	if len(e.state.Events) > 0 {
		msg += ". Последние события движка: " + strings.Join(e.state.Events, "; ")
	}
	return msg
}

// diagnosis объясняет, что делать, по тому, что показал движок.
func (e *diskLockedError) diagnosis() string {
	st := e.state
	switch {
	case len(st.Transfers) > 0:
		return "Диск держит незавершённая передача, которую служба не открывала в этом запуске: её мог открыть " +
			"администратор или другая система. Если она брошена, отмените её командой из блока «Что сделать вручную» " +
			"и повторите бэкап"
	case len(st.BusySnapshots) > 0:
		return "Со снапшотом ВМ идёт операция (создание или удаление со слиянием) — прерывать её нельзя. " +
			"Повторите бэкап, когда снапшот выйдет из статуса locked"
	case len(st.LockedVolumes) > 0 || st.DiskStatus == "locked":
		return "Тома диска остались в статусе locked в базе движка, хотя передач и операций со снапшотами нет: " +
			"так oVirt 4.3 оставляет том после отменённой передачи. Через API её не снять: нужен " +
			"unlock_entity.sh на хосте движка — передайте команду из блока «Что сделать вручную» администратору oVirt"
	default:
		return "Движок не показывает причину ни в передачах, ни в статусах диска и снапшотов: блокировку держит " +
			"незавершённая внутренняя команда движка (после передачи предыдущего тома). Через API её не снять; " +
			"она снимается перезапуском службы ovirt-engine (ВМ продолжают работать) — это делает администратор " +
			"oVirt с доступом к хосту движка. Проверить, отпустил ли движок диск, можно кнопкой «Проверить» в " +
			"остатках бэкапов ВМ: там видны передачи, статусы и события движка"
	}
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d с", int(d.Seconds()))
	}
	return fmt.Sprintf("%d мин", int(d.Round(time.Minute).Minutes()))
}

// lockWait — сколько ждать, пока движок отпустит диск.
func (e *Engine) lockWait() time.Duration {
	if e.cfg.Transfer.LockWait != 0 {
		return e.cfg.Transfer.LockWait
	}
	return legacyTransferOpenWait
}

// openVolumeTransfer открывает передачу тома, пока движок держит диск —
// с опросом причины и отменой собственной незакрытой передачи запуска.
func (e *Engine) openVolumeTransfer(ctx context.Context, client *ovirt.Client,
	req ovirt.TransferRequest) (*ovirt.ImageTransfer, error) {

	d, known := volumeDiskFrom(ctx)
	run, _ := ctx.Value(transferOwnerKey{}).(*model.BackupRun)
	started := time.Now()
	deadline := started.Add(e.lockWait())
	var (
		state     diskLockState
		probed    bool
		lastProbe time.Time
		lastSaid  string
	)
	for {
		transfer, err := client.CreateTransfer(ctx, req)
		if err == nil {
			if probed {
				e.log.Info().Str("диск", d.alias).Dur("ждали", time.Since(started).Round(time.Second)).
					Msg("движок отпустил диск — передача следующего тома открыта")
				e.event(ctx, run, model.RunEventDiskLockWait, time.Since(started),
					fmt.Sprintf("диск %s освобождён движком через %s", d.alias, humanDuration(time.Since(started))))
			}
			return transfer, nil
		}
		if !ovirt.IsConflict(err) {
			return nil, err
		}
		if known && time.Since(lastProbe) >= legacyLockProbeInterval {
			lastProbe = time.Now()
			state = probeDiskLock(ctx, client, d)
			if !probed {
				probed = true
				e.event(ctx, run, model.RunEventDiskLockWait, 0,
					fmt.Sprintf("движок держит диск %s (409): %s", d.alias, state))
			}
			if said := state.String(); said != lastSaid {
				lastSaid = said
				e.log.Warn().Str("диск", d.alias).Dur("ждём", time.Since(started).Round(time.Second)).
					Str("что показывает движок", said).Msg("движок держит диск — передача следующего тома ждёт")
			}
			e.releaseOwnRunTransfers(ctx, client, run, state.Transfers)
		}
		if time.Now().After(deadline) {
			if !known {
				return nil, fmt.Errorf("engine не снял блокировку за %s: %w", time.Since(started).Round(time.Second), err)
			}
			return nil, &diskLockedError{disk: d, waited: time.Since(started), state: state, cause: err}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(legacyLockRetryInterval):
		}
	}
}

// legacyLockRetryInterval — пауза между попытками открыть передачу.
var legacyLockRetryInterval = 5 * time.Second

// releaseOwnRunTransfers отменяет передачи этого диска, открытые этим же
// запуском: раз запуск открывает следующую, прежняя ему уже не нужна.
func (e *Engine) releaseOwnRunTransfers(ctx context.Context, client *ovirt.Client, run *model.BackupRun,
	transfers []ovirt.ImageTransfer) {

	if run == nil || len(transfers) == 0 {
		return
	}
	owners, err := e.store.TransferOwners(ctx, run.ServerID, run.VMID)
	if err != nil {
		return
	}
	var own []ovirt.ImageTransfer
	for _, t := range transfers {
		if owners[t.ID] == run.ID {
			own = append(own, t)
		}
	}
	for _, t := range own {
		if err := e.cancelTransfersAndWait(ctx, client, []ovirt.ImageTransfer{t})[t.ID]; err == nil {
			e.event(ctx, run, model.RunEventLeftoverClosed, 0,
				fmt.Sprintf("отменена собственная незакрытая передача %s (фаза %s): она держала диск", t.ID, t.Phase))
		}
	}
}

// legacyLockSteps — команды для оператора, когда движок так и не отпустил диск.
// Сначала — то, что выполняется через REST API движка с сервера службы; шаги
// на хосте движка помечены: без доступа к нему их выполняет администратор oVirt.
func legacyLockSteps(srv *model.Server, vmID string, lockErr *diskLockedError) []model.ManualStep {
	cmd := newEngineCommands(srv)
	steps := EngineUnlockSteps(srv, vmID, nil, lockErr.state.Transfers, []string{lockErr.disk.diskID})
	needle := lockErr.disk.alias
	if needle == "" {
		needle = lockErr.disk.diskID
	}
	steps = append(steps, model.ManualStep{
		Title: "События движка по диску (журнал движка через API)",
		Where: anywhere,
		Detail: "Доступ к хосту движка не нужен. Видно, чем закончилась передача тома и что движок делал с " +
			"диском дальше: «Image Download … succeeded», ошибки, операции со снапшотами.",
		Command: cmd.curl + " " + shellQuote(cmd.api+"/events?max=300") +
			" | grep -oE '\"description\" *: *\"[^\"]*" + strings.ReplaceAll(needle, "'", "") + "[^\"]*\"' | head -20",
	})
	steps = append(steps, model.ManualStep{
		Title: "Посмотреть в журнале на хосте движка, что держит диск",
		Where: "хост движка (если доступа нет — передайте администратору oVirt)",
		Detail: "Последние записи по диску: какая команда его заблокировала и чем она закончилась. " +
			"Ищите TransferDiskImageCommand, CreateSnapshot, RemoveSnapshot и строки с EngineLock.",
		Command: "sudo grep " + shellQuote(lockErr.disk.diskID) + " /var/log/ovirt-engine/engine.log | tail -60",
	})
	if !lockErr.state.visibleCause() {
		steps = append(steps, model.ManualStep{
			Title: "Если причины не видно: перезапустить службу движка",
			Where: "хост движка, root (если доступа нет — передайте администратору oVirt)",
			Risky: true,
			Detail: "Блокировку держит незавершённая команда в памяти движка, через API её не снять. Перезапуск " +
				"ovirt-engine её снимает; ВМ продолжают работать, но идущие через движок операции (миграции, " +
				"снапшоты, передачи) прервутся, а портал будет недоступен пару минут. Выполняйте, когда на движке " +
				"нет других операций.",
			Command: "sudo systemctl restart ovirt-engine",
		})
	}
	return steps
}

// asDiskLocked достаёт ошибку блокировки диска, если она есть.
func asDiskLocked(err error) (*diskLockedError, bool) {
	var lockErr *diskLockedError
	return lockErr, errors.As(err, &lockErr)
}

// setManualSteps сохраняет команды для оператора в карточке запуска.
func (e *Engine) setManualSteps(run *model.BackupRun, steps []model.ManualStep) {
	if run == nil {
		return
	}
	e.manualMu.Lock()
	run.ManualSteps = steps
	e.manualMu.Unlock()
}

// diskLockReport переводит состояние диска в строку отчёта об остатках.
func diskLockReport(d ovirt.Disk, st diskLockState) DiskLockReport {
	r := DiskLockReport{DiskID: d.ID, Alias: d.AliasOrName(), Status: st.DiskStatus,
		LockedVolumes: st.LockedVolumes, BusySnapshots: st.BusySnapshots, Summary: st.String()}
	for _, t := range st.Transfers {
		r.Transfers = append(r.Transfers, fmt.Sprintf("%s (фаза %s)", t.ID, t.Phase))
	}
	return r
}

// stuckLockNotice объясняет тома, застрявшие в статусе locked без активной
// передачи и без операции со снапшотом, и возвращает их диски.
func stuckLockNotice(disks []DiskLockReport) (string, []string) {
	var parts, ids []string
	for _, d := range disks {
		if len(d.LockedVolumes) == 0 || len(d.Transfers) > 0 || len(d.BusySnapshots) > 0 {
			continue
		}
		ids = append(ids, d.DiskID)
		parts = append(parts, fmt.Sprintf("диск %s: том(а) %s", d.Alias, strings.Join(d.LockedVolumes, ", ")))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "В базе движка остались заблокированными тома без активной передачи и без операции со снапшотом — " +
		strings.Join(parts, "; ") + ". Так oVirt 4.3 оставляет том после отменённой передачи. Пока блокировка " +
		"не снята, движок не удалит снапшоты с этим томом и не откроет его передачу (HTTP 409 «disks are locked»). " +
		"Через API её не снять: администратор oVirt снимает её на хосте движка командами из блока «Что сделать " +
		"вручную», после чего повторите уборку.", ids
}

// stuckLockSteps — команды администратору oVirt для застрявшей блокировки томов.
func stuckLockSteps(srv *model.Server, diskIDs []string) []model.ManualStep {
	const unlock = "/usr/share/ovirt-engine/setup/dbutils/unlock_entity.sh"
	return []model.ManualStep{
		{
			Title:   "Администратору oVirt: посмотреть заблокированные диски в базе движка",
			Where:   "хост движка, root",
			Detail:  "Только просмотр, ничего не меняет. Диск из отчёта должен быть в списке.",
			Command: unlock + " -t disk -q",
		},
		{
			Title: "Администратору oVirt: снять застрявшую блокировку томов диска",
			Where: "хост движка, root",
			Risky: true,
			Detail: "Снимает статус locked со всех томов диска в базе движка. Выполнять, только если отчёт службы " +
				"показывает: незавершённых передач диска нет, снапшоты не в операции. Снятие блокировки с диска, " +
				"с которым идёт операция, может повредить образ.",
			Command: unlock + " -t disk " + strings.Join(quoteAll(diskIDs), " "),
		},
	}
}
