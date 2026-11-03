package model

import "testing"

func TestJobCompressionValidation(t *testing.T) {
	job := BackupJob{Name: "job", ServerID: "engine", Type: BackupFull, StorageTargetIDs: []string{"repo"}}
	for _, name := range []string{"", "none", "zstd", "gzip", "s2", "lz4", "brotli"} {
		job.Compression = name
		if err := job.Validate(); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
	}
	job.Compression = "neural"
	if err := job.Validate(); err == nil {
		t.Fatal("unknown algorithm accepted")
	}
	job.Compression, job.Type = "lz4", BackupConfig
	if err := job.Validate(); err == nil {
		t.Fatal("non-disk algorithm accepted")
	}
}
