package model

import "testing"

func TestConsistencyBelow(t *testing.T) {
	cases := []struct {
		got, target Consistency
		want        bool
	}{
		{ConsistencyCrash, ConsistencyFilesystem, true},
		{ConsistencyFilesystem, ConsistencyApplication, true},
		{ConsistencyApplication, ConsistencyFilesystem, false},
		{ConsistencyFilesystem, ConsistencyFilesystem, false},
		// Точки, снятые до уровней, уровня не имеют — тревожить из-за них незачем.
		{"", ConsistencyApplication, false},
		{ConsistencyCrash, "", false},
	}
	for _, tc := range cases {
		if got := tc.got.Below(tc.target); got != tc.want {
			t.Errorf("%q.Below(%q) = %v, want %v", tc.got, tc.target, got, tc.want)
		}
	}
}

func TestNormalizeConsistency(t *testing.T) {
	cases := []struct {
		name        string
		job         BackupJob
		wantLevel   Consistency
		wantQuiesce bool
		wantRequire bool
	}{
		{"legacy job without freeze", BackupJob{}, ConsistencyCrash, false, false},
		{"legacy job with freeze", BackupJob{Quiesce: true}, ConsistencyFilesystem, true, false},
		{"application drives the flag", BackupJob{Consistency: ConsistencyApplication, RequireConsistency: true},
			ConsistencyApplication, true, true},
		{"crash wins over a stale flag", BackupJob{Consistency: ConsistencyCrash, Quiesce: true, RequireConsistency: true},
			ConsistencyCrash, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := tc.job
			job.NormalizeConsistency()
			if job.Consistency != tc.wantLevel || job.Quiesce != tc.wantQuiesce || job.RequireConsistency != tc.wantRequire {
				t.Fatalf("got level=%q quiesce=%v require=%v, want %q %v %v",
					job.Consistency, job.Quiesce, job.RequireConsistency, tc.wantLevel, tc.wantQuiesce, tc.wantRequire)
			}
		})
	}
}

func TestValidateRejectsUnknownConsistency(t *testing.T) {
	job := BackupJob{Name: "db", ServerID: "s", StorageTargetIDs: []string{"t"}, Type: BackupFull,
		VMIDs: []string{"vm"}, Consistency: "strong"}
	if err := job.Validate(); err == nil {
		t.Fatal("unknown consistency level must be rejected")
	}
	job.Consistency = ConsistencyApplication
	if err := job.Validate(); err != nil {
		t.Fatalf("application level must be accepted: %v", err)
	}
}
