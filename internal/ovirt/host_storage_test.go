package ovirt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostStorageDomainsScopesToDataCenterAndActiveData(t *testing.T) {
	for _, status := range []string{"up", "maintenance"} {
		t.Run(status, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/ovirt-engine/sso/oauth/token" {
					_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
					return
				}
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/ovirt-engine/api/hosts/host-1":
					_, _ = w.Write([]byte(`{"id":"host-1","name":"node-1","status":"` + status + `","cluster":{"id":"cluster-1"}}`))
				case "/ovirt-engine/api/clusters/cluster-1":
					if r.URL.Query().Get("follow") != "data_center" {
						t.Error("data center name must be requested")
					}
					_, _ = w.Write([]byte(`{"id":"cluster-1","name":"Default","data_center":{"id":"dc-1","name":"Local DC"}}`))
				case "/ovirt-engine/api/datacenters/dc-1/storagedomains":
					_, _ = w.Write([]byte(`{"storage_domain":[
						{"id":"local","name":"dengine-add1","type":"data","status":"active","storage":{"type":"localfs"},"available":"1024"},
						{"id":"shared","name":"shared-nfs","type":"data","status":"active","storage":{"type":"nfs"}},
						{"id":"inactive","type":"data","status":"maintenance"},
						{"id":"iso","type":"iso","status":"active"}]}`))
				default:
					t.Errorf("unexpected inventory request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client, err := New(Config{EngineURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			targets, err := client.HostStorageDomains(context.Background(), "engine-1", "host-1")
			if status != "up" {
				if err == nil || len(paths) != 1 {
					t.Fatalf("unavailable host accepted: %+v, %v, paths=%v", targets, err, paths)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if targets.HostName != "node-1" || targets.ClusterName != "Default" || targets.DataCenterName != "Local DC" {
				t.Fatalf("missing target location: %+v", targets)
			}
			if len(targets.Domains) != 2 || targets.Domains[0].ID != "local" || targets.Domains[1].ID != "shared" || targets.Domains[0].AvailableSize != 1024 || targets.Domains[0].ServerID != "engine-1" {
				t.Fatalf("incorrect storage filtering: %+v", targets.Domains)
			}
		})
	}
}

func TestHostStorageDomainsRejectsInvalidHostBeforeRequest(t *testing.T) {
	client, err := New(Config{EngineURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ".", "..", "../foreign", "host?query", "host#fragment"} {
		if _, err := client.HostStorageDomains(context.Background(), "engine-1", id); err == nil {
			t.Fatalf("invalid host accepted: %q", id)
		}
	}
}
