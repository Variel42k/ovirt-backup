package ovirt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Движок уже открыл бэкап и держит диски, даже если его ответ не разобрался.
// Идентификатор должен дойти до вызывающего, иначе закрыть бэкап будет некому.
func TestStartBackupKeepsIDWhenAnswerDoesNotDecode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"тестовый-токен","exp":"9999999999999"}`))
	})
	var sent map[string]any
	mux.HandleFunc("/ovirt-engine/api/vms/вм-1/backups", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "application/json")
		// description числом — поле, которое клиент ждёт строкой.
		_, _ = w.Write([]byte(`{"id":"бэкап-1","phase":"initializing","description":42}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}

	backup, err := client.StartBackup(context.Background(), "вм-1", []string{"диск-1"}, "", BackupMarker("run-1"))
	if err == nil {
		t.Fatal("ошибка разбора потеряна")
	}
	if backup == nil || backup.ID != "бэкап-1" {
		t.Fatalf("идентификатор бэкапа потерян: %+v", backup)
	}
	// По метке уборка узнаёт свой бэкап, даже если запуск не записал его id.
	if sent["description"] != "jhvirt run run-1" {
		t.Fatalf("метка службы не отправлена: %v", sent["description"])
	}
}
