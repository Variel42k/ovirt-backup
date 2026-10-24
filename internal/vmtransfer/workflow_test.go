package vmtransfer

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
	"github.com/rs/zerolog"
)

// The coordinator uses real database locks and real oVirt power-management
// requests. Only the already separately tested backup/restore data plane is fake.
type workflowEngine struct {
	st                       *store.Store
	backend                  repo.Backend
	storage                  *model.StorageTarget
	manifests                []*backup.DiskManifest
	sourceState, targetState string
	targetGone               bool
	backupStates             []string
	deltaCalls               int
}

func (e *workflowEngine) Execute(ctx context.Context, r backup.RunRequest) (*model.BackupRun, error) {
	e.backupStates = append(e.backupStates, e.sourceState)
	id := fmt.Sprintf("run-%d", len(e.manifests))
	parent := ""
	if len(e.manifests) > 0 {
		parent = e.manifests[len(e.manifests)-1].RunID
	}
	run := &model.BackupRun{ID: id, ServerID: r.ServerID, VMID: r.VMID, StorageTargetID: r.StorageTargetID, Type: r.Type, Status: model.RunSucceeded, DiskCount: 1, ParentRunID: parent}
	if err := e.st.CreateTransferBackupRun(ctx, run, r.TransferID); err != nil {
		return nil, err
	}
	if r.OnRunCreated != nil {
		r.OnRunCreated(run)
	}
	m := &backup.DiskManifest{RunID: id, DiskID: "disk", VirtualSize: 65536, Type: r.Type}
	w, err := backup.NewDiskWriter(ctx, m, backup.WriterOptions{Backend: e.backend, DataKey: id, ChunkSize: 65536, Compression: "none"})
	if err != nil {
		return nil, err
	}
	if err = w.WriteChunk(0, bytes.Repeat([]byte{byte(len(e.manifests) + 1)}, 65536)); err != nil {
		return nil, err
	}
	m, err = w.Close()
	if err != nil {
		return nil, err
	}
	e.manifests = append(e.manifests, m)
	return run, nil
}
func (e *workflowEngine) Verify(context.Context, string, model.VerifyMode, model.VerifyOptions) (*model.VerifyRun, error) {
	return &model.VerifyRun{}, nil
}
func (e *workflowEngine) RestoreVM(_ context.Context, r *model.RestoreVMRequest) (*backup.RestoreVMResult, error) {
	e.targetState = "down"
	if err := r.OnVMCreated("target-vm"); err != nil {
		return nil, err
	}
	if err := r.OnDiskRestored("disk", "target-disk"); err != nil {
		return nil, err
	}
	return &backup.RestoreVMResult{VMID: "target-vm"}, nil
}
func (e *workflowEngine) LoadChain(ctx context.Context, _ string) (*backup.ChainSet, error) {
	b, err := repo.Open(ctx, e.storage)
	if err != nil {
		return nil, err
	}
	return &backup.ChainSet{Backend: b, DiskOrder: []string{"disk"}, Manifests: map[string][]*backup.DiskManifest{"disk": e.manifests}}, nil
}
func (e *workflowEngine) LoadVMChainCopy(context.Context, string, string) (*backup.ChainSet, error) {
	return &backup.ChainSet{RunManifest: &backup.RunManifest{VMProfile: &backup.VMProfile{}}}, nil
}
func (e *workflowEngine) Restore(_ context.Context, r backup.RestoreRequest) (*model.RestoreRun, error) {
	if r.DeltaAfterRunID == "" || r.TargetDiskID != "target-disk" || e.targetState != "down" {
		return nil, fmt.Errorf("unsafe delta request")
	}
	e.deltaCalls++
	if r.Progress != nil {
		r.Progress(65536, 65536, 65536)
	}
	return &model.RestoreRun{}, nil
}

func TestColdAndWarmWorkflowAcrossIndependentEngines(t *testing.T) {
	for _, mode := range []string{"cold", "warm"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := storetest.New(t)
			eng := &workflowEngine{st: st, sourceState: "up", targetState: "down"}
			handler := func(target bool) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					path := strings.TrimPrefix(r.URL.Path, "/ovirt-engine/api")
					switch {
					case strings.HasSuffix(path, "/sso/oauth/token"):
						fmt.Fprint(w, `{"access_token":"test","expires_in":3600}`)
					case strings.HasSuffix(path, "/services/sso-logout"):
						fmt.Fprint(w, `{}`)
					case path == "/vms/vm/shutdown":
						eng.sourceState = "down"
						fmt.Fprint(w, `{}`)
					case path == "/vms/vm/start":
						if !eng.targetGone {
							t.Error("source restarted with live target")
						}
						eng.sourceState = "up"
						fmt.Fprint(w, `{}`)
					case path == "/vms/target-vm/start":
						if eng.sourceState != "down" {
							t.Error("target started with live source")
						}
						eng.targetState = "up"
						fmt.Fprint(w, `{}`)
					case path == "/vms/target-vm/shutdown":
						eng.targetState = "down"
						fmt.Fprint(w, `{}`)
					case path == "/vms/target-vm" && r.Method == "DELETE":
						eng.targetGone = true
						fmt.Fprint(w, `{}`)
					case path == "/vms/target-vm":
						if eng.targetGone {
							w.WriteHeader(404)
							fmt.Fprint(w, `{}`)
						} else {
							fmt.Fprintf(w, `{"id":"target-vm","status":%q}`, eng.targetState)
						}
					case path == "/vms/vm":
						fmt.Fprintf(w, `{"id":"vm","name":"Source VM","status":%q,"memory":"268435456","cpu":{"architecture":"x86_64","topology":{"cores":"1","sockets":"1","threads":"1"}}}`, eng.sourceState)
					case path == "/vms/vm/nics" || path == "/vms/target-vm/nics":
						fmt.Fprint(w, `{"nic":[]}`)
					case path == "/vms/vm/diskattachments":
						fmt.Fprint(w, `{"disk_attachment":[{"id":"attachment","active":true,"bootable":true,"interface":"virtio_scsi","disk":{"id":"disk","name":"disk","provisioned_size":"65536","format":"cow","backup":"incremental","content_type":"data","status":"ok"}}]}`)
					case path == "/disks/target-disk":
						fmt.Fprint(w, `{"id":"target-disk","provisioned_size":"65536","status":"ok"}`)
					case target && path == "/vms":
						fmt.Fprint(w, `{"vm":[]}`)
					case path == "/hosts":
						fmt.Fprint(w, `{"host":[{"id":"host","name":"target","status":"up","cluster":{"id":"cluster"}}]}`)
					case path == "/hosts/host":
						fmt.Fprint(w, `{"id":"host","status":"up","cluster":{"id":"cluster"}}`)
					case path == "/clusters/cluster":
						fmt.Fprint(w, `{"id":"cluster","data_center":{"id":"dc"}}`)
					case path == "/datacenters/dc/storagedomains":
						fmt.Fprint(w, `{"storage_domain":[{"id":"domain","type":"data","status":"active","available":"1000000000"}]}`)
					case path == "/vnicprofiles":
						fmt.Fprint(w, `{"vnic_profile":[]}`)
					default:
						t.Errorf("unexpected %s %s", r.Method, path)
						w.WriteHeader(500)
					}
				})
			}
			source := httptest.NewServer(handler(false))
			defer source.Close()
			target := httptest.NewServer(handler(true))
			defer target.Close()
			for _, srv := range []*model.Server{{ID: "source", Name: "source", Kind: model.KindOVirt, EngineURL: source.URL, Enabled: true, SupportsCBT: true}, {ID: "target", Name: "target", Kind: model.KindOVirt, EngineURL: target.URL, Enabled: true}} {
				if err := st.CreateServer(ctx, srv); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.SyncVMs(ctx, "source", []*model.VM{{ID: "vm", Name: "Source VM", Status: "up", DesiredState: model.DesiredUp}}); err != nil {
				t.Fatal(err)
			}
			if err := st.SyncClusters(ctx, "target", []*model.Cluster{{ID: "cluster", Name: "cluster"}}); err != nil {
				t.Fatal(err)
			}
			if err := st.SyncStorageDomains(ctx, "target", []*model.StorageDomain{{ID: "domain", Name: "domain", Type: "data", Status: "active", AvailableSize: 1000000000}}); err != nil {
				t.Fatal(err)
			}
			storage := &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal, Enabled: true, BasePath: t.TempDir()}
			if err := st.CreateStorageTarget(ctx, storage); err != nil {
				t.Fatal(err)
			}
			var err error
			eng.storage = storage
			eng.backend, err = repo.Open(ctx, storage)
			if err != nil {
				t.Fatal(err)
			}
			defer eng.backend.Close()
			pool := ovirt.NewPool(st.GetServer, time.Second, zerolog.Nop())
			defer pool.Close()
			s := New(ctx, st, pool, eng, 1, zerolog.Nop())
			req := model.VMTransferRequest{SourceServerID: "source", SourceVMID: "vm", TargetServerID: "target", ClusterID: "cluster", StorageDomainID: "domain", StorageTargetID: "repo", Name: "Moved VM", Mode: mode, Confirm: true}
			tr := &model.VMTransfer{ID: "move", Request: req, Status: "queued", OriginalDesired: model.DesiredUp, DiskMap: map[string]string{}}
			if err = st.CreateVMTransfer(ctx, tr); err != nil {
				t.Fatal(err)
			}
			if err = s.prepare(ctx, tr); err != nil {
				t.Fatal(err)
			}
			if mode == "warm" {
				if tr.Status != "ready" || eng.sourceState != "up" || eng.targetState != "down" || eng.deltaCalls != 1 {
					t.Fatalf("warm preparation switched too early: %+v", tr)
				}
				if err = s.cutover(ctx, tr); err != nil {
					t.Fatal(err)
				}
				if eng.deltaCalls != 2 || len(eng.backupStates) != 3 || eng.backupStates[0] != "up" || eng.backupStates[1] != "up" || eng.backupStates[2] != "down" {
					t.Fatal("incorrect final delta sequence", eng.backupStates)
				}
			} else if len(eng.backupStates) != 1 || eng.backupStates[0] != "down" {
				t.Fatal("cold copy started before shutdown", eng.backupStates)
			}
			if tr.Status != "awaiting_confirmation" || eng.sourceState != "down" || eng.targetState != "up" {
				t.Fatal("incorrect cutover result", tr)
			}
			if tr, err = s.Action(ctx, tr.ID, "complete", true); err != nil {
				t.Fatal(err)
			}
			if err = s.rollback(ctx, tr, "rollback"); err != nil {
				t.Fatal(err)
			}
			if !eng.targetGone || eng.sourceState != "up" {
				t.Fatal("rollback did not restore original workload")
			}
		})
	}
}
