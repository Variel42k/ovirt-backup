package proxmox

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testPEM(server *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
}

func TestFetchInventoryAndPowerAction(t *testing.T) {
	const tokenID = "backup@pve!jhvirt"
	const tokenSecret = "00000000-1111-2222-3333-444444444444"
	var actionPath, actionTarget string

	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken="+tokenID+"="+tokenSecret {
			t.Errorf("unexpected authorization header: %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var data any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/version":
			data = map[string]any{"version": "8.4.1", "release": "8.4", "repoid": "abc"}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/cluster/status":
			data = []map[string]any{
				{"type": "cluster", "name": "prod", "nodes": 2, "quorate": 1},
				{"type": "node", "name": "pve01", "ip": "10.0.0.11", "online": 1},
				{"type": "node", "name": "pve02", "ip": "10.0.0.12", "online": 1},
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/cluster/resources" && r.URL.Query().Get("type") == "node":
			data = []map[string]any{
				{"id": "node/pve01", "type": "node", "node": "pve01", "status": "online", "maxcpu": 16, "mem": 1024, "maxmem": 4096},
				{"id": "node/pve02", "type": "node", "node": "pve02", "status": "offline", "maxcpu": "8", "mem": 0, "maxmem": 2048},
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/cluster/resources" && r.URL.Query().Get("type") == "vm":
			data = []map[string]any{
				{"id": "qemu/101", "type": "qemu", "vmid": 101, "name": "database", "node": "pve01", "status": "running", "maxcpu": 4, "maxmem": 2048, "tags": "prod;database"},
				{"id": "lxc/102", "type": "lxc", "vmid": 102, "name": "proxy", "node": "pve02", "status": "stopped", "maxcpu": 2, "maxmem": 1024},
				{"id": "qemu/9000", "type": "qemu", "vmid": 9000, "name": "template", "template": 1},
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/cluster/resources" && r.URL.Query().Get("type") == "storage":
			data = []map[string]any{
				{"id": "storage/pve01/shared", "type": "storage", "storage": "shared", "node": "pve01", "status": "available", "shared": 1, "disk": 100, "maxdisk": 1000, "plugintype": "nfs"},
				{"id": "storage/pve02/shared", "type": "storage", "storage": "shared", "node": "pve02", "status": "available", "shared": 1, "disk": 100, "maxdisk": 1000, "plugintype": "nfs"},
				{"id": "storage/pve01/local", "type": "storage", "storage": "local", "node": "pve01", "status": "available", "disk": 200, "maxdisk": 500, "plugintype": "dir"},
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve01/qemu/101/config":
			data = map[string]any{
				"scsi0":      "shared:vm-101-disk-0,size=64G",
				"efidisk0":   "shared:vm-101-disk-1,size=4M,efitype=4m",
				"ide2":       "none,media=cdrom",
				"cipassword": "must-never-enter-the-manifest",
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/nodes/pve01/qemu/101/status/current":
			data = map[string]any{"diskread": "123456", "diskwrite": 789012}
		case r.Method == http.MethodGet && r.URL.Path == "/api2/json/cluster/nextid":
			data = "107"
		case r.Method == http.MethodPost && r.URL.Path == "/api2/json/nodes/pve01/qemu/101/migrate":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			actionPath, actionTarget = r.URL.Path, r.Form.Get("target")
			data = "UPID:pve01:123"
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})

	server := httptest.NewTLSServer(mux)
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, TokenID: tokenID, TokenSecret: tokenSecret, CACert: testPEM(server)})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := client.FetchInventory(t.Context(), "server-id")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Info.ClusterName != "prod" || !inv.Info.Quorate || len(inv.Clusters) != 1 || len(inv.Hosts) != 2 || len(inv.VMs) != 2 || len(inv.Domains) != 2 {
		t.Fatalf("unexpected inventory: info=%+v clusters=%d hosts=%d vms=%d domains=%d",
			inv.Info, len(inv.Clusters), len(inv.Hosts), len(inv.VMs), len(inv.Domains))
	}
	if inv.VMs[0].Name != "database" || inv.VMs[0].HostName != "pve01" || len(inv.VMs[0].Tags) != 2 {
		t.Fatalf("unexpected VM mapping: %+v", inv.VMs[0])
	}
	if inv.Hosts[0].ActiveVMs != 1 || inv.Hosts[1].Status != "down" {
		t.Fatalf("unexpected host mapping: %+v", inv.Hosts)
	}
	node, err := client.GuestNode(t.Context(), "qemu/101")
	if err != nil || node != "pve01" {
		t.Fatalf("unexpected current guest node %q: %v", node, err)
	}
	read, write, err := client.GuestIO(t.Context(), "qemu/101")
	if err != nil || read != 123456 || write != 789012 {
		t.Fatalf("unexpected guest IO: read=%d write=%d err=%v", read, write, err)
	}
	if err := client.VMAction(t.Context(), "qemu/101", "pve01", "migrate", "node/pve02"); err != nil {
		t.Fatal(err)
	}
	if actionPath == "" || actionTarget != "pve02" {
		t.Fatalf("migration not submitted: path=%q target=%q", actionPath, actionTarget)
	}
	metadata, err := client.BackupMetadata(t.Context(), "qemu/101", "pve01")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.GuestKind != "qemu" || metadata.SourceNode != "pve01" || metadata.RootFSSize != "" ||
		metadata.ProvisionedSize != (64<<30)+(4<<20) {
		t.Fatalf("unexpected native backup metadata: %+v", metadata)
	}
	vmID, err := client.NextVMID(t.Context())
	if err != nil || vmID != "107" {
		t.Fatalf("unexpected next VMID %q: %v", vmID, err)
	}
}

func TestFetchCertificateChain(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	bundle, err := FetchCertificateChain(t.Context(), server.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode([]byte(bundle))
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatalf("unexpected certificate bundle: %q", bundle)
	}
	if string(block.Bytes) != string(server.Certificate().Raw) {
		t.Fatal("retrieved certificate differs from the server certificate")
	}
}

func TestNewRejectsUnsafeEndpointAndTokenID(t *testing.T) {
	for _, endpoint := range []string{
		"http://pve.example.org:8006", "https://user:pass@pve.example.org:8006",
		"https://pve.example.org:8006/unrelated", "ftp://pve.example.org",
	} {
		if _, err := New(Config{BaseURL: endpoint, TokenID: "user@pve!token", TokenSecret: "secret"}); err == nil {
			t.Errorf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	for _, tokenID := range []string{"", "user@pve", "token", "user@pve!", "user@pve!one!two", "user@pve!bad/token"} {
		if _, err := New(Config{BaseURL: "https://pve.example.org:8006", TokenID: tokenID, TokenSecret: "secret"}); err == nil {
			t.Errorf("invalid token ID accepted: %q", tokenID)
		}
	}
}

func TestParseVMIDUsesProxmoxRange(t *testing.T) {
	for _, value := range []string{"qemu/100", "lxc/999999999"} {
		if _, _, err := ParseVMID(value); err != nil {
			t.Errorf("valid VMID %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"qemu/0", "qemu/99", "qemu/0100", "lxc/1000000000", "qemu/-1", "vm/100"} {
		if _, _, err := ParseVMID(value); err == nil {
			t.Errorf("invalid VMID %q accepted", value)
		}
	}
}

func TestErrorsRedactTokenSecretAndRedirectIsRejected(t *testing.T) {
	const secret = "super-secret-token"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(secret))
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/version") {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		http.Error(w, "failure "+secret, http.StatusBadGateway)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, TokenID: "user@pve!token", TokenSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	var version versionResponse
	err = client.do(t.Context(), http.MethodGet, "/version", nil, &version)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "перенаправил") {
		t.Fatalf("unsafe redirect/error handling: %v", err)
	}
	err = client.do(t.Context(), http.MethodGet, "/cluster/status", nil, &version)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[скрыто]") {
		t.Fatalf("token secret leaked from API error: %v", err)
	}
}
