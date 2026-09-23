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

// Уборка на движке.
//
// Пока бэкап движка открыт, диски ВМ заблокированы, и следующий бэкап
// получает 409 «Disk is locked». Бэкап остаётся открытым, если запуск не
// дошёл до finalize: служба упала или перезапустилась, связь оборвалась,
// прежняя версия не смогла разобрать ответ движка и потеряла идентификатор.
//
// Служба закрывает только свои бэкапы. На той же ВМ может работать другая
// система копирования, и закрыть её бэкап посреди чтения значит сорвать
// чужую копию. Поэтому то, что не удалось опознать как своё или закрыть,
// превращается в готовые команды для администратора в карточке запуска.

// leftoverWindow — насколько момент создания бэкапа на движке может
// отличаться от начала запуска: часы службы и движка расходятся, а запрос на
// создание уходит не в первую секунду запуска.
const leftoverWindow = 10 * time.Minute

// decodeFailureMarks — след прежней версии, которая открывала бэкап, но не
// могла разобрать ответ движка и не сохраняла его идентификатор.
var decodeFailureMarks = []string{"разбор ответа POST", "/backups"}

// ownerOf сообщает, чей открытый бэкап движка: запуска службы (его id) или
// неизвестно чей (пусто). live — бэкап принадлежит запуску, который ещё
// идёт: его трогать нельзя, он не остаток.
func ownerOf(b ovirt.Backup, runs []*model.BackupRun) (runID string, live bool) {
	active := func(r *model.BackupRun) bool {
		return r.Status == model.RunPending || r.Status == model.RunRunning
	}
	if strings.HasPrefix(b.Description, ovirt.BackupMarkerPrefix) {
		id := strings.TrimSpace(strings.TrimPrefix(b.Description, ovirt.BackupMarkerPrefix))
		for _, r := range runs {
			if r.ID == id {
				return r.ID, active(r)
			}
		}
		// Метка наша, а запуска в базе нет (удалён вместе с историей):
		// бэкап всё равно открыт службой, и ждать его некому.
		return id, false
	}
	for _, r := range runs {
		if r.EngineBackupID != "" && r.EngineBackupID == b.ID {
			return r.ID, active(r)
		}
	}
	created := b.CreationDate.Time()
	if created.IsZero() {
		return "", false
	}
	for _, r := range runs {
		if r.Status != model.RunFailed || r.EngineBackupID != "" || !hasAll(r.Error, decodeFailureMarks) {
			continue
		}
		at := r.CreatedAt
		if r.StartedAt != nil {
			at = *r.StartedAt
		}
		if d := created.Sub(at); d > -leftoverWindow && d < leftoverWindow {
			return r.ID, false
		}
	}
	return "", false
}

func hasAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// releaseEngineLeftovers закрывает на движке бэкапы этой ВМ, которые открыла
// служба и не закрыла. Вызывается до заморозки гостя: уборка занимает
// минуты, и держать всё это время запись остановленной незачем.
//
// Ошибка означает, что ВМ по-прежнему заблокирована. В run.ManualSteps к
// этому моменту записаны готовые команды для администратора.
func (e *Engine) releaseEngineLeftovers(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, run *model.BackupRun, diskIDs []string) error {

	backups, err := client.ListBackups(ctx, vm.ID)
	if err != nil {
		// Не удалось посмотреть — не повод отказываться от бэкапа: если ВМ
		// действительно заблокирована, движок ответит 409, и команды
		// соберутся там.
		e.log.Warn().Err(err).Str("vm", vm.Name).Msg("не удалось проверить незакрытые бэкапы на движке")
		return nil
	}
	var open []ovirt.Backup
	for _, b := range backups {
		if b.Open() {
			open = append(open, b)
		}
	}
	if len(open) == 0 {
		return nil
	}

	runs, err := e.store.ListBackupRuns(ctx, store.RunFilter{
		ServerID: srv.ID, VMID: vm.ID, IncludeDeleted: true, Limit: 500,
	})
	if err != nil {
		return fmt.Errorf("история запусков ВМ для уборки на движке: %w", err)
	}

	transfers, transfersErr := client.ListImageTransfers(ctx)
	if transfersErr != nil {
		e.log.Warn().Err(transfersErr).Msg("не удалось получить передачи образов движка")
	}

	var blocking []ovirt.Backup
	var reasons []string
	for _, b := range open {
		owner, live := ownerOf(b, runs)
		switch {
		case live:
			blocking = append(blocking, b)
			reasons = append(reasons, fmt.Sprintf("бэкап %s принадлежит идущему запуску %s", b.ID, owner))
		case owner == "":
			blocking = append(blocking, b)
			reasons = append(reasons, fmt.Sprintf("бэкап %s открыт не службой (описание %q)", b.ID, b.Description))
		default:
			if err := e.closeLeftover(ctx, client, vm, b, transfers); err != nil {
				blocking = append(blocking, b)
				reasons = append(reasons, fmt.Sprintf("свой бэкап %s закрыть не удалось: %v", b.ID, err))
				continue
			}
			e.event(ctx, run, model.RunEventLeftoverClosed, 0,
				fmt.Sprintf("закрыт бэкап движка %s, оставленный запуском %s", b.ID, owner))
			e.log.Info().Str("vm", vm.Name).Str("backup", b.ID).Str("запуск", owner).
				Msg("закрыт незакрытый бэкап движка")
		}
	}
	if len(blocking) == 0 {
		return nil
	}

	run.ManualSteps = EngineUnlockSteps(srv, vm.ID, blocking, relatedTransfers(transfers, blocking, diskIDs), diskIDs)
	return fmt.Errorf("диски ВМ заблокированы открытым бэкапом на движке: %s. "+
		"Готовые команды для ручной уборки — в карточке запуска", strings.Join(reasons, "; "))
}

// closeLeftover отменяет передачи образов бэкапа и закрывает его.
func (e *Engine) closeLeftover(ctx context.Context, client *ovirt.Client, vm *model.VM,
	b ovirt.Backup, transfers []ovirt.ImageTransfer) error {

	for _, t := range transfers {
		if t.Backup.ID == b.ID && !t.Terminal() {
			if err := client.CancelTransfer(ctx, t.ID); err != nil && !ovirt.IsNotFound(err) {
				return fmt.Errorf("отмена передачи образа %s: %w", t.ID, err)
			}
		}
	}
	if b.Phase != "finalizing" {
		if err := client.FinalizeBackup(ctx, vm.ID, b.ID); err != nil && !ovirt.IsNotFound(err) {
			return err
		}
	}
	return client.WaitBackupFinalized(ctx, vm.ID, b.ID, 5*time.Minute)
}

// relatedTransfers — незавершённые передачи, которые держат диски ВМ: по
// бэкапу или по диску.
func relatedTransfers(all []ovirt.ImageTransfer, backups []ovirt.Backup, diskIDs []string) []ovirt.ImageTransfer {
	var out []ovirt.ImageTransfer
	for _, t := range all {
		if t.Terminal() {
			continue
		}
		related := false
		for _, b := range backups {
			if t.Backup.ID != "" && t.Backup.ID == b.ID {
				related = true
			}
		}
		for _, id := range diskIDs {
			if t.Disk.ID == id {
				related = true
			}
		}
		if related {
			out = append(out, t)
		}
	}
	return out
}

// engineLockSteps собирает команды для 409 при старте бэкапа, когда уборка
// до заморозки ничего не нашла: блокировку держит что-то другое — передача
// образа, снапшот, перенос диска.
func (e *Engine) engineLockSteps(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vmID string, diskIDs []string) []model.ManualStep {

	var open []ovirt.Backup
	if backups, err := client.ListBackups(ctx, vmID); err == nil {
		for _, b := range backups {
			if b.Open() {
				open = append(open, b)
			}
		}
	}
	transfers, _ := client.ListImageTransfers(ctx)
	return EngineUnlockSteps(srv, vmID, open, relatedTransfers(transfers, open, diskIDs), diskIDs)
}

// caFile — куда шаг с сертификатом кладёт CA движка. Имя постоянное: команды
// должны выполняться без правки.
const caFile = "/tmp/jhvirt-engine-ca.pem"

// EngineUnlockSteps строит команды, которые освобождают диски ВМ на движке.
//
// Всё подставлено: адрес API, пользователь, идентификаторы, сертификат
// движка. Пароль curl спрашивает сам (-u без пароля), поэтому он не попадает
// ни в команду, ни в историю оболочки, ни в базу службы.
func EngineUnlockSteps(srv *model.Server, vmID string, backups []ovirt.Backup,
	transfers []ovirt.ImageTransfer, diskIDs []string) []model.ManualStep {

	api, err := ovirt.APIURL(srv.EngineURL)
	if err != nil {
		api = strings.TrimRight(srv.EngineURL, "/") + "/ovirt-engine/api"
	}
	user := srv.Username
	if user == "" {
		user = "admin@internal"
	}
	const anywhere = "любая машина с доступом к движку (например, сервер службы)"

	var steps []model.ManualStep
	tls := ""
	switch {
	case srv.InsecureTLS:
		tls = "-k "
	case strings.TrimSpace(srv.CACert) != "":
		tls = "--cacert " + caFile + " "
		steps = append(steps, model.ManualStep{
			Title:   "Сохранить сертификат движка",
			Where:   anywhere,
			Detail:  "Тот же сертификат, которому доверяет служба: команды ниже проверяют подлинность движка.",
			Command: "cat > " + caFile + " <<'JHVIRT_CA'\n" + strings.TrimSpace(srv.CACert) + "\nJHVIRT_CA",
		})
	}
	curl := "curl -sS " + tls + "-u " + shellQuote(user) + " -H 'Accept: application/json'"
	post := curl + " -H 'Content-Type: application/json' -X POST -d '{}'"

	steps = append(steps, model.ManualStep{
		Title: "Посмотреть бэкапы ВМ на движке",
		Where: anywhere,
		Detail: "curl спросит пароль " + user + ". Открытые бэкапы — с phase initializing, starting, " +
			"ready или finalizing: они и держат диски.",
		Command: curl + " " + shellQuote(api+"/vms/"+vmID+"/backups"),
	})

	for _, t := range transfers {
		steps = append(steps, model.ManualStep{
			Title: fmt.Sprintf("Отменить передачу образа %s", t.ID),
			Where: anywhere,
			Detail: fmt.Sprintf("Передача в фазе %s держит диск %s. Данные этой передачи никому не нужны: "+
				"запуск, который её открыл, уже завершился.", t.Phase, t.Disk.ID),
			Command: post + " " + shellQuote(api+"/imagetransfers/"+t.ID+"/cancel"),
		})
	}

	for _, b := range backups {
		ours := strings.HasPrefix(b.Description, ovirt.BackupMarkerPrefix)
		step := model.ManualStep{
			Title:   fmt.Sprintf("Закрыть бэкап движка %s", b.ID),
			Where:   anywhere,
			Command: post + " " + shellQuote(api+"/vms/"+vmID+"/backups/"+b.ID+"/finalize"),
		}
		created := ""
		if t := b.CreationDate.Time(); !t.IsZero() {
			created = ", создан " + t.Local().Format("02.01.2006 15:04")
		}
		if ours {
			step.Detail = fmt.Sprintf("Фаза %s%s. Бэкап открыт службой (%s); служба не смогла закрыть его сама.",
				b.Phase, created, b.Description)
		} else {
			step.Risky = true
			step.Detail = fmt.Sprintf("Фаза %s%s, описание %q. Служба не опознала этот бэкап как свой: "+
				"его могла открыть другая система копирования. Закрывайте, только если уверены, что он брошен, "+
				"иначе сорвёте чужую копию.", b.Phase, created, b.Description)
		}
		steps = append(steps, step)
	}

	steps = append(steps, model.ManualStep{
		Title:   "Проверить, что диски освободились",
		Where:   anywhere,
		Detail:  "У всех дисков ВМ должно быть \"status\": \"ok\". Закрытие бэкапа занимает до нескольких минут.",
		Command: curl + " " + shellQuote(api+"/vms/"+vmID+"/diskattachments?follow=disk") + ` | grep -o '"status" *: *"[a-z_]*"'`,
	})

	if len(diskIDs) > 0 {
		const unlock = "/usr/share/ovirt-engine/setup/dbutils/unlock_entity.sh"
		steps = append(steps,
			model.ManualStep{
				Title:   "Если диски всё ещё заблокированы: посмотреть блокировки в базе движка",
				Where:   "хост движка, root",
				Detail:  "Только просмотр, ничего не меняет.",
				Command: unlock + " -t disk -q",
			},
			model.ManualStep{
				Title: "Крайняя мера: снять блокировку с дисков ВМ",
				Where: "хост движка, root",
				Risky: true,
				Detail: "Только если шаги выше не помогли и на движке точно нет идущего бэкапа, передачи образа, " +
					"снапшота или переноса этих дисков. Снятие блокировки с диска, с которым идёт операция, " +
					"может повредить образ.",
				Command: unlock + " -t disk " + strings.Join(quoteAll(diskIDs), " "),
			})
	}
	return steps
}

// shellQuote заключает строку в одинарные кавычки для sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteAll(items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = shellQuote(s)
	}
	return out
}
