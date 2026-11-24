package main

import (
	"bufio"
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestReadMountsAndBoundaryMatching(t *testing.T) {
	const fromLinux = `31 1 253:0 / / rw - xfs /dev/mapper/root rw
32 31 253:4 / /data/storage/source-volume rw,relatime - xfs /dev/mapper/disk rw
33 31 0:43 / /rhev/data-center/mnt/source-hv.example.test:_data_storage_source-volume ro master:7 - nfs4 source-hv.example.test:/data/storage/source-volume rw
34 31 0:44 / /mnt/disk\040staging rw - nfs4 store.example.test:/export\040staging rw
`
	mounts, err := readMounts(strings.NewReader(fromLinux))
	if err != nil || len(mounts) != 4 {
		t.Fatalf("mounts %v: %v", mounts, err)
	}
	if !mounts[2].ReadOnly || mounts[3].Path != "/mnt/disk staging" || mounts[3].Device != "store.example.test:/export staging" {
		t.Fatalf("malformed parsed mounts: %+v", mounts)
	}
	if got := coveringMount("/data/storage/source-volume/copy.img", mounts); got.ID != "32" {
		t.Fatalf("wrong volume: %+v", got)
	}
	if got := coveringMount("/data/storage/source-volume-other", mounts); got.ID != "31" {
		t.Fatalf("matched partial path component: %+v", got)
	}
}

func TestAutoDiscoverAndChooseDiskMount(t *testing.T) {
	root, image := fixture(t)
	writeLayer(t, image, testBase, "RAW", "LEAF", noParent, []byte("test data"))
	mounts := []mountChoice{{ID: "32", Path: root, Type: "xfs"}}
	choices := findLocations(mounts, testDisk, []string{testDomain})
	if len(choices) != 1 || choices[0].Directory != image || choices[0].Mount.ID != "32" {
		t.Fatalf("unexpected locations: %+v", choices)
	}
	if choices := findLocations(mounts, testDisk, []string{testBase}); len(choices) != 0 {
		t.Fatalf("matched disk in wrong domain: %+v", choices)
	}
	var logs bytes.Buffer
	ui := console{input: bufio.NewReader(strings.NewReader("1\n")), output: &logs}
	selected, err := chooseDiskRoot(ui, mounts, candidate{inspect: ovirt.Disk{ID: testDisk}})
	if err != nil || selected != filepath.Join(root, testDomain) || !strings.Contains(logs.String(), testDisk) {
		t.Fatalf("unexpected selection %s: %v\n%s", selected, err, logs.String())
	}
}

func TestPreferredDiskLocation(t *testing.T) {
	nfs := diskLocation{Domain: testDomain, Mount: mountChoice{Type: "nfs4"}}
	xfs := diskLocation{Domain: testDomain, Mount: mountChoice{Type: "xfs"}}
	if got := preferredDiskLocation([]diskLocation{nfs, xfs}); got != 1 {
		t.Fatalf("expected direct path to the shared domain, got %d", got)
	}
	if got := preferredDiskLocation([]diskLocation{xfs, xfs}); got != -1 {
		t.Fatalf("recommended one of two direct paths: %d", got)
	}
	nfs.Domain = testBase
	if got := preferredDiskLocation([]diskLocation{xfs, nfs}); got != -1 {
		t.Fatalf("recommended a path across distinct domains: %d", got)
	}
}

func TestChooseDestinationMountFiltersUnsafeAndReadOnly(t *testing.T) {
	mounts := []mountChoice{
		{ID: "1", Path: "/", Type: "xfs"},
		{ID: "2", Path: "/rhev/data-center/mnt/storage", Type: "nfs4"},
		{ID: "3", Path: "/data/readonly", Type: "xfs", ReadOnly: true},
		{ID: "4", Path: "/data/export", Type: "xfs"},
	}
	var logs bytes.Buffer
	ui := console{input: bufio.NewReader(strings.NewReader("1\n")), output: &logs}
	selected, err := chooseMountPath(ui, "Получатель", mounts, func(directory string) bool {
		_, err := destinationFromInput("ops@hv.example.test", directory)
		return err == nil
	}, func(paths []string) map[string]uint64 {
		if len(paths) != 1 || paths[0] != "/data/export" {
			t.Fatalf("unexpected paths for space probe: %v", paths)
		}
		return map[string]uint64{"/data/export": 2 << 40}
	})
	if err != nil || selected != "/data/export" || strings.Contains(logs.String(), "/rhev") ||
		strings.Contains(logs.String(), "/data/readonly") || !strings.Contains(logs.String(), "свободно 2.00 ТиБ") {
		t.Fatalf("unexpected selection %s: %v\n%s", selected, err, logs.String())
	}
}
