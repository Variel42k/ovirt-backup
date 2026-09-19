package dbdump

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
)

// fakeHelper отвечает как jhvirt-db-dump, но без SSH и без СУБД.
type fakeHelper struct {
	dumps       map[string][]byte
	broken      string
	restoreName string
	restored    bytes.Buffer
}

func (f *fakeHelper) Probe(context.Context) (*ProbeResult, error) {
	return &ProbeResult{Engines: []model.DBEngineInfo{{Engine: model.DBEnginePostgreSQL, Version: "16.4"}},
		RestoreEnabled: true}, nil
}

func (f *fakeHelper) List(context.Context, model.DBEngine) ([]string, error) {
	names := []string{f.broken}
	for name := range f.dumps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (f *fakeHelper) Dump(_ context.Context, _ model.DBEngine, db string, consume func(io.Reader) error) error {
	if db == f.broken {
		// Часть потока успела уйти, затем pg_dump упал.
		_ = consume(io.MultiReader(bytes.NewReader([]byte("PGDMP partial")), errReader{}))
		return errors.New("pg_dump: error: connection to server lost")
	}
	return consume(bytes.NewReader(f.dumps[db]))
}

func (f *fakeHelper) Globals(_ context.Context, consume func(io.Reader) error) error {
	return consume(bytes.NewReader([]byte("CREATE ROLE app;\n")))
}

func (f *fakeHelper) Restore(_ context.Context, _ model.DBEngine, name string, dump io.Reader) error {
	f.restoreName = name
	_, err := io.Copy(&f.restored, dump)
	return err
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestDumpJobPartialFailureAndRestore(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	target := &model.StorageTarget{Name: "local", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatalf("хранилище: %v", err)
	}
	host := &model.DBHost{Name: "db-01", Address: "db.example", Username: "jhvirt_dump",
		PrivateKey: "not-used-by-fake", TrustAnyHostKey: true}
	if err := st.CreateDBHost(ctx, host); err != nil {
		t.Fatalf("хост: %v", err)
	}
	job := &model.DBDumpJob{Name: "nightly", Enabled: true, HostID: host.ID, Engine: model.DBEnginePostgreSQL,
		IncludeGlobals: true, StorageTargetIDs: []string{target.ID}, Encrypt: true}
	if err := st.CreateDBDumpJob(ctx, job); err != nil {
		t.Fatalf("задание: %v", err)
	}

	billing := bytes.Repeat([]byte("PGDMP billing data "), 200_000)
	fake := &fakeHelper{dumps: map[string][]byte{"billing": billing, "crm": []byte("PGDMP crm")}, broken: "audit"}
	engine := New(st, config.Config{}, testCipher(t), zerolog.Nop())
	engine.dial = func(*model.DBHost) (Transport, error) { return fake, nil }

	run := &model.DBDumpRun{JobID: job.ID, HostID: host.ID, Engine: job.Engine,
		StorageTargetID: target.ID, Encrypted: true}
	if err := st.CreateDBDumpRun(ctx, run); err != nil {
		t.Fatalf("запуск: %v", err)
	}
	if err := engine.execute(ctx, job, run); err != nil {
		t.Fatalf("дамп: %v", err)
	}
	stored, err := st.GetDBDumpRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.RunPartial || stored.ServerVersion != "16.4" || len(stored.Entries) != 4 {
		t.Fatalf("итог запуска: status=%s version=%q entries=%d", stored.Status, stored.ServerVersion, len(stored.Entries))
	}

	manifest, err := engine.Manifest(ctx, run.ID)
	if err != nil {
		t.Fatalf("манифест: %v", err)
	}
	kinds := map[string]string{}
	for _, entry := range manifest.Entries {
		kinds[entry.Database] = entry.Kind
	}
	if len(manifest.Entries) != 3 || kinds["globals"] != KindGlobals || kinds["billing"] != KindDatabase {
		t.Fatalf("в манифесте должны быть две базы и глобальные объекты, без упавшей: %v", kinds)
	}
	if _, ok := kinds["audit"]; ok {
		t.Fatal("оборванный дамп попал в манифест")
	}

	if err := engine.Verify(ctx, run.ID); err != nil {
		t.Fatalf("проверка точки: %v", err)
	}
	if verified, err := st.GetDBDumpRun(ctx, run.ID); err != nil || verified.VerifyStatus != model.RunSucceeded ||
		verified.VerifiedAt == nil {
		t.Fatalf("итог проверки не сохранён: %+v %v", verified, err)
	}

	if err := engine.Restore(ctx, RestoreRequest{RunID: run.ID, Database: "billing", NewName: "billing_restored"}); err != nil {
		t.Fatalf("восстановление: %v", err)
	}
	if fake.restoreName != "billing_restored" || !bytes.Equal(fake.restored.Bytes(), billing) {
		t.Fatalf("хелпер получил %d байт в %q, ожидалось %d в billing_restored",
			fake.restored.Len(), fake.restoreName, len(billing))
	}
	if err := engine.Restore(ctx, RestoreRequest{RunID: run.ID, Database: "billing", NewName: "bad name"}); err == nil {
		t.Fatal("имя новой базы с пробелом принято")
	}
	if err := engine.Restore(ctx, RestoreRequest{RunID: run.ID, Database: "globals", NewName: "x"}); err == nil {
		t.Fatal("глобальные объекты нельзя восстанавливать как базу")
	}

	if err := engine.DeleteRun(ctx, run.ID); err != nil {
		t.Fatalf("удаление запуска: %v", err)
	}
	if _, err := st.GetDBDumpRun(ctx, run.ID); err == nil {
		t.Fatal("запись о запуске осталась после удаления")
	}
}
