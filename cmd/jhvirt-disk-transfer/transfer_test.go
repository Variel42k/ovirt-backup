package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

const testDomain = "10000000-0000-4000-8000-000000000001"
const testDisk = "10000000-0000-4000-8000-000000000002"
const testBase = "10000000-0000-4000-8000-000000000003"
const testLeaf = "10000000-0000-4000-8000-000000000004"

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root, err := realDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dom := filepath.Join(root, testDomain)
	image := filepath.Join(dom, "images", testDisk)
	for _, directory := range []string{filepath.Join(dom, "dom_md"), image} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dom, "dom_md", "metadata"), []byte("SDUUID="+testDomain+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root, image
}

func writeLayer(t *testing.T, directory, id, format, kind, parent string, data []byte) {
	t.Helper()
	path := filepath.Join(directory, id)
	if err := os.WriteFile(path+".meta", []byte("FORMAT="+format+"\nVOLTYPE="+kind+"\nLEGALITY=LEGAL\nPUUID="+parent+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if data != nil {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSelectIndexesRejectsDuplicatesAndOutOfRange(t *testing.T) {
	indexes, err := selectIndexes("3, 1", 3)
	if err != nil || len(indexes) != 2 || indexes[0] != 2 || indexes[1] != 0 {
		t.Fatalf("indexes=%v, err=%v", indexes, err)
	}
	for _, input := range []string{"", "1,1", "0", "4", "all"} {
		if _, err := selectIndexes(input, 3); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestFindStandaloneRawByDiskNotDomainUUID(t *testing.T) {
	root, image := fixture(t)
	writeLayer(t, image, testBase, "RAW", "LEAF", noParent, []byte("raw contents"))
	if _, err := locateDisk(root, candidate{inspect: ovirt.Disk{ID: testDomain}}); err == nil {
		t.Fatal("accepted domain UUID as a disk")
	}
	source, err := locateDisk(root, candidate{inspect: ovirt.Disk{ID: testDisk}})
	if err != nil || source.format != "raw" || len(source.chain) != 1 || source.chain[0] != filepath.Join(image, testBase) {
		t.Fatalf("source=%+v, err=%v", source, err)
	}
}

func TestDryRunSelectsRawWithoutCreatingOutput(t *testing.T) {
	root, image := fixture(t)
	writeLayer(t, image, testBase, "RAW", "LEAF", noParent, []byte("raw contents"))
	stage := t.TempDir()
	input := fmt.Sprintf("3\n%s\n%s\n1\n%s\n1\n", testDisk, root, stage)
	var logs bytes.Buffer
	ui := console{input: bufio.NewReader(strings.NewReader(input)), output: &logs}
	noMounts := func() ([]mountChoice, error) { return nil, nil }
	if err := run(t.Context(), ui, true, noMounts); err != nil {
		t.Fatalf("dry run failed: %v: %s", err, logs.String())
	}
	contents, err := os.ReadDir(stage)
	if err != nil || len(contents) != 0 || !strings.Contains(logs.String(), testBase) {
		t.Fatalf("unexpected dry run: entries=%d, err=%v, logs=%s", len(contents), err, logs.String())
	}
}

func TestEngineSelectionReturnsSeveralDisks(t *testing.T) {
	const vmID = "10000000-0000-4000-8000-000000000005"
	const otherDisk = "10000000-0000-4000-8000-000000000006"
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"access_token":"fixture","exp":"9999999999999"}`)
	})
	mux.HandleFunc("/ovirt-engine/api/vms", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("search") != "name=*Example*" {
			t.Error("wrong VM search")
		}
		fmt.Fprintf(writer, `{"vm":[{"id":%q,"name":"Example VM","status":"down"}]}`, vmID)
	})
	mux.HandleFunc("/ovirt-engine/api/vms/"+vmID+"/diskattachments", func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(writer, `{"disk_attachment":[{"disk":{"id":%q,"alias":"OS"}},{"disk":{"id":%q,"alias":"data"}}]}`, testDisk, otherDisk)
	})
	mux.HandleFunc("/ovirt-engine/api/disks/", func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprintf(writer, `{"id":%q,"status":"ok","image_id":%q}`, filepath.Base(request.URL.Path), testBase)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := ovirt.New(ovirt.Config{EngineURL: server.URL, Username: "admin@internal", Password: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	ui := console{input: bufio.NewReader(strings.NewReader("Example\n1\n2,1\n")), output: io.Discard}
	chosen, err := selectInventory(context.Background(), ui, client, true)
	if err != nil || len(chosen) != 2 || chosen[0].inspect.ID != otherDisk || chosen[1].inspect.ID != testDisk || chosen[1].vm != vmID {
		t.Fatalf("unexpected disk selection %+v, err=%v", chosen, err)
	}
}

func TestChainRequiresAndChecksConfirmedLeaf(t *testing.T) {
	root, image := fixture(t)
	writeLayer(t, image, testBase, "RAW", "INTERNAL", noParent, []byte("base"))
	writeLayer(t, image, testLeaf, "COW", "LEAF", testBase, []byte("top"))
	var ambiguous *leafRequired
	if _, err := locateDisk(root, candidate{inspect: ovirt.Disk{ID: testDisk}}); !errors.As(err, &ambiguous) || len(ambiguous.leaves) != 1 {
		t.Fatalf("expected confirmation prompt, got %v", err)
	}
	if _, err := locateDisk(root, candidate{inspect: ovirt.Disk{ID: testDisk}, leaf: testBase}); err == nil {
		t.Fatal("accepted base as a leaf")
	}
	source, err := locateDisk(root, candidate{inspect: ovirt.Disk{ID: testDisk}, leaf: testLeaf})
	if err != nil || source.format != "qcow2" || len(source.chain) != 2 || filepath.Base(source.chain[1]) != testBase {
		t.Fatalf("source=%+v err=%v", source, err)
	}
	writeLayer(t, image, testBase, "RAW", "INTERNAL", noParent, []byte("modified base"))
	if err := source.unchanged(); err == nil {
		t.Fatal("failed to detect changed source")
	}
	writeLayer(t, image, testLeaf, "COW", "LEAF", testLeaf, nil)
	if _, err := locateDisk(root, candidate{inspect: ovirt.Disk{ID: testDisk}, leaf: testLeaf}); err == nil {
		t.Fatal("accepted a cyclic chain")
	}
}

func TestDestinationCannotBeManagedOrShellSyntax(t *testing.T) {
	root, image := fixture(t)
	if _, err := stagingDirectory(root); err != nil {
		t.Fatal(err)
	}
	if _, err := stagingDirectory(image); err == nil {
		t.Fatal("accepted managed local destination")
	}
	for _, directory := range []string{"/data/images/stage", "/data/../root", "/tmp/touch;id", "/data/with space"} {
		if _, err := destinationFromInput("ops@hv.example.test", directory); err == nil {
			t.Errorf("accepted destination %q", directory)
		}
	}
	if _, err := destinationFromInput("-oProxyCommand=bad@host", "/data/exports"); err == nil {
		t.Fatal("accepted SSH option as host")
	}
	if _, err := destinationFromInput("ops@hv.example.test", "/data/exports"); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareRealQCOW2(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img is not available")
	}
	root, image := fixture(t)
	data := make([]byte, 2<<20)
	copy(data, "backing disk contents")
	writeLayer(t, image, testBase, "RAW", "INTERNAL", noParent, data)
	writeLayer(t, image, testLeaf, "COW", "LEAF", testBase, nil)
	command := exec.Command("qemu-img", "create", "-f", "qcow2", "-F", "raw", "-b", testBase, testLeaf)
	command.Dir = image
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("make test overlay: %v: %s", err, output)
	}
	if _, err := exec.LookPath("qemu-io"); err == nil {
		command := exec.Command("qemu-io", "-f", "qcow2", "-c", "write -P 0x7a 65536 4096", filepath.Join(image, testLeaf))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("write overlay sector: %v: %s", err, output)
		}
	}
	source, err := locateDisk(root, candidate{inspect: ovirt.Disk{ID: testDisk}, leaf: testLeaf})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "flat.qcow2")
	var logs bytes.Buffer
	if err := prepareDisk(context.Background(), &logs, source, output, "qcow2", func() error { return nil }); err != nil {
		t.Fatalf("prepare: %v: %s", err, logs.String())
	}
	if err := source.unchanged(); err != nil {
		t.Fatal(err)
	}
	result, err := qemuInfo(context.Background(), output, "qcow2")
	if err != nil || len(result) != 1 || result[0].Backing != "" || !strings.Contains(logs.String(), "Images are identical") {
		t.Fatalf("expected verified standalone result, info=%v err=%v logs=%s", result, err, logs.String())
	}
	blocked := filepath.Join(filepath.Dir(output), "blocked.qcow2")
	err = prepareDisk(context.Background(), io.Discard, source, blocked, "qcow2", func() error { return errors.New("source no longer safe") })
	if err == nil {
		t.Fatal("published image after failed pre-publication check")
	}
	if _, err := os.Stat(blocked); !os.IsNotExist(err) {
		t.Fatal("failed preparation left a final file")
	}
}
