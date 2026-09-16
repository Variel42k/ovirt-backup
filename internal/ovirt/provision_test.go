package ovirt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Движок РЕД/oVirt отдаёт administrative строкой ("true"/"false"); разбор
// /roles не должен на этом падать.
func TestRoleDecodesStringAdministrative(t *testing.T) {
	var list roleList
	body := `{"role":[{"id":"1","name":"SuperUser","administrative":"true"},{"id":"2","name":"UserRole","administrative":"false"}]}`
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("разбор /roles со строковым administrative: %v", err)
	}
	if len(list.Role) != 2 || !bool(list.Role[0].Administrative) || bool(list.Role[1].Administrative) {
		t.Fatalf("administrative разобрался неверно: %+v", list.Role)
	}
}

// SelectPermits оставляет существующие права и отдельно возвращает отброшенные;
// пустой каталог означает «не выяснить» — тогда набор проходит как есть.
func TestSelectPermits(t *testing.T) {
	catalog := map[string]string{"login": "1300", "backup_disk": "1600", "access_image_storage": "1700"}

	use, skipped := SelectPermits([]string{"login", "access_image_transfer", "backup_disk"}, catalog)
	if len(use) != 2 || use[0] != "login" || use[1] != "backup_disk" {
		t.Fatalf("ожидались login,backup_disk; получено %v", use)
	}
	if len(skipped) != 1 || skipped[0] != "access_image_transfer" {
		t.Fatalf("ожидалось отбросить access_image_transfer; получено %v", skipped)
	}

	// Пустой каталог: ничего не отбрасываем, отдаём как есть.
	use, skipped = SelectPermits([]string{"login", "whatever"}, nil)
	if len(use) != 2 || len(skipped) != 0 {
		t.Fatalf("пустой каталог должен пропускать всё: use=%v skipped=%v", use, skipped)
	}
}

// EnginePermits объединяет права (имя→id) по всем уровням кластера, встроенным в
// коллекцию /clusterlevels.
func TestEnginePermitsUnionsLevels(t *testing.T) {
	client := engineStub(t, map[string]any{
		"clusterlevels": `{"cluster_level":[
			{"id":"4.3","permits":{"permit":[
				{"name":"login","id":"1300"},{"name":"backup_disk","id":"1600"},
				{"name":"access_image_storage","id":"1700"},{"name":"manipulate_vm_snapshots","id":"5"},
				{"name":"create_disk","id":"20"},{"name":"delete_disk","id":"21"},
				{"name":"configure_disk_storage","id":"22"},{"name":"create_vm","id":"1"},
				{"name":"edit_vm_properties","id":"3"},{"name":"configure_vm_storage","id":"23"}]}},
			{"id":"4.6","permits":{"permit":[
				{"name":"login","id":"1300"},{"name":"sparsify_disk","id":"99"}]}}
		]}`,
	})

	catalog, err := client.EnginePermits(context.Background())
	if err != nil {
		t.Fatalf("каталог: %v", err)
	}
	// Имя→id, объединение по уровням: право только с 4.6 тоже попадает в каталог.
	if catalog["login"] != "1300" {
		t.Fatalf("ожидался login=1300, получено %q", catalog["login"])
	}
	if catalog["sparsify_disk"] != "99" {
		t.Fatalf("объединение по уровням не сработало: %v", catalog)
	}
	// Реальное имя есть, устаревшее — нет.
	if catalog["access_image_storage"] == "" || catalog["access_image_transfer"] != "" {
		t.Fatalf("каталог должен знать access_image_storage и не знать access_image_transfer: %v", catalog)
	}

	// Весь набор по умолчанию должен резолвиться в этом каталоге без остатка.
	_, skipped := SelectPermits(DefaultBackupPermits, catalog)
	if len(skipped) != 0 {
		t.Fatalf("DefaultBackupPermits содержит имена вне каталога движка: %v", skipped)
	}
}

// CreateRole обязан слать права прямо в теле создания и по id — движок РЕД/oVirt
// требует role.permits.id и не принимает добавление по имени отдельным шагом.
func TestCreateRoleSendsPermitIDs(t *testing.T) {
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"t","exp":"9999999999999"}`))
	})
	mux.HandleFunc("/ovirt-engine/api/roles", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"role-1","name":"jhvirt-backup","administrative":"true"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := New(Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	catalog := map[string]string{"login": "1300", "backup_disk": "1600"}
	role, err := client.CreateRole(context.Background(), "jhvirt-backup", "desc",
		[]string{"login", "backup_disk"}, catalog)
	if err != nil {
		t.Fatalf("создание роли: %v", err)
	}
	if role.ID != "role-1" {
		t.Fatalf("id роли: %q", role.ID)
	}

	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("тело запроса: %v", err)
	}
	permitsWrap, ok := body["permits"].(map[string]any)
	if !ok {
		t.Fatalf("в теле нет permits: %s", gotBody)
	}
	permits, ok := permitsWrap["permit"].([]any)
	if !ok || len(permits) != 2 {
		t.Fatalf("ожидались 2 права: %s", gotBody)
	}
	first := permits[0].(map[string]any)
	if first["id"] != "1300" {
		t.Fatalf("право должно ссылаться по id 1300: %v", first)
	}
	if _, hasName := first["name"]; hasName {
		t.Fatalf("право должно быть по id, без name: %v", first)
	}

	// Пустое пересечение с каталогом — понятная ошибка, а не запрос без прав.
	if _, err := client.CreateRole(context.Background(), "x", "d",
		[]string{"nonexistent"}, catalog); err == nil {
		t.Fatal("ожидалась ошибка при отсутствии сопоставленных прав")
	}
}

// DefaultBackupPermits не должен содержать несуществующее на движке имя, из-за
// которого движок отвергал бы всю роль.
func TestDefaultBackupPermitsUseRealNames(t *testing.T) {
	for _, p := range DefaultBackupPermits {
		if p == "access_image_transfer" {
			t.Fatal("access_image_transfer не существует на движке; нужно access_image_storage")
		}
	}
}
