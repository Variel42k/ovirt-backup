package model

import "testing"

func TestValidDBName(t *testing.T) {
	good := []string{"billing", "crm_2026", "app.prod", "a-b", "_tmp", "X"}
	bad := []string{"", "..", ".hidden", "-x", "has space", "semi;colon", "$(id)", "a/b", "quote'", "back`tick",
		"x234567890123456789012345678901234567890123456789012345678901234"}
	for _, name := range good {
		if !ValidDBName(name) {
			t.Errorf("имя %q должно приниматься", name)
		}
	}
	for _, name := range bad {
		if ValidDBName(name) {
			t.Errorf("имя %q должно отвергаться", name)
		}
	}
}

func TestDBHostValidateRequiresKeyAndHostTrust(t *testing.T) {
	host := DBHost{Name: "db", Address: "db.example", Username: "jhvirt_dump", PrivateKey: "key", HostKey: "ssh-ed25519 AAAA"}
	if err := host.Validate(); err != nil {
		t.Fatalf("корректный хост отвергнут: %v", err)
	}
	noTrust := host
	noTrust.HostKey = ""
	if noTrust.Validate() == nil {
		t.Fatal("хост без ключа хоста и без явного отказа от проверки принят")
	}
	noTrust.TrustAnyHostKey = true
	if err := noTrust.Validate(); err != nil {
		t.Fatalf("явный отказ от проверки должен приниматься: %v", err)
	}
	noKey := host
	noKey.PrivateKey = ""
	if noKey.Validate() == nil {
		t.Fatal("хост без SSH-ключа принят")
	}
	noKey.PrivateKeyStored = true
	if err := noKey.Validate(); err != nil {
		t.Fatalf("сохранённый ключ должен засчитываться: %v", err)
	}
	badAddr := host
	badAddr.Address = "db.example; rm -rf"
	if badAddr.Validate() == nil {
		t.Fatal("адрес с пробелом принят")
	}
}

func TestDBDumpJobValidate(t *testing.T) {
	job := DBDumpJob{Name: "nightly", HostID: "h", Engine: DBEnginePostgreSQL, StorageTargetIDs: []string{"t"},
		Databases: []string{"billing"}, IncludeGlobals: true}
	if err := job.Validate(); err != nil {
		t.Fatalf("корректное задание отвергнуто: %v", err)
	}
	cases := map[string]func(*DBDumpJob){
		"неизвестная СУБД":         func(j *DBDumpJob) { j.Engine = "oracle" },
		"имя базы с метасимволами": func(j *DBDumpJob) { j.Databases = []string{"x;drop"} },
		"роли для MySQL":           func(j *DBDumpJob) { j.Engine = DBEngineMySQL },
		"нет хранилища":            func(j *DBDumpJob) { j.StorageTargetIDs = nil },
	}
	for name, mutate := range cases {
		j := job
		j.Databases = append([]string(nil), job.Databases...)
		mutate(&j)
		if j.Validate() == nil {
			t.Errorf("%s: задание принято", name)
		}
	}
}
