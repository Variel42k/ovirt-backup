package ovirt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Смешанный бэкап (oVirt 4.4.5+): режим каждого диска сообщает движок, и
// служба читает диск целиком или изменения по нему.
func TestBackupDiskModes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"тестовый-токен","exp":"9999999999999"}`))
	})
	mux.HandleFunc("/ovirt-engine/api/vms/вм-1/backups/бэкап-1/disks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disk":[
			{"id":"raw-1","backup":"none","backup_mode":"full"},
			{"id":"qcow-1","backup":"incremental","backup_mode":"incremental"},
			{"id":"old-engine"}
		]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	modes, err := client.BackupDiskModes(context.Background(), "вм-1", "бэкап-1")
	if err != nil {
		t.Fatal(err)
	}
	if modes["raw-1"] != "full" || modes["qcow-1"] != "incremental" {
		t.Fatalf("режимы дисков = %v", modes)
	}
	// Движок старше 4.4.5 режима не сообщает — такого диска в карте нет.
	if _, ok := modes["old-engine"]; ok {
		t.Fatalf("диск без режима не должен попадать в карту: %v", modes)
	}
}
