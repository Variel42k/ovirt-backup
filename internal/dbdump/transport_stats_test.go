package dbdump

import (
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestParseStats(t *testing.T) {
	got, err := ParseStats("jhvirt-db-stats/1 postgresql 120 3 7 987654\n", model.DBEnginePostgreSQL)
	if err != nil {
		t.Fatalf("ParseStats: %v", err)
	}
	if got.Commits != 120 || got.Rollbacks != 3 || got.Active != 7 || got.LogBytes != 987654 {
		t.Fatalf("unexpected counters: %+v", got)
	}
}

func TestParseStatsRejectsUnexpectedOutput(t *testing.T) {
	cases := []string{
		"jhvirt-db-stats/1 mysql 1 2 3 4 trailing",
		"jhvirt-db-stats/1 mysql 1 -2 3 4",
		"jhvirt-db-stats/1 postgresql 1 2 3 4",
		"debug message\njhvirt-db-stats/1 mysql 1 2 3 4",
	}
	for _, input := range cases {
		if _, err := ParseStats(input, model.DBEngineMySQL); err == nil {
			t.Errorf("accepted invalid response %q", input)
		}
	}
}
