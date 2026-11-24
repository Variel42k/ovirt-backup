package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestChooseExportMode(t *testing.T) {
	raw := sourceDisk{format: "raw", chain: []string{"base"}}
	chain := sourceDisk{format: "qcow2", chain: []string{"active", "parent"}}
	var logs bytes.Buffer
	ui := console{input: bufio.NewReader(strings.NewReader("2\n\n")), output: &logs}
	forceQCOW2, err := chooseExportMode(ui, []sourceDisk{raw, chain})
	if err != nil || forceQCOW2 || !strings.Contains(logs.String(), "Есть диск с цепочкой") {
		t.Fatalf("expected incompatible choice 2 to retry and Enter to select auto, got %t, %v\n%s", forceQCOW2, err, &logs)
	}
	if exportFormat(raw, forceQCOW2) != "raw" || exportFormat(chain, forceQCOW2) != "qcow2" {
		t.Fatal("auto mode must preserve single RAW and flatten the chain as QCOW2")
	}
	ui.input = bufio.NewReader(strings.NewReader("1\n"))
	forceQCOW2, err = chooseExportMode(ui, []sourceDisk{raw, chain})
	if err != nil || !forceQCOW2 || exportFormat(raw, forceQCOW2) != "qcow2" {
		t.Fatalf("expected choice 1 to force conversion to QCOW2, got %t, %v", forceQCOW2, err)
	}
}
