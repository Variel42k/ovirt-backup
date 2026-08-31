package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Переименование хранилища проходит без согласования.
//
// Ради этой проверки согласование смены цели и делалось по содержимому
// запроса, а не по маршруту: под одним адресом живут две разные операции, и
// кворум за правку названия сделал бы согласование помехой вместо защиты.
func TestRenamingStorageNeedsNoApproval(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "старое название")

	code, body := do(t, ts, "PUT", "/storages/"+target.ID, cookies["инициатор"],
		`{"name":"новое название","kind":"local","base_path":`+quoted(target.BasePath)+`}`)
	if code != http.StatusOK {
		t.Fatalf("переименование потребовало согласования: %d %s", code, body)
	}

	updated, err := srv.store.GetStorageTarget(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("чтение хранилища: %v", err)
	}
	if updated.Name != "новое название" {
		t.Errorf("имя не изменилось: %q", updated.Name)
	}
}

// Смена пути — это увод будущих копий в другое место и потеря доступа к
// старым. Такое требует подписи второго человека.
func TestRetargetingStorageOpensRequest(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "смена цели")
	newPath := t.TempDir()

	code, body := do(t, ts, "PUT", "/storages/"+target.ID+"?reason=переезд+на+новый+массив", cookies["инициатор"],
		`{"name":"смена цели","kind":"local","base_path":`+quoted(newPath)+`}`)
	if code != http.StatusAccepted {
		t.Fatalf("смена пути выполнена без согласования: %d %s", code, body)
	}

	unchanged, err := srv.store.GetStorageTarget(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("чтение хранилища: %v", err)
	}
	if unchanged.BasePath != target.BasePath {
		t.Fatalf("путь изменён до согласования: %q", unchanged.BasePath)
	}

	// Заявка должна называть, что именно меняется: «смена цели хранилища» без
	// старого и нового значения подтверждают не глядя.
	id := requestIDFrom(t, body)
	req, err := srv.store.GetApprovalRequest(context.Background(), id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if req.ObjectID != target.ID {
		t.Errorf("заявка не привязана к хранилищу: %q", req.ObjectID)
	}
	if !strings.Contains(req.Summary, "каталог") {
		t.Errorf("в описании заявки не сказано, что меняется: %q", req.Summary)
	}

	// После кворума то же изменение проходит по ссылке на заявку.
	for _, who := range []string{"первый", "второй"} {
		if code, msg := do(t, ts, "POST", "/approvals/"+id+"/vote", cookies[who],
			`{"approve":true}`); code != http.StatusOK {
			t.Fatalf("голос %s не принят: %d %s", who, code, msg)
		}
	}
	code, body = do(t, ts, "PUT", "/storages/"+target.ID+"?approval="+id+"&reason=переезд+на+новый+массив", cookies["инициатор"],
		`{"name":"смена цели","kind":"local","base_path":`+quoted(newPath)+`}`)
	if code != http.StatusOK {
		t.Fatalf("согласованная смена цели не выполнена: %d %s", code, body)
	}

	changed, err := srv.store.GetStorageTarget(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("чтение хранилища: %v", err)
	}
	if changed.BasePath != newPath {
		t.Errorf("путь не изменился после согласования: %q", changed.BasePath)
	}
}

func TestRetargetingStorageRejectsPathOutsideWritableRoots(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "выход за корень")
	outside := string(filepath.Separator)
	if volume := filepath.VolumeName(os.TempDir()); volume != "" {
		outside = volume + string(filepath.Separator)
	}

	code, body := do(t, ts, "PUT", "/storages/"+target.ID+"?reason=проверка+пути", cookies["инициатор"],
		`{"name":"выход за корень","kind":"local","base_path":`+quoted(outside)+`}`)
	if code != http.StatusBadRequest {
		t.Fatalf("путь вне разрешённых корней принят: %d %s", code, body)
	}
	unchanged, err := srv.store.GetStorageTarget(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.BasePath != target.BasePath {
		t.Fatalf("отклонённый путь сохранён: %q", unchanged.BasePath)
	}
}

// Секреты не должны попадать в описание заявки: она живёт в базе и уходит в
// оповещения, то есть ключ оказался бы сразу в двух местах, откуда его не
// отозвать.
func TestRetargetSummaryHidesSecrets(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "секреты")

	const secret = "очень-секретный-ключ"
	_, body := do(t, ts, "PUT", "/storages/"+target.ID+"?reason=плановая+смена+ключа+доступа", cookies["инициатор"],
		`{"name":"секреты","kind":"local","base_path":`+quoted(target.BasePath)+
			`,"password":"`+secret+`"}`)

	id := requestIDFrom(t, body)
	req, err := srv.store.GetApprovalRequest(context.Background(), id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if strings.Contains(req.Summary, secret) {
		t.Fatalf("секрет попал в описание заявки: %q", req.Summary)
	}
	if !strings.Contains(req.Summary, "учётные данные") {
		t.Errorf("замена учётных данных не отражена в описании: %q", req.Summary)
	}
}

// quoted — строка как литерал JSON. Пути в Windows содержат обратные слэши,
// и подставлять их в тело запроса без экранирования нельзя.
func quoted(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
