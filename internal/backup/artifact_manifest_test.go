package backup

import "testing"

func TestValidateArtifactManifestBindsPublishedObject(t *testing.T) {
	valid := &DiskManifest{RunID: "run", DiskID: "vm", DiskFormat: ArtifactProxmoxVZDUMP, DataKey: "run/archive.data"}
	if err := ValidateArtifactManifest("run", "vm", ArtifactProxmoxVZDUMP, "run/archive.data", valid); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}

	tests := []struct {
		name     string
		runID    string
		diskID   string
		kind     string
		dataKey  string
		manifest *DiskManifest
	}{
		{"missing manifest", "run", "vm", ArtifactProxmoxVZDUMP, "run/archive.data", nil},
		{"other run", "other", "vm", ArtifactProxmoxVZDUMP, "run/archive.data", valid},
		{"other guest", "run", "other", ArtifactProxmoxVZDUMP, "run/archive.data", valid},
		{"other kind", "run", "vm", ArtifactQcow2, "run/archive.data", valid},
		{"other data", "run", "vm", ArtifactProxmoxVZDUMP, "run/other.data", valid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateArtifactManifest(test.runID, test.diskID, test.kind, test.dataKey, test.manifest); err == nil {
				t.Fatal("mismatched artifact accepted")
			}
		})
	}
}
