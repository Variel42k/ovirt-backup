package ovirt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseGuestDiskUsage(t *testing.T) {
	// Движок отдаёт числа строками; повторы и записи без пути отбрасываются.
	got := parseGuestDiskUsage(`[
		{"path":"/var/opt/gitlab","total":"214748364800","used":"152471339008","fs":"ext4"},
		{"path":"/","total":53660876800,"used":"3521986560","fs":"xfs"},
		{"path":"/","total":"1","used":"1","fs":"xfs"},
		{"path":"","total":"1","used":"1","fs":"tmpfs"}]`)
	if len(got) != 2 || got[0].Mountpoint != "/" || got[0].Type != "xfs" ||
		got[0].TotalBytes != 53660876800 || got[0].UsedBytes != 3521986560 ||
		got[1].Mountpoint != "/var/opt/gitlab" || got[1].UsedBytes != 152471339008 {
		t.Fatalf("unexpected filesystems: %+v", got)
	}
	for _, empty := range []string{"", "  ", "[]", "не JSON"} {
		if got := parseGuestDiskUsage(empty); len(got) != 0 {
			t.Fatalf("%q parsed into %+v", empty, got)
		}
	}
}

func TestVMGuestFilesystemsReadsDisksUsageStatistic(t *testing.T) {
	body := `{"statistic":[{"name":"memory.installed","values":{"value":[{"datum":1024}]}}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("/ovirt-engine/api/vms/vm-1/statistics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}

	// Агент ещё не сообщил: показателя нет — пустой список без ошибки.
	if got, err := client.VMGuestFilesystems(context.Background(), "vm-1"); err != nil || len(got) != 0 {
		t.Fatalf("statistic absent: %+v, %v", got, err)
	}
	body = `{"statistic":[
		{"name":"memory.installed","values":{"value":[{"datum":1024}]}},
		{"name":"disks.usage","type":"string","values":{"value":[{"detail":"[{\"path\":\"/\",\"total\":\"100\",\"used\":\"40\",\"fs\":\"ext4\"}]"}]}}]}`
	got, err := client.VMGuestFilesystems(context.Background(), "vm-1")
	if err != nil || len(got) != 1 || got[0].Mountpoint != "/" || got[0].TotalBytes != 100 || got[0].UsedBytes != 40 {
		t.Fatalf("disks.usage: %+v, %v", got, err)
	}
}
