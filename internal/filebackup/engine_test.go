package filebackup

import (
	"path/filepath"
	"testing"
)

func TestSafePathsRejectTraversal(t *testing.T) {
	root := t.TempDir()
	if _, err := safeSourcePath(root, "../outside"); err == nil {
		t.Fatal("safeSourcePath accepted traversal")
	}
	if _, err := safeRestorePath(root, "../outside"); err == nil {
		t.Fatal("safeRestorePath accepted traversal")
	}
	if got, err := safeRestorePath(root, "nested/file"); err != nil || got != filepath.Join(root, "nested", "file") {
		t.Fatalf("safe relative restore path: got %q, err %v", got, err)
	}
}

func TestExcludeGlobs(t *testing.T) {
	tests := []struct {
		path  string
		globs []string
		want  bool
	}{
		{"cache/object.bin", []string{"cache/**"}, true},
		{"logs/debug.tmp", []string{"**/*.tmp"}, true},
		{"debug.tmp", []string{"**/*.tmp"}, true},
		{"data/a.txt", []string{"data/?.txt"}, true},
		{"data/ab.txt", []string{"data/?.txt"}, false},
	}
	for _, tc := range tests {
		if got := excluded(tc.path, tc.globs); got != tc.want {
			t.Errorf("excluded(%q, %v) = %v, want %v", tc.path, tc.globs, got, tc.want)
		}
	}
}

func TestSelectedEntriesIncludesDirectoryChildren(t *testing.T) {
	entries := []Entry{{Path: "etc", Type: "directory"}, {Path: "etc/app.conf", Type: "file"}, {Path: "var/log", Type: "directory"}}
	selected := selectedEntries(entries, []string{"etc"})
	if len(selected) != 2 || selected[0].Path != "etc" || selected[1].Path != "etc/app.conf" {
		t.Fatalf("unexpected selection: %#v", selected)
	}
}
