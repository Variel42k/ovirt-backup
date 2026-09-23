package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// ManualSteps — команды для того, что служба не трогает.
	ManualSteps []model.ManualStep `json:"manual_steps,omitempty"`
	CheckedAt   time.Time          `json:"checked_at"`
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
	diskIDs   []string
	busyVM    bool
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
	if disks, err := client.ListVMDisks(ctx, vm.ID); err == nil {
		for _, d := range disks {
			sc.diskIDs = append(sc.diskIDs, d.ID)
		}
	}
	return sc, nil
}

// report превращает собранное состояние в отчёт для оператора.
func (e *Engine) report(sc *leftoverScan) *LeftoverReport {
	rep := &LeftoverReport{ServerID: sc.srv.ID, VMID: sc.vm.ID, VMName: sc.vm.Name, CheckedAt: time.Now().UTC()}
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
		item := Leftover{Kind: LeftoverSnapshot, ID: s.ID, Title: "Снапшот «" + s.Description + "»",
			State: s.SnapshotStatus, Owner: owner, Ours: true, Reason: why}
		if t := s.Date.Time(); !t.IsZero() {
			item.Created = t.UTC().Format(time.RFC3339)
		}
		if verdict == snapshotRemove {
			item.Removable = !sc.busyVM
			item.Reason = why + ": будет удалён; движок сольёт слои, это может занять время"
			ownSnaps[s.ID] = true
		}
		rep.Items = append(rep.Items, item)
	}

	for _, t := range sc.transfers {
		if t.Terminal() {
			continue
		}
		ours := ownBackups[t.Backup.ID] || ownSnaps[t.Snapshot.ID]
		related := ours || slices.Contains(sc.diskIDs, t.Disk.ID)
		if !related {
			continue
		}
		item := Leftover{Kind: LeftoverTransfer, ID: t.ID, Title: "Передача образа", State: t.Phase, Ours: ours}
		if ours {
			item.Removable = !sc.busyVM
			item.Reason = "передача брошенного бэкапа или снапшота службы: будет отменена"
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
		for _, s := range sc.snaps {
			owner, verdict, _ := leftoverSnapshot(s, sc.runs, time.Now())
			if verdict != snapshotRemove {
				continue
			}
			for _, t := range sc.transfers {
				if t.Snapshot.ID == s.ID && !t.Terminal() {
					ok := client.CancelTransfer(ctx, t.ID) == nil
					res.Actions = append(res.Actions, CleanupAction{Kind: LeftoverTransfer, ID: t.ID, OK: ok,
						Detail: map[bool]string{true: "передача отменена", false: "передачу отменить не удалось"}[ok]})
				}
			}
			if err := client.DeleteSnapshot(ctx, vm.ID, s.ID); err != nil && !ovirt.IsNotFound(err) {
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
