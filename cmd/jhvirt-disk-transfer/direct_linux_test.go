package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestDirectRawResumesAndPreservesSource(t *testing.T) {
	rsync, err := exec.LookPath("rsync")
	if err != nil {
		t.Skip("rsync is required")
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img is required")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("JHVIRT_TEST_RSYNC", rsync)
	bin := t.TempDir()
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	for name, script := range map[string]string{
		"ssh": `#!/bin/sh
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o|-S|-l) shift 2 ;;
        --) shift; break ;;
        *) break ;;
    esac
done
shift
if [ "$#" = 1 ]; then exec /bin/sh -c "$1"; fi
shift
exec "$JHVIRT_TEST_RSYNC" "$@"
`,
		"rsync": `#!/bin/sh
if [ -n "$JHVIRT_TEST_ERRORS" ]; then
    attempts=0
    if [ -e "$JHVIRT_TEST_ERRORS" ]; then read attempts < "$JHVIRT_TEST_ERRORS"; fi
    if [ "$attempts" -lt 2 ]; then
        echo "$((attempts + 1))" > "$JHVIRT_TEST_ERRORS"
        echo 'Connection reset by peer' >&2
        exit 10
    fi
fi
if [ -n "$JHVIRT_TEST_THROTTLE" ]; then set -- --bwlimit=768 "$@"; fi
exec "$JHVIRT_TEST_RSYNC" --stats "$@"
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	root, imageDir := fixture(t)
	data := make([]byte, 4<<20)
	rand.Read(data)
	writeLayer(t, imageDir, testBase, "RAW", "LEAF", noParent, data)
	source, err := locateDisk(root, candidate{inspect: ovirt.Disk{ID: testDisk}})
	if err != nil {
		t.Fatal(err)
	}

	const vmID = "10000000-0000-4000-8000-000000000005"
	var running atomic.Bool
	api := http.NewServeMux()
	api.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"fixture","exp":"9999999999999"}`)
	})
	api.HandleFunc("/ovirt-engine/api/disks/"+testDisk, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":%q,"status":"ok","image_id":%q,"storage_domains":{"storage_domain":[{"id":%q}]},"vms":{"vm":[{"id":%q}]}}`,
			testDisk, testBase, testDomain, vmID)
	})
	api.HandleFunc("/ovirt-engine/api/vms/"+vmID, func(w http.ResponseWriter, r *http.Request) {
		status := "down"
		if running.Load() {
			status = "up"
		}
		fmt.Fprintf(w, `{"id":%q,"name":"test VM","status":%q}`, vmID, status)
	})
	engine := httptest.NewTLSServer(api)
	defer engine.Close()
	client, err := ovirt.New(ovirt.Config{EngineURL: engine.URL, Username: "fixture@internal", Password: "fixture",
		CACert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: engine.Certificate().Raw}))})
	if err != nil {
		t.Fatal(err)
	}

	mounts, err := hostMounts()
	if err != nil {
		t.Fatal(err)
	}
	image, err := describeDirectImage(t.Context(), source, mounts)
	if err != nil {
		t.Fatal(err)
	}
	recipient := t.TempDir()
	dest, err := destinationFromInput("ops@fixture", recipient)
	if err != nil {
		t.Fatal(err)
	}
	dest.mount = coveringMount(recipient, mounts)
	job, err := os.MkdirTemp(t.TempDir(), "ovirt-transfer-")
	if err != nil {
		t.Fatal(err)
	}
	task := newDeliveryJob(dest)
	task.Engine, task.Files = client.BaseURL(), []deliveryFile{image}
	if err := writeDelivery(job, task); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(recipient, "ovirt-transfer-"+task.ID)
	ui := console{output: io.Discard}

	t.Setenv("JHVIRT_TEST_THROTTLE", "1")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runSavedDelivery(ctx, ui, job, dest, client, false) }()
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
transfer:
	for {
		select {
		case err := <-result:
			t.Fatalf("transfer exited before interruption: %v", err)
		case <-deadline.C:
			t.Fatal("timed out waiting for first data")
		case <-tick.C:
			names, _ := filepath.Glob(filepath.Join(remote+".part", ".*"))
			for _, name := range names {
				if stat, err := os.Stat(name); err == nil && stat.Mode().IsRegular() && stat.Size() > 128<<10 {
					if saved, ok := inspectSavedJob(job); !ok || !saved.active {
						t.Fatal("running job was not marked active")
					}
					cancel()
					break transfer
				}
			}
		}
	}
	if err := <-result; err == nil {
		t.Fatal("interrupted transfer unexpectedly succeeded")
	}
	if partial, err := os.ReadDir(filepath.Join(remote+".part", ".rsync-partial")); err != nil || len(partial) != 1 {
		t.Fatalf("missing partial data: %v, %v", partial, err)
	}
	jobs, err := discoverSavedJobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, saved := range jobs {
		if saved.directory == job {
			if !saved.tracked || saved.active {
				t.Fatalf("interrupted job was not eligible for startup resume: %+v", saved)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("interrupted job was missing from the job list")
	}

	t.Setenv("JHVIRT_TEST_THROTTLE", "")
	running.Store(true)
	if err := runSavedDelivery(t.Context(), ui, job, dest, client, false); err == nil || !strings.Contains(err.Error(), "не выключена") {
		t.Fatalf("resume accepted a running VM: %v", err)
	}
	running.Store(false)
	t.Setenv("JHVIRT_TEST_ERRORS", filepath.Join(t.TempDir(), "retries"))
	delays := deliveryRetryDelays
	deliveryRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { deliveryRetryDelays = delays })
	var log bytes.Buffer
	ui.output = &log
	if err := runSavedDelivery(t.Context(), ui, job, dest, client, false); err != nil {
		t.Fatalf("resume failed: %v\n%s", err, &log)
	}
	if !strings.Contains(log.String(), "Matched data:") || strings.Contains(log.String(), "Matched data: 0 bytes") {
		t.Fatalf("resume did not reuse partial blocks:\n%s", &log)
	}
	if strings.Count(log.String(), "Возобновление ") != 2 {
		t.Fatalf("expected two automatic retries:\n%s", &log)
	}
	jobs, err = discoverSavedJobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, saved := range jobs {
		if saved.directory == job {
			t.Fatal("completed transfer still appeared in the pending list")
		}
	}
	for _, path := range []string{source.chain[0], filepath.Join(remote, image.Name)} {
		if contents, err := os.ReadFile(path); err != nil || !bytes.Equal(contents, data) {
			t.Fatalf("image %s was changed or missing: %v", path, err)
		}
	}
	if files, err := os.ReadDir(job); err != nil || len(files) != 1 || files[0].Name() != deliveryManifest {
		t.Fatalf("expected only the saved job, got %v: %v", files, err)
	}
	if err := os.WriteFile(source.chain[0], make([]byte, len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source.chain[0], image.ModTime, image.ModTime); err != nil {
		t.Fatal(err)
	}
	if err := verifyDirectImage(t.Context(), client, image); err == nil || !strings.Contains(err.Error(), "изменился") {
		t.Fatalf("resume accepted a modified RAW with the original mtime: %v", err)
	}
}
