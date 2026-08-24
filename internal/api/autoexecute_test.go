package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// jobFor заводит задание бэкапа, которое потом удаляют через согласование.
//
// Вместе с подключением: задание ссылается на сервер внешним ключом, и без
// него запись не пройдёт.
func jobFor(t *testing.T, srv *Server, name string) *model.BackupJob {
	t.Helper()
	ctx := context.Background()

	server := &model.Server{
		Name: "движок для " + name, Kind: model.KindOVirt,
		EngineURL: "https://engine.example", Username: "admin@internal", Enabled: true,
	}
	if err := srv.store.CreateServer(ctx, server); err != nil {
		t.Fatalf("создание подключения: %v", err)
	}

	job := &model.BackupJob{
		Name: name, ServerID: server.ID, Enabled: false,
		Schedule: "0 3 * * *", Type: model.BackupFull,
	}
	if err := srv.store.CreateBackupJob(ctx, job); err != nil {
		t.Fatalf("создание задания: %v", err)
	}
	return job
}

// Главное свойство уровня veto: подтверждение собрано, окно отмены вышло —
// действие выполняется само.
//
// Пока его доводил человек повторным запросом, окно работало наполовину: о
// забытой заявке никто не вспоминал, а согласующие были уверены, что действие
// состоялось.
func TestScheduledActionExecutesAfterVetoWindow(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	job := jobFor(t, srv, "под удаление по вето")
	ctx := context.Background()

	_, body := do(t, ts, "DELETE", "/jobs/"+job.ID+"?reason=задание+больше+не+нужно",
		cookies["инициатор"], "")
	id := requestIDFrom(t, body)

	if code, msg := do(t, ts, "POST", "/approvals/"+id+"/vote", cookies["первый"],
		`{"approve":true}`); code != http.StatusOK {
		t.Fatalf("голос не принят: %d %s", code, msg)
	}

	req, err := srv.store.GetApprovalRequest(ctx, id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if req.State != model.ApprovalScheduled {
		t.Fatalf("состояние %q, ожидалось scheduled", req.State)
	}

	// Пока окно не вышло, обход ничего не делает: иначе вето было бы
	// декоративным.
	srv.sweepDueApprovals(ctx)
	if _, err := srv.store.GetBackupJob(ctx, job.ID); err != nil {
		t.Fatalf("задание удалено до истечения окна отмены: %v", err)
	}

	// Окно вышло — действие доводится само.
	past := time.Now().UTC().Add(-time.Minute)
	if err := srv.store.SetApprovalState(ctx, id, model.ApprovalScheduled,
		req.DecidedAt, &past, req.Escalated, req.GroupName); err != nil {
		t.Fatalf("сдвиг окна отмены: %v", err)
	}
	srv.sweepDueApprovals(ctx)

	if _, err := srv.store.GetBackupJob(ctx, job.ID); err == nil {
		t.Fatal("задание не удалено после истечения окна отмены")
	}
	done, err := srv.store.GetApprovalRequest(ctx, id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if done.State != model.ApprovalExecuted {
		t.Errorf("состояние заявки %q, ожидалось executed", done.State)
	}
}

// Отменённая заявка не выполняется никогда: вето и есть смысл окна.
func TestVetoedActionNeverExecutes(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	job := jobFor(t, srv, "под вето")
	ctx := context.Background()

	_, body := do(t, ts, "DELETE", "/jobs/"+job.ID+"?reason=проверка+права+вето",
		cookies["инициатор"], "")
	id := requestIDFrom(t, body)

	if code, msg := do(t, ts, "POST", "/approvals/"+id+"/vote", cookies["первый"],
		`{"approve":true}`); code != http.StatusOK {
		t.Fatalf("голос не принят: %d %s", code, msg)
	}
	if code, msg := do(t, ts, "POST", "/approvals/"+id+"/cancel", cookies["второй"],
		""); code != http.StatusOK {
		t.Fatalf("вето не наложено: %d %s", code, msg)
	}

	srv.sweepDueApprovals(ctx)

	if _, err := srv.store.GetBackupJob(ctx, job.ID); err != nil {
		t.Fatalf("задание удалено вопреки вето: %v", err)
	}
	req, err := srv.store.GetApprovalRequest(ctx, id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if req.State != model.ApprovalVetoed {
		t.Errorf("состояние %q, ожидалось vetoed", req.State)
	}
}

// Истёкшая заявка не выполняется. Разница с окном вето принципиальная: там
// молчание означает согласие после подтверждения, здесь подтверждения не было
// вовсе, и выполнение по таймауту стало бы способом провести что угодно,
// дождавшись отпуска согласующих.
func TestUnconfirmedRequestIsNeverExecuted(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	job := jobFor(t, srv, "истечёт без подтверждений")
	ctx := context.Background()

	_, body := do(t, ts, "DELETE", "/jobs/"+job.ID+"?reason=никто+не+подтвердит",
		cookies["инициатор"], "")
	id := requestIDFrom(t, body)

	// Заявка без подтверждения не попадает в выборку готовых к выполнению: у
	// неё нет ни состояния scheduled, ни срока выполнения.
	srv.sweepDueApprovals(ctx)
	srv.sweepApprovals(ctx)
	srv.sweepDueApprovals(ctx)

	if _, err := srv.store.GetBackupJob(ctx, job.ID); err != nil {
		t.Fatalf("задание удалено без подтверждения: %v", err)
	}
	req, err := srv.store.GetApprovalRequest(ctx, id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if req.State == model.ApprovalExecuted {
		t.Error("неподтверждённая заявка отмечена выполненной")
	}
}

// Неудача не теряет заявку: причина записана, заявка ждёт повтора, а не
// исчезает выполненной.
func TestFailedExecutionKeepsRequestAndRecordsCause(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	job := jobFor(t, srv, "исчезнет до выполнения")
	ctx := context.Background()

	_, body := do(t, ts, "DELETE", "/jobs/"+job.ID+"?reason=проверка+разбора+неудачи",
		cookies["инициатор"], "")
	id := requestIDFrom(t, body)
	if code, msg := do(t, ts, "POST", "/approvals/"+id+"/vote", cookies["первый"],
		`{"approve":true}`); code != http.StatusOK {
		t.Fatalf("голос не принят: %d %s", code, msg)
	}

	// Задание удалили руками, пока шло окно отмены. Для заявки это не отказ:
	// то, ради чего она заводилась, уже произошло.
	if err := srv.store.DeleteBackupJob(ctx, job.ID); err != nil {
		t.Fatalf("удаление задания: %v", err)
	}

	req, err := srv.store.GetApprovalRequest(ctx, id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if err := srv.store.SetApprovalState(ctx, id, model.ApprovalScheduled,
		req.DecidedAt, &past, req.Escalated, req.GroupName); err != nil {
		t.Fatalf("сдвиг окна отмены: %v", err)
	}
	srv.sweepDueApprovals(ctx)

	done, err := srv.store.GetApprovalRequest(ctx, id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if done.State != model.ApprovalExecuted {
		t.Errorf("состояние %q, ожидалось executed: объекта уже нет, значит сделано",
			done.State)
	}
}

// Заявка, для которой исполнителя нет, обходом не трогается и остаётся ждать
// человека. Молча закрывать её выполненной — худшее, что можно сделать.
func TestActionWithoutExecutorWaitsForHuman(t *testing.T) {
	srv, _, _ := approvalFixture(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-time.Minute)
	req := &model.ApprovalRequest{
		Action: model.GuardStorageDelete, ObjectID: "нет-такого",
		Summary: "удаление хранилища", Requester: "инициатор", Reason: "проверка",
		State: model.ApprovalScheduled, Level: model.LevelVeto, Quorum: 1,
		GroupName: defaultApprovalGroup,
		CreatedAt: past, ExpiresAt: time.Now().UTC().Add(time.Hour),
		ExecuteAfter: &past,
	}
	if err := srv.store.CreateApprovalRequest(ctx, req); err != nil {
		t.Fatalf("создание заявки: %v", err)
	}

	srv.sweepDueApprovals(ctx)

	after, err := srv.store.GetApprovalRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if after.State != model.ApprovalScheduled {
		t.Errorf("состояние %q, ожидалось scheduled: исполнителя нет, заявка ждёт человека",
			after.State)
	}
}
