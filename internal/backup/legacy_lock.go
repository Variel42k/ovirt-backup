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
	vmID, diskID, alias string
}

type volumeDiskKey struct{}

// withVolumeDisk помечает контекст диском, тома которого скачиваются.
func withVolumeDisk(ctx context.Context, vmID string, disk ovirt.Disk) context.Context {
	return context.WithValue(ctx, volumeDiskKey{}, volumeDisk{vmID: vmID, diskID: disk.ID, alias: disk.AliasOrName()})
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
	st.ProbeError = strings.Join(errs, "; ")
	return st
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
	return fmt.Sprintf("движок %s не отпускал диск %s для передачи следующего тома (HTTP 409 «disks are locked»). "+
		"Что показывал движок: %s. %s",
		humanDuration(e.waited), e.disk.alias, e.state, e.diagnosis())
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
			"движок 4.3 не снял блокировку после завершения передачи. Проверьте engine.log по диску и, если " +
			"операций с ним действительно нет, снимите блокировку командой unlock_entity.sh из блока «Что сделать вручную»"
	default:
		return "Движок не показывает причину ни в передачах, ни в статусах диска и снапшотов: блокировку держит " +
			"незавершённая внутренняя команда движка (после передачи предыдущего тома). Она видна только в " +
			"engine.log; снимается перезапуском службы ovirt-engine — ВМ при этом продолжают работать"
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
func legacyLockSteps(srv *model.Server, vmID string, lockErr *diskLockedError) []model.ManualStep {
	steps := EngineUnlockSteps(srv, vmID, nil, lockErr.state.Transfers, []string{lockErr.disk.diskID})
	steps = append(steps, model.ManualStep{
		Title: "Посмотреть в журнале движка, что делал с диском",
		Where: "хост движка",
		Detail: "Последние записи по диску: какая команда его заблокировала и чем она закончилась. " +
			"Ищите TransferDiskImageCommand, CreateSnapshot, RemoveSnapshot и строки с EngineLock.",
		Command: "sudo grep " + shellQuote(lockErr.disk.diskID) + " /var/log/ovirt-engine/engine.log | tail -60",
	})
	if !lockErr.state.visibleCause() {
		steps = append(steps, model.ManualStep{
			Title: "Если причины не видно: перезапустить службу движка",
			Where: "хост движка, root",
			Risky: true,
			Detail: "Блокировку держит незавершённая команда в памяти движка. Перезапуск ovirt-engine её снимает; " +
				"ВМ продолжают работать, но идущие через движок операции (миграции, снапшоты, передачи) прервутся, " +
				"а портал будет недоступен пару минут. Выполняйте, когда на движке нет других операций.",
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
