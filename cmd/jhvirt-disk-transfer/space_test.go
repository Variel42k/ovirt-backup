package main

import "testing"

func TestFreeSpaceResponseKeepsPathAssociation(t *testing.T) {
	paths := []string{"/data/available", "/data/unreachable", "/data/full", "/data/overflow", "/tmp"}
	results := parseFreeSpace(paths, "0 262144 4096\nunexpected line\n2 0 4096\n3 18446744073709551615 4096\n8 12 4096\n4 1 2")
	if got := freeSpaceLabel(results, paths[0]); got != "свободно 1.00 ГиБ" {
		t.Fatalf("available: %q", got)
	}
	if got := freeSpaceLabel(results, paths[2]); got != "свободно 0 Б" {
		t.Fatalf("full: %q", got)
	}
	for _, index := range []int{1, 3, 4} {
		if _, found := results[paths[index]]; found {
			t.Fatalf("invalid or missing result was reported for %s: %v", paths[index], results)
		}
	}
}
