package model

import "testing"

func TestValidateFreezeMountpoints(t *testing.T) {
	for _, ok := range [][]string{
		nil,
		{"/var/lib/postgresql"},
		{"/", "/data"},
		{`D:\`, "E:"},
	} {
		if err := ValidateFreezeMountpoints(ok); err != nil {
			t.Errorf("%q должен приниматься: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		{""},
		{"var/lib/postgresql"},
		{"/data\n/etc"},
		{`D:\data`},
		make([]string, MaxFreezeMountpoints+1),
	} {
		if err := ValidateFreezeMountpoints(bad); err == nil {
			t.Errorf("%q должен отвергаться", bad)
		}
	}
}

// Без заморозки список точек монтирования не нужен и не хранится.
func TestNormalizeConsistencyDropsMountpointsWithoutFreeze(t *testing.T) {
	j := BackupJob{Consistency: ConsistencyCrash, FreezeMountpoints: []string{"/data"}}
	j.NormalizeConsistency()
	if j.FreezeMountpoints != nil {
		t.Fatalf("у задания без заморозки остался список %q", j.FreezeMountpoints)
	}
}
