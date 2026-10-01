package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestFinishVerifyDiskActionCancelsBeforeDelete(t *testing.T) {
	for _, rejectCancel := range []bool{false, true} {
		name := "success"
		if rejectCancel {
			name = "cancel-rejected"
		}
		t.Run(name, func(t *testing.T) {
			var canceled atomic.Bool
			var deleted atomic.Bool
			var confirmedClosed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/sso/oauth/token"):
					_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
				case r.URL.Path == "/ovirt-engine/api/imagetransfers" && r.Method == http.MethodGet:
					_, _ = w.Write([]byte(`{"image_transfer":[{"id":"transfer-1","image":{"id":"disk-1"},"direction":"upload","phase":"transferring"}]}`))
				case r.URL.Path == "/ovirt-engine/api/imagetransfers/transfer-1/cancel":
					if rejectCancel {
						w.WriteHeader(500)
						_, _ = w.Write([]byte(`{"detail":"host unavailable"}`))
						return
					}
					canceled.Store(true)
					w.WriteHeader(202)
				case r.URL.Path == "/ovirt-engine/api/imagetransfers/transfer-1":
					if !canceled.Load() {
						t.Error("проверка завершения до отмены")
					}
					confirmedClosed.Store(true)
					_, _ = w.Write([]byte(`{"id":"transfer-1","phase":"finished_failure"}`))
				case r.URL.Path == "/ovirt-engine/api/disks/disk-1":
					if r.Method == http.MethodDelete {
						if !confirmedClosed.Load() {
							t.Error("DELETE до закрытия ImageTransfer")
						}
						deleted.Store(true)
						w.WriteHeader(202)
						return
					}
					if deleted.Load() {
						w.WriteHeader(404)
						return
					}
					status := "locked"
					if confirmedClosed.Load() {
						status = "ok"
					}
					_, _ = w.Write([]byte(`{"id":"disk-1","status":"` + status + `","vms":{"vm":[]}}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client, err := ovirt.New(ovirt.Config{EngineURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			err = finishVerifyDiskAction(context.Background(), client, &model.RestoreRun{TargetDiskID: "disk-1"}, true, zerolog.Nop())
			if rejectCancel {
				if err == nil || deleted.Load() {
					t.Fatalf("удаление после отказа отмены: err=%v deleted=%v", err, deleted.Load())
				}
			} else if err != nil || !deleted.Load() {
				t.Fatalf("уборка не завершена: %v", err)
			}
		})
	}
}

func TestVerifyDiskOwnershipAndTransferSelection(t *testing.T) {
	record := &model.RestoreRun{Target: model.RestoreToNewDisk, TargetDiskID: "created-disk",
		TargetVMName: "jhv-verify-gitlab-abcdef1234", TransferID: "saved-transfer"}
	if verifyDiskRecord([]*model.RestoreRun{record}, "original-disk") != nil {
		t.Fatal("исходный диск опознан как проверочный")
	}
	existing := *record
	existing.Target = model.RestoreToDisk
	if verifyDiskRecord([]*model.RestoreRun{&existing}, "created-disk") != nil {
		t.Fatal("разрешено удаление существующего диска")
	}
	if verifyDiskRecord([]*model.RestoreRun{record}, "created-disk") != record {
		t.Fatal("не найден созданный диск")
	}
	all := []ovirt.ImageTransfer{
		{ID: "old-engine", Direction: "upload", Phase: "paused_system", Image: ovirt.Ref{ID: "created-disk"}},
		{ID: "new-engine", Direction: "upload", Phase: "transferring", Disk: ovirt.Ref{ID: "created-disk"}},
		{ID: "saved-transfer", Direction: "upload", Phase: "transferring"},
		{ID: "download", Direction: "download", Phase: "transferring", Image: ovirt.Ref{ID: "created-disk"}},
		{ID: "foreign", Direction: "upload", Phase: "transferring", Image: ovirt.Ref{ID: "original-disk"}},
		{ID: "finished", Direction: "upload", Phase: "finished_success", Image: ovirt.Ref{ID: "created-disk"}},
	}
	found := verifyDiskTransfers(all, record)
	if len(found) != 3 || found[0].ID != "old-engine" || found[1].ID != "new-engine" || found[2].ID != "saved-transfer" {
		t.Fatalf("передачи для отмены: %+v", found)
	}
}

func TestStopDiskVerificationWaitsForWriterCleanup(t *testing.T) {
	d := &Dispatcher{}
	writerCtx, cancel := context.WithCancel(context.Background())
	control := &verifyControl{cancel: cancel, done: make(chan struct{})}
	d.activeVerify.Store("abcdef12-3400-0000-0000-000000000000", control)
	ctx, stop := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer stop()
	err := d.stopDiskVerification(ctx, "jhv-verify-gitlab-abcdef1234")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("не дождались завершения работника: %v", err)
	}
	if writerCtx.Err() != context.Canceled {
		t.Fatal("запись не остановлена")
	}
	close(control.done)
	if err := d.stopDiskVerification(context.Background(), "jhv-verify-gitlab-abcdef1234"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveCanceledVerifyDiskChecksAttachmentsAndWaitsForDeletion(t *testing.T) {
	for _, attached := range []bool{false, true} {
		name := "unattached"
		if attached {
			name = "attached"
		}
		t.Run(name, func(t *testing.T) {
			var deleted atomic.Bool
			var deleteCalls atomic.Int32
			var attachmentChecks atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/sso/oauth/token") {
					_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
					return
				}
				if r.URL.Path != "/ovirt-engine/api/disks/disk-1" {
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				if r.Method == http.MethodDelete {
					deleteCalls.Add(1)
					if attachmentChecks.Load() == 0 {
						t.Error("DELETE до проверки подключений")
					}
					deleted.Store(true)
					w.WriteHeader(202)
					return
				}
				if deleted.Load() {
					w.WriteHeader(404)
					return
				}
				if r.URL.Query().Get("all_content") == "true" {
					attachmentChecks.Add(1)
					if attached {
						_, _ = w.Write([]byte(`{"id":"disk-1","status":"ok","vms":{"vm":[{"id":"production-vm"}]}}`))
						return
					}
				}
				_, _ = w.Write([]byte(`{"id":"disk-1","status":"ok","vms":{"vm":[]}}`))
			}))
			defer server.Close()
			client, err := ovirt.New(ovirt.Config{EngineURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			err = removeCanceledVerifyDisk(context.Background(), client, "disk-1", func(string) {})
			if attached {
				if err == nil || deleteCalls.Load() != 0 {
					t.Fatalf("подключённый диск удалён: calls=%d err=%v", deleteCalls.Load(), err)
				}
			} else if err != nil || deleteCalls.Load() != 1 {
				t.Fatalf("не удалён диск: calls=%d err=%v", deleteCalls.Load(), err)
			}
		})
	}
}

func TestRemoveCanceledVerifyDiskDoesNotDeleteLockedDisk(t *testing.T) {
	var deleteCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sso/oauth/token") {
			_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
			return
		}
		if r.Method == http.MethodDelete {
			deleteCalls.Add(1)
			return
		}
		_, _ = w.Write([]byte(`{"id":"disk-1","status":"locked"}`))
	}))
	defer server.Close()
	client, err := ovirt.New(ovirt.Config{EngineURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = removeCanceledVerifyDisk(ctx, client, "disk-1", func(string) {})
	if err == nil || deleteCalls.Load() != 0 {
		t.Fatalf("locked диск удаляется: calls=%d err=%v", deleteCalls.Load(), err)
	}
}
