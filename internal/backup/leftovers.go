package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Остатки бэкапов на движке по кнопке.
//
// Незакрытый бэкап, зависшая передача образа и брошенный снапшот держат диски
// ВМ, и следующий бэкап получает 409. Убирать их служба умеет, но делает это
// по команде администратора: он видит, что найдено, чьё это и что будет
// сделано, и сам выбирает момент — например, когда хост вернулся после аварии.
//
// Трогается только своё. Бэкап другой системы копирования, снапшот
// администратора, остатки идущего запуска остаются как есть; для них в отчёте
// готовые команды. Блокировки в базе движка служба не снимает никогда.

// LeftoverKind — вид остатка.
type LeftoverKind string

const (
	LeftoverBackup   LeftoverKind = "engine_backup"
	LeftoverTransfer LeftoverKind = "image_transfer"
	LeftoverSnapshot LeftoverKind = "snapshot"
)

// Leftover — один найденный остаток.
type Leftover struct {
	Kind  LeftoverKind `json:"kind"`
	ID    string       `json:"id"`
	Title string       `json:"title"`
	// State — состояние на движке: фаза бэкапа или передачи, статус снапшота.
	State string `json:"state"`
	// Owner — запуск службы, которому принадлежит остаток; пусто — не службы.
	Owner string `json:"owner,omitempty"`
	Ours  bool   `json:"ours"`
	// Removable — служба уберёт это по кнопке сейчас; Reason объясняет решение.
	Removable bool   `json:"removable"`
	Reason    string `json:"reason"`
	Created   string `json:"created,omitempty"`
}

// LeftoverReport — что найдено на движке по ВМ.
type LeftoverReport struct {
	ServerID string     `json:"server_id"`
	VMID     string     `json:"vm_id"`
	VMName   string     `json:"vm_name"`
	Items    []Leftover `json:"items"`
	// Removable — сколько остатков служба уберёт по кнопке.
	Removable int `json:"removable"`
	// Blocked — почему кнопка сейчас ничего не сделает (идёт бэкап ВМ).
	Blocked string `json:"blocked,omitempty"`
	// StuckLock — том диска застрял в статусе locked в базе движка без
	// активной операции: удаление снапшотов не пройдёт, пока администратор
	// oVirt не снимет блокировку на хосте движка.
	StuckLock string `json:"stuck_lock,omitempty"`
	// ManualSteps — команды для того, что служба не трогает.
	ManualSteps []model.ManualStep `json:"manual_steps,omitempty"`
	// Disks — что движок показывает о занятости дисков ВМ. Только для движков
	// без Backup API: там диск бывает занят без видимой причины.
	Disks []DiskLockReport `json:"disks,omitempty"`
	// EngineEvents — последние события движка по ВМ (журнал движка через API).
	EngineEvents []string  `json:"engine_events,omitempty"`
	CheckedAt    time.Time `json:"checked_at"`
}

// DiskLockReport — что движок показывает о занятости диска.
type DiskLockReport struct {
	DiskID        string   `json:"disk_id"`
	Alias         string   `json:"alias"`
	Status        string   `json:"status,omitempty"`
	Transfers     []string `json:"transfers,omitempty"`
	LockedVolumes []string `json:"locked_volumes,omitempty"`
	BusySnapshots []string `json:"busy_snapshots,omitempty"`
	// Summary — одной строкой для интерфейса.
	Summary string `json:"summary"`
}

// CleanupAction — что служба сделала по кнопке.
type CleanupAction struct {
	Kind   LeftoverKind `json:"kind"`
	ID     string       `json:"id"`
	OK     bool         `json:"ok"`
	Detail string       `json:"detail"`
}

// CleanupResult — итог уборки и свежий отчёт после неё.
type CleanupResult struct {
	Actions []CleanupAction `json:"actions"`
	After   *LeftoverReport `json:"after"`
}

// ErrLeftoversUnsupported — у платформы нет Backup API oVirt: остатков такого
// рода не бывает.
var ErrLeftoversUnsupported = errors.New("остатки бэкапов на движке бывают только у oVirt и его производных")

// leftoverScan — собранное состояние ВМ на движке.
type leftoverScan struct {
	srv       *model.Server
	vm        *model.VM
	client    *ovirt.Client
	runs      []*model.BackupRun
	backups   []ovirt.Backup // открытые
	transfers []ovirt.ImageTransfer
	snaps     []ovirt.Snapshot
	// snapshotByTransferRef maps both a VM snapshot UUID and its disk-volume
	// image_id values to that VM snapshot. oVirt 4.3 ImageTransfer uses the
	// latter in transfer.snapshot.id.
	snapshotByTransferRef map[string]string
	// transferOwners — передачи, открытые запусками службы по этой ВМ
	// (запись в хронологии): идентификатор передачи → запуск.
	transferOwners map[string]string
	diskIDs        []string
	busyVM         bool
	// diskLocks и events — только для движков без Backup API.
	diskLocks []DiskLockReport
	events    []string
}

func (e *Engine) scanLeftovers(ctx context.Context, serverID, vmID string) (*leftoverScan, error) {
	srv, err := e.store.GetServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if !srv.Kind.UsesOVirtAPI() {
		return nil, ErrLeftoversUnsupported
	}
	vm, err := e.store.GetVM(ctx, serverID, vmID)
	if err != nil {
		return nil, err
	}
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return nil, err
	}
	sc := &leftoverScan{srv: srv, vm: vm, client: client}

	if sc.runs, err = e.store.ListBackupRuns(ctx, store.RunFilter{ServerID: srv.ID, VMID: vm.ID,
		IncludeDeleted: true, Limit: 500}); err != nil {
		return nil, err
	}
	for _, r := range sc.runs {
		if r.Status == model.RunPending || r.Status == model.RunRunning {
			sc.busyVM = true
		}
	}
	backups, err := client.ListBackups(ctx, vm.ID)
	if err != nil {
		return nil, fmt.Errorf("бэкапы ВМ на движке: %w", err)
	}
	for _, b := range backups {
		if b.Open() {
			sc.backups = append(sc.backups, b)
		}
	}
	if sc.snaps, err = client.ListSnapshots(ctx, vm.ID); err != nil {
		return nil, fmt.Errorf("снапшоты ВМ: %w", err)
	}
	if sc.transfers, err = client.ListImageTransfers(ctx); err != nil {
		return nil, fmt.Errorf("передачи образов: %w", err)
	}
	if sc.transferOwners, err = e.store.TransferOwners(ctx, srv.ID, vm.ID); err != nil {
		e.log.Warn().Err(err).Str("vm", vm.Name).Msg("записи о передачах запусков недоступны")
	}
	sc.snapshotByTransferRef = map[string]string{}
	ownedSnapshotIDs := map[string]bool{}
	for _, b := range sc.backups {
		if owner, _ := ownerOf(b, sc.runs); owner != "" && b.Snapshot.ID != "" {
			ownedSnapshotIDs[b.Snapshot.ID] = true
		}
	}
	for _, s := range sc.snaps {
		owner, _, _ := leftoverSnapshot(s, sc.runs, time.Now())
		if owner == "" && !ownedSnapshotIDs[s.ID] {
			continue
		}
		for ref := range transferRefsForSnapshot(ctx, client, vm.ID, s) {
			sc.snapshotByTransferRef[ref] = s.ID
		}
	}
	if disks, err := client.ListVMDisks(ctx, vm.ID); err == nil {
		for _, d := range disks {
			sc.diskIDs = append(sc.diskIDs, d.ID)
			if !srv.SupportsCBT {
				st := probeDiskLock(ctx, client, volumeDisk{vmID: vm.ID, vmName: vm.Name, diskID: d.ID, alias: d.AliasOrName()})
				sc.diskLocks = append(sc.diskLocks, diskLockReport(d, st))
			}
		}
	}
	if !srv.SupportsCBT {
		sc.events = recentEngineEvents(ctx, client, 10, vm.Name)
	}
	return sc, nil
}

// report превращает собранное состояние в отчёт для оператора.
func (e *Engine) report(sc *leftoverScan) *LeftoverReport {
	rep := &LeftoverReport{ServerID: sc.srv.ID, VMID: sc.vm.ID, VMName: sc.vm.Name, CheckedAt: time.Now().UTC(),
		Disks: sc.diskLocks, EngineEvents: sc.events}
	if sc.busyVM {
		rep.Blocked = "идёт бэкап этой ВМ — он сам убирает за собой; дождитесь его окончания"
	}
	var blocking []ovirt.Backup
	ownBackups := map[string]bool{}
	// Служебный снапшот движка под брошенный бэкап: за запуском он ещё не
	// записан, но бэкап на него ссылается.
	backupSnapshot := map[string]string{}

	for _, b := range sc.backups {
		owner, live := ownerOf(b, sc.runs)
		item := Leftover{Kind: LeftoverBackup, ID: b.ID, Title: "Бэкап на движке", State: b.Phase,
			Owner: owner, Ours: owner != ""}
		if t := b.CreationDate.Time(); !t.IsZero() {
			item.Created = t.UTC().Format(time.RFC3339)
		}
		switch {
		case live:
			item.Reason = "бэкап идущего запуска службы — не остаток"
		case owner == "":
			item.Reason = fmt.Sprintf("открыт не службой (описание %q) — возможно, другой системой копирования", b.Description)
			blocking = append(blocking, b)
		case b.Phase == "finalizing":
			item.Reason = "движок уже закрывает этот бэкап"
		default:
			item.Removable = !sc.busyVM
			item.Reason = "брошен запуском службы: будет закрыт, его передачи отменены"
			ownBackups[b.ID] = true
			if b.Snapshot.ID != "" {
				backupSnapshot[b.Snapshot.ID] = owner
			}
			if b.Host.ID != "" {
				item.Reason += "; закрытие выполняет хост " + b.Host.ID + " — если он недоступен, бэкап не закроется"
			}
		}
		rep.Items = append(rep.Items, item)
	}

	ownSnaps := map[string]bool{}
	for _, s := range sc.snaps {
		owner, verdict, why := leftoverSnapshot(s, sc.runs, time.Now())
		if owner == "" && backupSnapshot[s.ID] != "" {
			owner, verdict = backupSnapshot[s.ID], snapshotBusy
			why = "служебный снапшот брошенного бэкапа: будет удалён сразу после закрытия бэкапа — до этого движок его не отдаст"
		}
		if owner == "" {
			continue // снапшоты администратора и чужие в отчёт не попадают
		}
		ownSnaps[s.ID] = true
		item := Leftover{Kind: LeftoverSnapshot, ID: s.ID, Title: "Снапшот «" + s.Description + "»",
			State: s.SnapshotStatus, Owner: owner, Ours: true, Reason: why}
		if t := s.Date.Time(); !t.IsZero() {
			item.Created = t.UTC().Format(time.RFC3339)
		}
		if verdict == snapshotRemove {
			item.Removable = !sc.busyVM
			item.Reason = why + ": будет удалён; движок сольёт слои, это может занять время"
		}
		rep.Items = append(rep.Items, item)
	}

	for _, t := range sc.transfers {
		if t.Terminal() {
			continue
		}
		snapshotID := sc.snapshotByTransferRef[t.Snapshot.ID]
		if snapshotID == "" {
			snapshotID = sc.snapshotByTransferRef[t.Image.ID]
		}
		_, recorded := ownTransfer(t.ID, sc.transferOwners, sc.runs)
		ours := ownBackups[t.Backup.ID] || ownSnaps[snapshotID] || recorded
		related := ours || slices.Contains(sc.diskIDs, t.Disk.ID)
		if !related {
			continue
		}
		item := Leftover{Kind: LeftoverTransfer, ID: t.ID, Title: "Передача образа", State: t.Phase, Ours: ours}
		if ours {
			item.Removable = !sc.busyVM
			item.Reason = "передача брошенного бэкапа или снапшота службы: будет отменена"
			if recorded {
				item.Reason = "передачу открыл запуск службы, который уже завершился; она держит диск — будет отменена"
			}
		} else {
			item.Reason = "передача не связана с остатками службы — её мог открыть идущий бэкап или другая система"
		}
		rep.Items = append(rep.Items, item)
	}

	for _, item := range rep.Items {
		if item.Removable {
			rep.Removable++
		}
	}
	var stuckDisks []string
	if rep.StuckLock, stuckDisks = stuckLockNotice(sc.diskLocks); rep.StuckLock != "" {
		rep.ManualSteps = append(rep.ManualSteps, stuckLockSteps(sc.srv, stuckDisks)...)
	}
	if len(blocking) > 0 || (len(rep.Items) > rep.Removable && !sc.busyVM) {
		rep.ManualSteps = EngineUnlockSteps(sc.srv, sc.vm.ID, blocking,
			relatedTransfers(sc.transfers, blocking, sc.diskIDs), sc.diskIDs)
	}
	return rep
}

// snapshotNow — есть ли снапшот сейчас и в каком он состоянии. Ошибка
// запроса считается как «есть, состояние неизвестно».
func snapshotNow(ctx context.Context, client *ovirt.Client, vmID, snapshotID string) (gone bool, state string) {
	snap, err := client.GetSnapshot(ctx, vmID, snapshotID)
	if ovirt.IsNotFound(err) {
		return true, ""
	}
	if err != nil {
		return false, ""
	}
	return false, snap.SnapshotStatus
}

// InspectLeftovers показывает, что осталось на движке по ВМ, ничего не меняя.
func (e *Engine) InspectLeftovers(ctx context.Context, serverID, vmID string) (*LeftoverReport, error) {
	sc, err := e.scanLeftovers(ctx, serverID, vmID)
	if err != nil {
		return nil, err
	}
	return e.report(sc), nil
}

// CleanupLeftovers убирает остатки службы по ВМ: закрывает брошенные бэкапы
// движка, отменяет их передачи и запускает удаление брошенных снапшотов.
// Снапшоты удаляются только после того, как все свои бэкапы закрыты: пока
// бэкап открыт, движок снапшот не отдаст. Слияние слоёв при удалении снапшота
// идёт на движке — служба его не дожидается, итог видно по «Проверить
// остатки».
func (e *Engine) CleanupLeftovers(ctx context.Context, serverID, vmID string) (*CleanupResult, error) {
	sc, err := e.scanLeftovers(ctx, serverID, vmID)
	if err != nil {
		return nil, err
	}
	if sc.busyVM {
		return nil, fmt.Errorf("идёт бэкап ВМ %s — он сам убирает за собой; повторите после его окончания", sc.vm.Name)
	}
	res := &CleanupResult{}
	client, vm := sc.client, sc.vm

	backupsClosed := true
	for _, b := range sc.backups {
		owner, live := ownerOf(b, sc.runs)
		if owner == "" || live || b.Phase == "finalizing" {
			continue
		}
		e.rememberBackupSnapshot(ctx, owner, b)
		if err := e.closeLeftover(ctx, client, vm, b, sc.transfers); err != nil {
			backupsClosed = false
			detail := fmt.Sprintf("не закрыт: %v", err)
			if b.Host.ID != "" {
				detail += fmt.Sprintf(". Закрытие выполняет хост %s — проверьте, что он в состоянии Up", b.Host.ID)
			}
			res.Actions = append(res.Actions, CleanupAction{Kind: LeftoverBackup, ID: b.ID, Detail: detail})
			continue
		}
		res.Actions = append(res.Actions, CleanupAction{Kind: LeftoverBackup, ID: b.ID, OK: true,
			Detail: "бэкап закрыт, диски отпущены"})
	}

	if backupsClosed {
		// Список запусков перечитывается: закрытие могло записать за запуском
		// служебный снапшот бэкапа.
		runs, err := e.store.ListBackupRuns(ctx, store.RunFilter{ServerID: sc.srv.ID, VMID: vm.ID,
			IncludeDeleted: true, Limit: 500})
		if err == nil {
			sc.runs = runs
		}
		snaps, err := client.ListSnapshots(ctx, vm.ID)
		if err == nil {
			sc.snaps = snaps
		}
		// Сначала закрываются брошенные передачи: пока передача открыта,
		// движок держит блокировку диска в памяти — в интерфейсе диск
		// свободен, а удаление снапшота получает 409 «disks are locked».
		refs := map[string]struct{}{}
		for _, s := range sc.snaps {
			if _, verdict, _ := leftoverSnapshot(s, sc.runs, time.Now()); verdict == snapshotRemove {
				for ref := range transferRefsForSnapshot(ctx, client, vm.ID, s) {
					refs[ref] = struct{}{}
				}
			}
		}
		abandoned := abandonedTransfers(sc.transfers, sc.transferOwners, sc.runs, refs, "")
		transferErrs := e.cancelTransfersAndWait(ctx, client, abandoned)
		for _, t := range abandoned {
			action := CleanupAction{Kind: LeftoverTransfer, ID: t.ID, OK: true, Detail: "передача отменена, движок её закрыл"}
			if err := transferErrs[t.ID]; err != nil {
				action = CleanupAction{Kind: LeftoverTransfer, ID: t.ID, Detail: "передачу закрыть не удалось: " + err.Error()}
			}
			res.Actions = append(res.Actions, action)
		}
		for _, s := range sc.snaps {
			owner, verdict, _ := leftoverSnapshot(s, sc.runs, time.Now())
			if verdict != snapshotRemove {
				continue
			}
			if err := client.DeleteSnapshotWhenReady(ctx, vm.ID, s.ID, 10*time.Minute); err != nil && !ovirt.IsNotFound(err) {
				// HTTP 409 explicitly means that DELETE was rejected. A locked
				// snapshot in this case is the pre-existing operation, not proof
				// that our deletion started; report it honestly to the operator.
				if ovirt.IsConflict(err) {
					res.Actions = append(res.Actions, CleanupAction{Kind: LeftoverSnapshot, ID: s.ID,
						Detail: fmt.Sprintf("удаление не запущено: %v%s", err, e.cleanupLockHint(ctx, client, sc, refs))})
					continue
				}
				// Ответ мог потеряться, а движок — принять удаление (или его уже
				// начал повтор): судить по самому снапшоту, а не по ответу.
				if gone, state := snapshotNow(ctx, client, vm.ID, s.ID); !gone && state != "locked" {
					res.Actions = append(res.Actions, CleanupAction{Kind: LeftoverSnapshot, ID: s.ID,
						Detail: fmt.Sprintf("удаление не запущено: %v", err)})
					continue
				}
			}
			res.Actions = append(res.Actions, CleanupAction{Kind: LeftoverSnapshot, ID: s.ID, OK: true,
				Detail: fmt.Sprintf("удаление запущено (запуск %s); движок сливает слои — диски освободятся, когда снапшот исчезнет из списка", owner)})
		}
	} else {
		for _, s := range sc.snaps {
			if owner, _, _ := leftoverSnapshot(s, sc.runs, time.Now()); owner != "" {
				res.Actions = append(res.Actions, CleanupAction{Kind: LeftoverSnapshot, ID: s.ID,
					Detail: "не тронут: сначала должны закрыться бэкапы движка, иначе движок снапшот не отдаст"})
			}
		}
	}

	after, err := e.scanLeftovers(ctx, sc.srv.ID, vm.ID)
	if err == nil {
		res.After = e.report(after)
	}
	return res, nil
}

// lockHolderHint называет передачи, которые держат диск после уборки: их
// служба не опознала как свои. В 4.3 это блокировка в памяти движка — в
// интерфейсе диск выглядит свободным, и без подсказки 409 непонятен.
func lockHolderHint(ctx context.Context, client *ovirt.Client, diskIDs []string, refs map[string]struct{}) string {
	transfers, err := client.ListImageTransfers(ctx)
	if err != nil {
		return ""
	}
	holders := transfersOnDisks(transfers, diskIDs, refs)
	if len(holders) == 0 {
		return ""
	}
	parts := make([]string, 0, len(holders))
	for _, t := range holders {
		parts = append(parts, fmt.Sprintf("%s (фаза %s)", t.ID, t.Phase))
	}
	return ". Диск держит незавершённая передача образа " + strings.Join(parts, ", ") +
		": служба не открывала её или не может опознать как свою, поэтому не отменяет сама. " +
		"Если передача брошена, отмените её командой из отчёта об остатках и повторите уборку"
}

// cleanupLockHint объясняет 409 при удалении снапшота: передачи, которые
// держат диск, а на движке без Backup API — и тома, застрявшие в locked.
func (e *Engine) cleanupLockHint(ctx context.Context, client *ovirt.Client, sc *leftoverScan,
	refs map[string]struct{}) string {

	hint := lockHolderHint(ctx, client, sc.diskIDs, refs)
	if sc.srv.SupportsCBT {
		return hint
	}
	var fresh []DiskLockReport
	for _, d := range sc.diskLocks {
		st := probeDiskLock(ctx, client, volumeDisk{vmID: sc.vm.ID, vmName: sc.vm.Name, diskID: d.DiskID, alias: d.Alias})
		fresh = append(fresh, diskLockReport(ovirt.Disk{ID: d.DiskID, Alias: d.Alias}, st))
	}
	if notice, _ := stuckLockNotice(fresh); notice != "" {
		hint += ". " + notice
	}
	return hint
}
