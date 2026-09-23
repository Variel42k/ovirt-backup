package backup

import (
	"context"
	"fmt"
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
	// Снапшот записан за запуском — самый надёжный признак: так служба узнаёт
	// и снапшот, который движок создал под бэкап со своим описанием.
	for _, r := range runs {
		if r.SnapshotID == "" || r.SnapshotID != s.ID {
			continue
		}
		switch {
		case active(r):
			return r.ID, snapshotLive, "запуск ещё идёт"
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

// Фоновая уборка повторяется, пока не закроет всё своё: после аварии хоста
// движок не может ни закрыть бэкап, ни удалить снапшот, пока хост не
// вернётся, и ждать следующего запуска по расписанию незачем — диски ВМ всё
// это время заблокированы. Тесты укорачивают оба срока.
var (
	cleanupRetryInterval = 10 * time.Minute
	cleanupDeadline      = 6 * time.Hour
)

// startCleanup запускает фоновую уборку ВМ. Одна уборка на ВМ за раз: второй
// вызов просто уходит.
func (e *Engine) startCleanup(client *ovirt.Client, srv *model.Server, vm *model.VM, runID string) {
	key := srv.ID + "/" + vm.ID
	if _, busy := e.sweeping.LoadOrStore(key, struct{}{}); busy {
		return
	}
	go func() {
		defer e.sweeping.Delete(key)
		// Контекст не связан с запуском: он уже закончился. Служба может
		// перезапуститься посреди уборки — тогда её продолжит следующий бэкап.
		ctx, cancel := context.WithTimeout(context.Background(), cleanupDeadline+time.Hour)
		defer cancel()
		e.cleanupVM(ctx, client, srv, vm, runID)
	}()
}

// cleanupVM закрывает брошенные бэкапы движка и удаляет брошенные снапшоты
// ВМ, повторяя попытки, пока своего не останется или не выйдет срок. Если
// начался новый бэкап ВМ, уборка уступает ему: он убирает за собой сам.
func (e *Engine) cleanupVM(ctx context.Context, client *ovirt.Client, srv *model.Server, vm *model.VM, runID string) {
	deadline := time.Now().Add(cleanupDeadline)
	for {
		if active, err := e.store.ListBackupRuns(ctx, store.RunFilter{ServerID: srv.ID, VMID: vm.ID,
			Statuses: []model.RunStatus{model.RunPending, model.RunRunning}, Limit: 1}); err == nil && len(active) > 0 {
			return
		}
		pending := e.closeOwnBackups(ctx, client, srv, vm, runID)
		// Снапшот бэкапа удаляется только после закрытия самого бэкапа.
		if pending == 0 {
			pending += e.sweepLeftoverSnapshots(ctx, client, srv, vm, runID)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.log.Warn().Str("vm", vm.Name).Int("осталось", pending).
				Msg("фоновая уборка не закончила за отведённый срок — команды для администратора в карточке запуска")
			lookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			if steps := e.engineLockSteps(lookCtx, client, srv, vm.ID, nil); len(steps) > 0 {
				e.attachSteps(lookCtx, runID, steps)
			}
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cleanupRetryInterval):
		}
	}
}

// sweepLeftoverSnapshots удаляет брошенные снапшоты службы по одному и
// возвращает, сколько своих снапшотов осталось: не удалось удалить или с ними
// ещё идёт операция. Отметки пишутся в хронологию запуска runID, после
// которого уборка началась; если снапшот удалить не удалось, туда же
// попадают команды для администратора.
func (e *Engine) sweepLeftoverSnapshots(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, runID string) int {

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
			Statuses: []model.RunStatus{model.RunPending, model.RunRunning}, Limit: 1}); err != nil || len(active) > 0 {
			e.log.Info().Str("vm", vm.Name).Msg("уборка снапшотов отложена: идёт бэкап ВМ")
			return 0
		}
		// Зависшая передача образа держит снапшот: движок не удалит его, пока
		// она не закончена. Передача брошенного запуска никому не нужна.
		if !transfersLoaded {
			transfers, _ = client.ListImageTransfers(ctx)
			transfersLoaded = true
		}
		for _, t := range transfers {
			if t.Snapshot.ID == s.ID && !t.Terminal() {
				if err := client.CancelTransfer(ctx, t.ID); err == nil {
					e.log.Info().Str("transfer", t.ID).Str("snapshot", s.ID).
						Msg("отменена зависшая передача брошенного снапшота")
				}
			}
		}
		started := time.Now()
		err := client.DeleteSnapshotWhenReady(ctx, vm.ID, s.ID, 10*time.Minute)
		if err == nil || ovirt.IsNotFound(err) {
			err = client.WaitSnapshotGone(ctx, vm.ID, s.ID, 5*time.Hour)
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
				e.attachSteps(ctx, runID, SnapshotCleanupSteps(srv, vm.ID, s))
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
