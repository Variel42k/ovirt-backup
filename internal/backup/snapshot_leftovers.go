package backup

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Уборка брошенных временных снапшотов.
//
// Бэкап через снапшот создаёт снапшот «jhvirt backup <id запуска>», читает из
// него диски и удаляет его. Если служба упала или удаление не прошло, снапшот
// остаётся: цепочка томов растёт, диск замедляется, а оценки объёма
// занижаются, потому что движок считает занятым только верхний том.
//
// Удаление снапшота у работающей ВМ — это слияние слоёв, и оно может идти
// долго. Поэтому уборка идёт в фоне после бэкапа и строго по одному снапшоту,
// а следующий бэкап сначала дожидается конца слияния: пока оно идёт, диски
// заблокированы, и движок ответил бы 409.

// unknownSnapshotAge — сколько должен пролежать снапшот с нашей меткой, чей
// запуск в базе не найден, прежде чем уборка его тронет. Такой снапшот мог
// только что создать запуск, ещё не записанный в базу, или другая установка
// службы, которая копирует ту же ВМ.
const unknownSnapshotAge = 6 * time.Hour

// snapshotWaitLimit — сколько бэкап ждёт конца операции со снапшотом.
const snapshotWaitLimit = time.Hour

// snapshotPollInterval — как часто проверяется состояние снапшотов; тесты
// укорачивают его.
var snapshotPollInterval = 10 * time.Second

// snapshotVerdict — что уборке делать со снапшотом.
type snapshotVerdict int

const (
	// snapshotNotOurs — снапшот не службы или свежий без записи в базе: не трогать.
	snapshotNotOurs snapshotVerdict = iota
	// snapshotRemove — брошенный снапшот службы: удалить.
	snapshotRemove
	// snapshotBusy — свой, но с ним идёт операция: вернуться к нему позже.
	snapshotBusy
	// snapshotLive — свой, запуск ещё идёт: это не остаток.
	snapshotLive
)

// leftoverSnapshot решает, брошенный ли это снапшот службы. why объясняет
// решение для хронологии и журнала.
func leftoverSnapshot(s ovirt.Snapshot, runs []*model.BackupRun, now time.Time) (owner string, verdict snapshotVerdict, why string) {
	if s.SnapshotType != "" && s.SnapshotType != "regular" {
		return "", snapshotNotOurs, "не обычный снапшот"
	}
	busy := s.SnapshotStatus != "" && s.SnapshotStatus != "ok"
	busyWhy := fmt.Sprintf("снапшот в состоянии %s — с ним идёт операция", s.SnapshotStatus)
	active := func(r *model.BackupRun) bool {
		return r.Status == model.RunPending || r.Status == model.RunRunning
	}
	legacyQcowLeaf := func(r *model.BackupRun) bool {
		if r.LegacyIncrementalMode != model.LegacyIncrementalQcow2 || r.Deleted ||
			(r.Status != model.RunSucceeded && r.Status != model.RunPartial) {
			return false
		}
		for _, child := range runs {
			diskPoint := child.Type == model.BackupFull || child.Type == model.BackupIncremental ||
				child.Type == model.BackupDifferential || child.Type == model.BackupSnapshot
			// A successful newer point for the same repository supersedes this
			// engine-side base even when it starts a fresh chain or switches to
			// block comparison. Matching the target is important: two repositories
			// may intentionally maintain independent chains for the same VM.
			newerSameTarget := child.StorageTargetID == r.StorageTargetID && child.CreatedAt.After(r.CreatedAt)
			if diskPoint && !child.Deleted && (child.Status == model.RunSucceeded || child.Status == model.RunPartial) &&
				(child.ParentRunID == r.ID || newerSameTarget) {
				return false
			}
		}
		return true
	}
	// Снапшот записан за запуском — самый надёжный признак: так служба узнаёт
	// и снапшот, который движок создал под бэкап со своим описанием.
	for _, r := range runs {
		if r.SnapshotID == "" || r.SnapshotID != s.ID {
			continue
		}
		switch {
		case active(r):
			return r.ID, snapshotLive, "запуск ещё идёт"
		case legacyQcowLeaf(r):
			return r.ID, snapshotLive, "опорный snapshot совместимой QCOW2-цепочки"
		case busy:
			return r.ID, snapshotBusy, busyWhy
		}
		return r.ID, snapshotRemove, "снапшот записан за завершённым запуском"
	}
	if !strings.HasPrefix(s.Description, ovirt.SnapshotMarkerPrefix) {
		return "", snapshotNotOurs, "снапшот не службы"
	}
	owner = strings.TrimSpace(strings.TrimPrefix(s.Description, ovirt.SnapshotMarkerPrefix))
	if owner == "" || strings.ContainsAny(owner, " 	") {
		return "", snapshotNotOurs, "описание похоже на метку службы, но не содержит запуска"
	}
	for _, r := range runs {
		if r.ID != owner {
			continue
		}
		switch {
		case active(r):
			return r.ID, snapshotLive, "запуск ещё идёт"
		case legacyQcowLeaf(r):
			return r.ID, snapshotLive, "опорный snapshot совместимой QCOW2-цепочки"
		case busy:
			return r.ID, snapshotBusy, busyWhy
		}
		return r.ID, snapshotRemove, "запуск завершён"
	}
	created := s.Date.Time()
	if created.IsZero() || now.Sub(created) < unknownSnapshotAge {
		return owner, snapshotNotOurs, "запуска нет в базе, а снапшот слишком свежий — его мог создать идущий запуск"
	}
	if busy {
		return owner, snapshotBusy, busyWhy
	}
	return owner, snapshotRemove, "запуска нет в базе, снапшот старше " + unknownSnapshotAge.String()
}

// transferRefsForSnapshot returns every identifier with which oVirt may link
// an ImageTransfer to a VM snapshot. In 4.3 transfer.snapshot.id is usually
// the image_id of a disk volume, not the UUID returned by /vms/.../snapshots.
// Comparing only the latter used to hide our abandoned transfer from cleanup,
// leaving the disk locked indefinitely.
func transferRefsForSnapshot(ctx context.Context, client *ovirt.Client, vmID string, s ovirt.Snapshot) map[string]struct{} {
	refs := map[string]struct{}{s.ID: {}}
	disks, err := client.ListSnapshotDisks(ctx, vmID, s.ID)
	if err != nil {
		return refs
	}
	for _, d := range disks {
		if d.ImageID != "" {
			refs[d.ImageID] = struct{}{}
		}
	}
	return refs
}

func transferMatchesRefs(t ovirt.ImageTransfer, refs map[string]struct{}) bool {
	_, bySnapshot := refs[t.Snapshot.ID]
	_, byImage := refs[t.Image.ID]
	return bySnapshot || byImage
}

// releaseLegacyTransferLeftovers cancels imageio tickets left by completed
// snapshot runs before a new snapshot is created. This is especially
// important on oVirt 4.3: it does not support timeout_policy=cancel, and a
// paused or half-open ticket can keep the disk locked after the service was
// restarted. Only tickets tied to a snapshot that is provably ours are
// touched; unknown transfers are left for the administrator.
func (e *Engine) releaseLegacyTransferLeftovers(ctx context.Context, client *ovirt.Client,
	srv *model.Server, vm *model.VM, run *model.BackupRun, diskIDs []string) error {

	transfers, err := client.ListImageTransfers(ctx)
	if err != nil {
		return fmt.Errorf("проверка старых передач образов: %w", err)
	}
	// Обычный случай — на движке нет ни одной незавершённой передачи:
	// остальные запросы не нужны.
	if !anyPendingTransfer(transfers) {
		return nil
	}
	snaps, err := client.ListSnapshots(ctx, vm.ID)
	if err != nil {
		return fmt.Errorf("проверка старых снапшотов перед бэкапом: %w", err)
	}
	runs, err := e.store.ListBackupRuns(ctx, store.RunFilter{
		ServerID: srv.ID, VMID: vm.ID, IncludeDeleted: true, Limit: 500,
	})
	if err != nil {
		return fmt.Errorf("история запусков для проверки старых передач: %w", err)
	}

	ownedRefs := map[string]struct{}{}
	for _, s := range snaps {
		owner, verdict, _ := leftoverSnapshot(s, runs, time.Now())
		if owner == "" || owner == run.ID || (verdict != snapshotRemove && verdict != snapshotBusy) {
			continue
		}
		for ref := range transferRefsForSnapshot(ctx, client, vm.ID, s) {
			ownedRefs[ref] = struct{}{}
		}
	}
	owners, err := e.store.TransferOwners(ctx, srv.ID, vm.ID)
	if err != nil {
		e.log.Warn().Err(err).Str("vm", vm.Name).Msg("записи о передачах прошлых запусков недоступны")
	}
	abandoned := abandonedTransfers(transfers, owners, runs, ownedRefs, run.ID)

	var failed []ovirt.ImageTransfer
	errs := e.cancelTransfersAndWait(ctx, client, abandoned)
	for _, t := range abandoned {
		if errs[t.ID] != nil {
			failed = append(failed, t)
			continue
		}
		e.event(ctx, run, model.RunEventLeftoverClosed, 0,
			fmt.Sprintf("отменена передача образа %s (фаза %s), оставленная предыдущим бэкапом: она держала диск", t.ID, t.Phase))
	}
	if len(failed) > 0 {
		run.ManualSteps = EngineUnlockSteps(srv, vm.ID, nil, failed, diskIDs)
		ids := make([]string, 0, len(failed))
		for _, t := range failed {
			ids = append(ids, t.ID)
		}
		return fmt.Errorf("диск удерживают передачи предыдущего snapshot-бэкапа %s; автоматическая отмена не завершилась, "+
			"снапшот не создан, команды сохранены в карточке запуска", strings.Join(ids, ", "))
	}

	// Передача, которую служба не опознала как свою, держит блокировку диска
	// в памяти движка: в интерфейсе диск выглядит свободным, а снапшот,
	// созданный сейчас, нельзя будет ни прочитать, ни удалить (HTTP 409).
	// Останавливаемся до снапшота и называем передачу.
	if len(abandoned) > 0 {
		if transfers, err = client.ListImageTransfers(ctx); err != nil {
			return fmt.Errorf("проверка передач образов после отмены: %w", err)
		}
	}
	vmRefs := map[string]struct{}{}
	for _, s := range snaps {
		for ref := range transferRefsForSnapshot(ctx, client, vm.ID, s) {
			vmRefs[ref] = struct{}{}
		}
	}
	blocking := transfersOnDisks(transfers, diskIDs, vmRefs)
	if len(blocking) == 0 {
		return nil
	}
	run.ManualSteps = EngineUnlockSteps(srv, vm.ID, nil, blocking, diskIDs)
	parts := make([]string, 0, len(blocking))
	for _, t := range blocking {
		parts = append(parts, fmt.Sprintf("%s (фаза %s)", t.ID, t.Phase))
	}
	return fmt.Errorf("диск ВМ держит незавершённая передача образа %s, которую служба не открывала или не может "+
		"опознать как свою: движок держит блокировку диска, пока передача не закрыта, хотя в интерфейсе диск "+
		"выглядит свободным. Снапшот не создан. Если передача брошена (например, её открыла служба до "+
		"обновления), отмените её командой из карточки запуска и повторите бэкап", strings.Join(parts, ", "))
}

// anyPendingTransfer — есть ли на движке незавершённые передачи.
func anyPendingTransfer(transfers []ovirt.ImageTransfer) bool {
	for _, t := range transfers {
		if !t.Terminal() {
			return true
		}
	}
	return false
}

// transfersOnDisks — незавершённые передачи дисков ВМ: по диску или по тому
// (image_id) любого её снапшота — движок 4.3 заполняет не все ссылки.
func transfersOnDisks(transfers []ovirt.ImageTransfer, diskIDs []string, refs map[string]struct{}) []ovirt.ImageTransfer {
	var out []ovirt.ImageTransfer
	for _, t := range transfers {
		if t.Terminal() {
			continue
		}
		if slices.Contains(diskIDs, t.Disk.ID) || transferMatchesRefs(t, refs) {
			out = append(out, t)
		}
	}
	return out
}

// waitSnapshotOperations ждёт, пока на ВМ не останется снапшотов в операции
// (создание, удаление со слиянием). Без этого бэкап сразу после уборки
// получил бы 409: пока слияние идёт, диски заблокированы.
func (e *Engine) waitSnapshotOperations(ctx context.Context, client *ovirt.Client, vm *model.VM, run *model.BackupRun) {
	started := time.Now()
	waited := false
	for {
		snaps, err := client.ListSnapshots(ctx, vm.ID)
		if err != nil {
			e.log.Warn().Err(err).Str("vm", vm.Name).Msg("не удалось проверить снапшоты перед бэкапом")
			return
		}
		var busy []string
		for _, s := range snaps {
			if s.SnapshotStatus == "locked" {
				busy = append(busy, fmt.Sprintf("%s (%s)", s.ID, s.Description))
			}
		}
		if len(busy) == 0 {
			if waited {
				e.event(ctx, run, model.RunEventSnapshotWait, time.Since(started), "операция со снапшотом завершилась")
			}
			return
		}
		if !waited {
			waited = true
			e.log.Info().Str("vm", vm.Name).Strs("снапшоты", busy).
				Msg("на ВМ идёт операция со снапшотом — бэкап ждёт её завершения")
		}
		if time.Since(started) > snapshotWaitLimit {
			e.event(ctx, run, model.RunEventSnapshotWait, time.Since(started),
				"операция со снапшотом не закончилась за "+snapshotWaitLimit.String()+
					": "+strings.Join(busy, ", ")+"; бэкап пробует начать всё равно")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(snapshotPollInterval):
		}
	}
}

// startSnapshotSweep запускает однократную уборку брошенных снапшотов ВМ в
// фоне после запуска. Одна уборка на ВМ за раз: второй вызов просто уходит.
// Повторять её служба не пытается: всё, что осталось, администратор убирает
// кнопкой «Убрать остатки службы» (см. CleanupLeftovers).
func (e *Engine) startSnapshotSweep(client *ovirt.Client, srv *model.Server, vm *model.VM, runID string) {
	key := srv.ID + "/" + vm.ID
	if _, busy := e.sweeping.LoadOrStore(key, struct{}{}); busy {
		return
	}
	go func() {
		defer e.sweeping.Delete(key)
		// Слияние большого снапшота на загруженном хранилище идёт часами.
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		e.sweepLeftoverSnapshots(ctx, client, srv, vm, runID)
	}()
}

// sweepLeftoverSnapshots удаляет брошенные снапшоты службы по одному и
// возвращает, сколько своих снапшотов осталось: не удалось удалить или с ними
// ещё идёт операция. Отметки пишутся в хронологию запуска runID, после
// которого уборка началась; если снапшот удалить не удалось, туда же
// попадают команды для администратора.
func (e *Engine) sweepLeftoverSnapshots(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, runID string) int {
	return e.sweepSnapshots(ctx, client, srv, vm, runID, sweepOptions{attachSteps: true})
}

// sweepOptions — чем уборка перед бэкапом отличается от фоновой.
type sweepOptions struct {
	// currentRun — запуск, который сам вызвал уборку перед своим снапшотом:
	// он не считается «идущим бэкапом ВМ», из-за которого уборку откладывают.
	currentRun string
	// attachSteps — дописывать команды к завершённому запуску runID. Перед
	// бэкапом запуск ещё идёт и сохранит свои команды сам.
	attachSteps bool
}

func (e *Engine) sweepSnapshots(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, runID string, opts sweepOptions) int {

	snaps, err := client.ListSnapshots(ctx, vm.ID)
	if err != nil {
		e.log.Warn().Err(err).Str("vm", vm.Name).Msg("уборка снапшотов: не удалось получить список")
		return 1
	}
	for _, s := range snaps {
		// ВМ в предпросмотре снапшота: оператор разбирается с ней руками,
		// любое удаление сейчас может помешать ему.
		if s.SnapshotStatus == "in_preview" {
			e.log.Info().Str("vm", vm.Name).Msg("уборка снапшотов пропущена: ВМ в режиме предпросмотра снапшота")
			return 0
		}
	}
	runs, err := e.store.ListBackupRuns(ctx, store.RunFilter{ServerID: srv.ID, VMID: vm.ID, IncludeDeleted: true, Limit: 500})
	if err != nil {
		e.log.Warn().Err(err).Msg("уборка снапшотов: история запусков недоступна")
		return 1
	}
	run := &model.BackupRun{ID: runID}
	var transfers []ovirt.ImageTransfer
	var owners map[string]string
	transfersLoaded := false

	pending := 0
	for i, s := range snaps {
		owner, verdict, why := leftoverSnapshot(s, runs, time.Now())
		if verdict != snapshotRemove {
			if verdict == snapshotBusy {
				// Свой, но пока занят операцией: фоновая уборка вернётся к нему.
				pending++
			}
			if owner != "" {
				e.log.Debug().Str("snapshot", s.ID).Str("причина", why).Msg("снапшот службы оставлен")
			}
			continue
		}
		// Между удалениями мог начаться бэкап этой ВМ: удаление упёрлось бы в
		// него. Уборку тогда продолжит следующий запуск.
		if active, err := e.store.ListBackupRuns(ctx, store.RunFilter{ServerID: srv.ID, VMID: vm.ID,
			Statuses: []model.RunStatus{model.RunPending, model.RunRunning}, Limit: 2}); err != nil ||
			otherActiveRun(active, opts.currentRun) {
			e.log.Info().Str("vm", vm.Name).Msg("уборка снапшотов отложена: идёт бэкап ВМ")
			return 0
		}
		// Зависшая передача образа держит снапшот: движок не удалит его, пока
		// она не закончена. Передача брошенного запуска никому не нужна.
		if !transfersLoaded {
			transfers, _ = client.ListImageTransfers(ctx)
			owners, _ = e.store.TransferOwners(ctx, srv.ID, vm.ID)
			transfersLoaded = true
		}
		// Передачи своих завершившихся запусков (по записи в хронологии) и
		// передачи томов этого снапшота: пока они открыты, движок держит диск.
		refs := transferRefsForSnapshot(ctx, client, vm.ID, s)
		abandoned := abandonedTransfers(transfers, owners, runs, refs, runID)
		_ = e.cancelTransfersAndWait(ctx, client, abandoned)
		if len(abandoned) > 0 {
			transfers, _ = client.ListImageTransfers(ctx)
		}
		// Том снапшота застрял в locked в базе движка: DELETE получил бы 409,
		// пока администратор oVirt не снимет блокировку. Не ждём и не
		// останавливаемся — остальные снапшоты удаляются.
		if stuck := stuckSnapshotVolumes(ctx, client, vm.ID, s, transfers); len(stuck) > 0 {
			pending++
			e.log.Warn().Str("vm", vm.Name).Str("snapshot", s.ID).Strs("тома", stuck).
				Msg("снапшот пропущен: его том заблокирован в базе движка без активной передачи")
			if _, reported := e.snapshotFailures.LoadOrStore("stuck:"+s.ID, struct{}{}); !reported {
				e.event(ctx, run, model.RunEventSnapshotRemoveFailed, 0, fmt.Sprintf(
					"снапшот %s (%s) не удалить: том(а) %s заблокирован(ы) в базе движка без активной передачи — "+
						"снять блокировку может только администратор oVirt на хосте движка", s.ID, s.Description,
					strings.Join(stuck, ", ")))
				if opts.attachSteps {
					e.attachSteps(ctx, runID, stuckLockSteps(srv, snapshotDiskIDs(ctx, client, vm.ID, s)))
				}
			}
			continue
		}
		started := time.Now()
		err := client.DeleteSnapshotWhenReady(ctx, vm.ID, s.ID, 10*time.Minute)
		if err == nil || ovirt.IsNotFound(err) {
			err = client.WaitSnapshotGone(ctx, vm.ID, s.ID, 5*time.Hour)
			// Время уборки перед бэкапом вышло, а движок ещё сливает слои:
			// удаление идёт, бэкап дождётся его. Это не неудача.
			if err != nil && ctx.Err() != nil {
				e.log.Info().Str("vm", vm.Name).Str("snapshot", s.ID).
					Msg("удаление снапшота запущено, движок ещё сливает слои — продолжится без уборки")
				return pending + len(snaps) - i
			}
		}
		if err != nil {
			e.log.Error().Err(err).Str("vm", vm.Name).Str("snapshot", s.ID).
				Msg("не удалось удалить брошенный снапшот — команды для администратора в карточке запуска")
			// Фоновая уборка повторяется каждые несколько минут: отметка и
			// команды — только при первой неудаче, иначе хронология утонет в
			// одинаковых строках.
			if _, reported := e.snapshotFailures.LoadOrStore(s.ID, struct{}{}); !reported {
				e.event(ctx, run, model.RunEventSnapshotRemoveFailed, time.Since(started),
					fmt.Sprintf("снапшот %s (%s): %v", s.ID, s.Description, err))
				if opts.attachSteps {
					e.attachSteps(ctx, runID, SnapshotCleanupSteps(srv, vm.ID, s))
				}
			}
			// Движок не справился с одним — остальные подождут следующего
			// раза, чтобы не громоздить операции на проблемное хранилище.
			return pending + len(snaps) - i
		}
		e.event(ctx, run, model.RunEventSnapshotRemoved, time.Since(started),
			fmt.Sprintf("снапшот %s оставлен запуском %s (%s); слияние данных завершено", s.ID, owner, why))
		e.log.Info().Str("vm", vm.Name).Str("snapshot", s.ID).Str("запуск", owner).
			Msg("удалён брошенный снапшот")
	}
	return pending
}

// attachSteps дописывает команды к уже завершённому запуску. Запуск читается
// из базы заново: объект, который вернул Execute, принадлежит вызывающему.
func (e *Engine) attachSteps(ctx context.Context, runID string, steps []model.ManualStep) {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	run, err := e.store.GetBackupRun(saveCtx, runID)
	if err != nil {
		e.log.Warn().Err(err).Str("run", runID).Msg("не удалось дописать команды к запуску")
		return
	}
	run.ManualSteps = steps
	if err := e.store.UpdateBackupRun(saveCtx, run); err != nil {
		e.log.Warn().Err(err).Str("run", runID).Msg("не удалось дописать команды к запуску")
	}
}

// SnapshotCleanupSteps — готовые команды, чтобы удалить брошенный снапшот
// вручную. Всё подставлено; пароль curl спросит сам.
func SnapshotCleanupSteps(srv *model.Server, vmID string, s ovirt.Snapshot) []model.ManualStep {
	cmd := newEngineCommands(srv)
	steps := append([]model.ManualStep(nil), cmd.steps...)
	snapshots := cmd.url("/vms/" + vmID + "/snapshots")
	check := cmd.curl + " " + snapshots +
		` | grep -oE '"(id|description|snapshot_status|snapshot_type)" *: *"[^"]*"'`
	steps = append(steps,
		model.ManualStep{
			Title: "Посмотреть снапшоты ВМ",
			Where: anywhere,
			Detail: fmt.Sprintf("Снапшот %s — «%s». Удалять можно, когда его snapshot_status — ok: "+
				"locked значит, что операция с ним ещё идёт.", s.ID, s.Description),
			Command: check,
		},
		model.ManualStep{
			Title: "Удалить брошенный снапшот",
			Where: anywhere,
			Detail: "Перед удалением убедитесь, что бэкап этой ВМ сейчас не идёт. Удаление у работающей ВМ " +
				"сливает слои и нагружает хранилище; то же можно сделать в интерфейсе oVirt: ВМ → Snapshots → Delete.",
			Command: cmd.delete + " " + cmd.url("/vms/"+vmID+"/snapshots/"+s.ID),
		},
		model.ManualStep{
			Title:   "Проверить, что снапшот исчез",
			Where:   anywhere,
			Detail:  "Слияние данных может идти от минут до часов. Снапшот пропадёт из списка, когда оно закончится.",
			Command: check,
		},
	)
	return steps
}
