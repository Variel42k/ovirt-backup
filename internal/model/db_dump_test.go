package model

import (
	"encoding/json"
	"strings"
	"testing"
)

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

func TestDBHostMonitoringLinkIsAllOrNothing(t *testing.T) {
	base := DBHost{
		Name: "database", Address: "db.example.org", Port: 22, Username: "jhvirt_dump",
		PrivateKey: "key", HostKey: "ssh-ed25519 AAAA",
	}
	valid := base
	valid.ServerID, valid.VMID, valid.MonitorEngine = "server", "vm", DBEnginePostgreSQL
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid monitoring link rejected: %v", err)
	}
	for name, mutate := range map[string]func(*DBHost){
		"server only": func(host *DBHost) { host.ServerID = "server" },
		"vm only":     func(host *DBHost) { host.VMID = "vm" },
		"engine only": func(host *DBHost) { host.MonitorEngine = DBEnginePostgreSQL },
		"no engine": func(host *DBHost) {
			host.ServerID, host.VMID = "server", "vm"
		},
	} {
		t.Run(name, func(t *testing.T) {
			host := base
			mutate(&host)
			if err := host.Validate(); err == nil {
				t.Fatal("incomplete monitoring link accepted")
			}
		})
	}
}

func TestDBStatsCumulativeCountersKeepJSONPrecision(t *testing.T) {
	raw, err := json.Marshal(DBStatsSample{
		Commits: 9007199254740993, Rollbacks: 9007199254740994, LogBytes: 9007199254740995,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"commits":"9007199254740993"`,
		`"rollbacks":"9007199254740994"`,
		`"log_bytes":"9007199254740995"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("counter lost its string representation: %s", raw)
		}
	}
}
