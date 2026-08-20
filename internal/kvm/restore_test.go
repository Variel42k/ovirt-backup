package kvm

import (
	"strings"
	"testing"

	"adveng/jh_virt/internal/backup"
)

func TestRestoreDomainXMLDoesNotReplayMACAndEscapesValues(t *testing.T) {
	raw, err := RestoreDomainXML(RestoreDomain{
		Name:    "restored<&vm",
		Profile: &backup.VMProfile{Architecture: "x86_64", MemoryMiB: 1024, VCPUs: 1},
		Disks:   []RestoreDisk{{Path: "/pool/a&b.raw", Target: "vda", Bus: "virtio"}},
		NICs:    []RestoreNIC{{Name: "nic0", TargetKind: "network", TargetID: "isolated<&", Connected: false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"restored&lt;&amp;vm", "/pool/a&amp;b.raw", "network='isolated&lt;&amp;'", "<link state='down'/>"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("XML does not contain %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "<mac") {
		t.Fatalf("restored XML must not replay a source MAC:\n%s", raw)
	}
}

func TestRestoreDomainXMLRejectsDuplicateDiskTargets(t *testing.T) {
	_, err := RestoreDomainXML(RestoreDomain{Name: "vm", Disks: []RestoreDisk{
		{Path: "/one", Target: "vda"}, {Path: "/two", Target: "vda"},
	}})
	if err == nil {
		t.Fatal("expected duplicate target to be rejected")
	}
}
