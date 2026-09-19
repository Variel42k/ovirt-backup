package dbdump

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/retention"
	"github.com/Variel42k/ovirt-backup/internal/secret"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

const (
	// FormatName отличает манифест дампов от чужих файлов в каталоге запуска.
	FormatName = "jhvirt-db-dump"
	// ManifestVersion — версия формата dumps.manifest.
	ManifestVersion = 1
	manifestName    = "dumps.manifest"

	KindDatabase = "database"
	KindGlobals  = "globals"
)

// Manifest — описание запуска, публикуется последним: без него каталог
// запуска точкой восстановления не считается.
type Manifest struct {
	Format        string          `json:"format"`
	Version       int             `json:"version"`
	RunID         string          `json:"run_id"`
	JobID         string          `json:"job_id,omitempty"`
	JobName       string          `json:"job_name,omitempty"`
	HostName      string          `json:"host_name"`
	Engine        model.DBEngine  `json:"engine"`
	ServerVersion string          `json:"server_version,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	Entries       []ManifestEntry `json:"entries"`
}

// ManifestEntry — одна база или глобальные объекты. Data — тот же
// DiskManifest, что у диска ВМ: чанки, SHA-256 открытых данных, шифрование.
type ManifestEntry struct {
	Database string               `json:"database"`
	Kind     string               `json:"kind"`
	Format   string               `json:"format"`
	Data     *backup.DiskManifest `json:"data"`
}

// Engine выполняет задания дампов.
type Engine struct {
	store  *store.Store
	cfg    config.Config
	cipher *secret.Cipher
	log    zerolog.Logger
	// dial открывает канал к хосту; в тестах подменяется поддельным хелпером.
	dial func(*model.DBHost) (Transport, error)

	mu       sync.Mutex
	running  map[string]bool
	restores []*RestoreStatus
}

// New собирает движок дампов.
func New(st *store.Store, cfg config.Config, cipher *secret.Cipher, log zerolog.Logger) *Engine {
	return &Engine{
		store: st, cfg: cfg, cipher: cipher, log: log,
		dial: func(h *model.DBHost) (Transport, error) {
			return NewSSHTransport(h, 30*time.Second)
		},
		running: map[string]bool{},
	}
}

// RecoverInterrupted закрывает запуски, оборванные прежним процессом.
func (e *Engine) RecoverInterrupted(ctx context.Context) {
	if n, err := e.store.FailInterruptedDBDumpRuns(ctx); err != nil {
		e.log.Warn().Err(err).Msg("не удалось закрыть прерванные дампы СУБД")
	} else if n > 0 {
		e.log.Warn().Int64("запусков", n).Msg("дампы СУБД, прерванные перезапуском, помечены как неудачные")
	}
}

// Probe проверяет хелпер на хосте и сохраняет, какие СУБД он видит.
func (e *Engine) Probe(ctx context.Context, hostID string) (*model.DBHost, *ProbeResult, error) {
	host, err := e.store.GetDBHost(ctx, hostID)
	if err != nil {
		return nil, nil, err
	}
	transport, err := e.dial(host)
	if err != nil {
		_ = e.store.SetDBHostProbe(ctx, host.ID, nil, err.Error())
		return host, nil, err
	}
	res, err := transport.Probe(ctx)
	if err != nil {
		_ = e.store.SetDBHostProbe(ctx, host.ID, nil, err.Error())
		return host, nil, err
	}
	_ = e.store.SetDBHostProbe(ctx, host.ID, res.Engines, probeErrors(res))
	host, err = e.store.GetDBHost(ctx, hostID)
	return host, res, err
}

func probeErrors(res *ProbeResult) string {
	if len(res.Errors) == 0 {
		return ""
	}
	parts := make([]string, 0, len(res.Errors))
	for engine, detail := range res.Errors {
		parts = append(parts, fmt.Sprintf("%s: %s", engine.Title(), detail))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// ListDatabases возвращает базы, которые хелпер готов снимать.
func (e *Engine) ListDatabases(ctx context.Context, hostID string, engine model.DBEngine) ([]string, error) {
	host, err := e.store.GetDBHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	transport, err := e.dial(host)
	if err != nil {
		return nil, err
	}
	return transport.List(ctx, engine)
}

// Start запускает задание в фоне и сразу возвращает запись о запуске.
func (e *Engine) Start(ctx context.Context, jobID string) (*model.DBDumpRun, error) {
	job, err := e.store.GetDBDumpJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if len(job.StorageTargetIDs) == 0 {
		return nil, errors.New("у задания не выбрано хранилище")
	}
	if job.Encrypt && e.cipher == nil {
		return nil, errors.New("задание требует шифрования, а ключ службы недоступен")
	}
	e.mu.Lock()
	if e.running[job.ID] {
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: задание %q уже выполняется", store.ErrConflict, job.Name)
	}
	e.running[job.ID] = true
	e.mu.Unlock()

	run := &model.DBDumpRun{
		JobID: job.ID, JobName: job.Name, HostID: job.HostID, Engine: job.Engine,
		StorageTargetID: job.StorageTargetIDs[0], Status: model.RunPending, Encrypted: job.Encrypt,
	}
	if err := e.store.CreateDBDumpRun(ctx, run); err != nil {
		e.release(job.ID)
		return nil, err
	}
	go func() {
		defer e.release(job.ID)
		background := context.WithoutCancel(ctx)
		err := e.execute(background, job, run)
		if err == nil && len(job.StorageTargetIDs) > 1 {
			e.finishCopies(background, job, run)
		}
		e.reportOutcome(background, job, run, err)
		if err == nil && job.VerifyAfter {
			_ = e.Verify(background, run.ID)
		}
	}()
	return run, nil
}

func (e *Engine) release(jobID string) {
	e.mu.Lock()
	delete(e.running, jobID)
	e.mu.Unlock()
}

// reportOutcome поднимает или снимает оповещение по итогу запуска: дамп,
// который тихо перестал сниматься, обнаруживают в день, когда он нужен.
func (e *Engine) reportOutcome(ctx context.Context, job *model.DBDumpJob, run *model.DBDumpRun, err error) {
	objectID := "db:" + job.ID
	if err == nil && run.Status == model.RunSucceeded {
		_ = e.store.ResolveAlert(ctx, "", model.ScopeBackup, objectID, model.AlertBackupFailed)
		return
	}
	message := fmt.Sprintf("дамп СУБД «%s» не выполнен полностью: %s", job.Name, run.Status)
	if err != nil {
		message = fmt.Sprintf("дамп СУБД «%s» не выполнен: %v", job.Name, err)
	}
	severity := model.SeverityWarning
	if run.Status == model.RunFailed {
		severity = model.SeverityCritical
	}
	_ = e.store.RaiseAlert(ctx, &model.Alert{
		Scope: model.ScopeBackup, ObjectID: objectID, ObjectName: job.Name,
		Kind: model.AlertBackupFailed, Severity: severity, Message: message, Details: run.Error,
	})
}

func (e *Engine) execute(ctx context.Context, job *model.DBDumpJob, run *model.DBDumpRun) error {
	started := time.Now().UTC()
	run.StartedAt, run.Status = &started, model.RunRunning
	_ = e.store.UpdateDBDumpRun(ctx, run)
	fail := func(err error) error {
		ended := time.Now().UTC()
		run.Status, run.Error, run.EndedAt = model.RunFailed, err.Error(), &ended
		_ = e.store.UpdateDBDumpRun(context.WithoutCancel(ctx), run)
		e.log.Error().Err(err).Str("задание", job.Name).Str("run", run.ID).Msg("дамп СУБД не выполнен")
		return err
	}

	host, err := e.store.GetDBHost(ctx, job.HostID)
	if err != nil {
		return fail(fmt.Errorf("хост СУБД: %w", err))
	}
	transport, err := e.dial(host)
	if err != nil {
		return fail(err)
	}
	probe, err := transport.Probe(ctx)
	if err != nil {
		return fail(err)
	}
	for _, info := range probe.Engines {
		if info.Engine == job.Engine {
			run.ServerVersion = info.Version
		}
	}
	if run.ServerVersion == "" {
		detail := probe.Errors[job.Engine]
		if detail == "" {
			detail = "клиент не установлен"
		}
		return fail(fmt.Errorf("%s на хосте %s недоступна: %s", job.Engine.Title(), host.Name, detail))
	}

	databases := job.Databases
	if len(databases) == 0 {
		if databases, err = transport.List(ctx, job.Engine); err != nil {
			return fail(fmt.Errorf("список баз: %w", err))
		}
	}
	if len(databases) == 0 && !job.IncludeGlobals {
		return fail(errors.New("на хосте нет пользовательских баз для дампа"))
	}

	target, err := e.store.GetStorageTarget(ctx, run.StorageTargetID)
	if err != nil {
		return fail(fmt.Errorf("хранилище: %w", err))
	}
	if !target.Enabled {
		return fail(fmt.Errorf("хранилище %q отключено", target.Name))
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return fail(fmt.Errorf("открытие хранилища %q: %w", target.Name, err))
	}
	defer backend.Close()

	w := e.writer(backend, job.Encrypt)
	prefix := RunPrefix(host.Name, job.Engine, run.CreatedAt, run.ID)
	// abandon убирает всё, что запуск успел записать: без манифеста эти
	// объекты точкой не являются и только занимали бы место.
	abandon := func(err error) error {
		if _, delErr := backend.DeletePrefix(context.WithoutCancel(ctx), prefix); delErr != nil {
			e.log.Warn().Err(delErr).Str("run", run.ID).Msg("объекты неудачного дампа не удалены")
		}
		return fail(err)
	}
	manifest := &Manifest{
		Format: FormatName, Version: ManifestVersion, RunID: run.ID, JobID: job.ID, JobName: job.Name,
		HostName: host.Name, Engine: job.Engine, ServerVersion: run.ServerVersion, CreatedAt: run.CreatedAt,
	}

	type item struct{ name, kind, format string }
	items := make([]item, 0, len(databases)+1)
	for _, name := range databases {
		format := "sql"
		if job.Engine == model.DBEnginePostgreSQL {
			format = "custom"
		}
		items = append(items, item{name, KindDatabase, format})
	}
	if job.IncludeGlobals && job.Engine == model.DBEnginePostgreSQL {
		items = append(items, item{"globals", KindGlobals, "sql"})
	}

	failed := 0
	for index, it := range items {
		entry := model.DBDumpEntry{Database: it.name, Kind: it.kind, Format: it.format}
		var data *backup.DiskManifest
		consume := func(r io.Reader) error {
			var writeErr error
			data, writeErr = w.write(ctx, DataKey(prefix, index, it.name), run.ID, index, it.name, r)
			return writeErr
		}
		var dumpErr error
		if it.kind == KindGlobals {
			dumpErr = transport.Globals(ctx, consume)
		} else {
			dumpErr = transport.Dump(ctx, job.Engine, it.name, consume)
		}
		if dumpErr != nil {
			// Неудача одной базы не отменяет остальные: точка с девятью базами
			// из десяти полезнее, чем никакая.
			failed++
			entry.Error = dumpErr.Error()
			if data != nil {
				_ = backend.Delete(context.WithoutCancel(ctx), data.DataKey)
			}
			e.log.Error().Err(dumpErr).Str("база", it.name).Str("run", run.ID).Msg("дамп базы не снят")
		} else {
			entry.LogicalBytes, entry.StoredBytes = data.LogicalBytes, data.StoredBytes
			run.LogicalBytes += data.LogicalBytes
			run.StoredBytes += data.StoredBytes
			manifest.Entries = append(manifest.Entries, ManifestEntry{
				Database: it.name, Kind: it.kind, Format: it.format, Data: data,
			})
		}
		run.Entries = append(run.Entries, entry)
		_ = e.store.UpdateDBDumpRun(ctx, run)
	}

	if len(manifest.Entries) == 0 {
		return abandon(fmt.Errorf("ни одна база не снята; первая ошибка: %s", run.Entries[0].Error))
	}
	body, err := backup.EncodeManifest(manifest)
	if err != nil {
		return abandon(err)
	}
	manifestKey := prefix + manifestName
	n, err := backend.Put(ctx, manifestKey, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return abandon(fmt.Errorf("запись манифеста: %w", err))
	}
	run.ManifestKey = manifestKey
	run.StoredBytes += n

	ended := time.Now().UTC()
	run.EndedAt, run.Status = &ended, model.RunSucceeded
	if failed > 0 {
		run.Status = model.RunPartial
		run.Error = fmt.Sprintf("не снято баз: %d из %d", failed, len(items))
	}
	if len(job.StorageTargetIDs) > 1 {
		run.EndedAt, run.Status = nil, model.RunWaitingCopies
	}
	if err := e.store.UpdateDBDumpRun(ctx, run); err != nil {
		return err
	}
	e.log.Info().Str("задание", job.Name).Int("баз", len(manifest.Entries)).
		Int64("записано", run.StoredBytes).Msg("дамп СУБД снят")
	return nil
}

// RunPrefix — каталог запуска в хранилище.
func RunPrefix(hostName string, engine model.DBEngine, createdAt time.Time, runID string) string {
	return fmt.Sprintf("%s/db/%s/%s/%s/%s/", repo.Root, repo.Segment(hostName), repo.Segment(string(engine)),
		createdAt.UTC().Format("2006/01/02"), repo.Segment(runID))
}

// DataKey — объект данных одной базы.
func DataKey(prefix string, index int, database string) string {
	return fmt.Sprintf("%sdb-%03d-%s.data", prefix, index, repo.Segment(database))
}

// streamWriter укладывает поток дампа в чанкованный объект. Вынесен отдельно,
// чтобы формат проверялся тестами без PostgreSQL службы и без SSH.
type streamWriter struct {
	backend     repo.Backend
	cipher      *secret.Cipher
	chunkSize   int64
	compression string
	level       int
}

func (e *Engine) writer(backend repo.Backend, encrypt bool) *streamWriter {
	w := &streamWriter{
		backend: backend, chunkSize: int64(e.cfg.Backup.ChunkSize),
		compression: e.cfg.Backup.Compression, level: e.cfg.Backup.CompressionLevel,
	}
	if encrypt {
		w.cipher = e.cipher
	}
	return w
}

func (w *streamWriter) write(ctx context.Context, dataKey, runID string, index int, name string,
	r io.Reader) (*backup.DiskManifest, error) {
	chunkSize := w.chunkSize
	if chunkSize <= 0 {
		chunkSize = backup.DefaultChunkSize
	}
	m := &backup.DiskManifest{
		RunID: runID, ChainID: runID, Type: model.BackupFull,
		DiskID: name, Alias: name, Index: index, DiskFormat: "db-dump",
	}
	dw, err := backup.NewDiskWriter(ctx, m, backup.WriterOptions{
		Backend: w.backend, DataKey: dataKey, ChunkSize: chunkSize,
		Compression: w.compression, Level: w.level, Cipher: w.cipher,
	})
	if err != nil {
		return nil, err
	}
	buf := make([]byte, chunkSize)
	var chunk int64
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			if err := dw.WriteChunk(chunk, buf[:n]); err != nil {
				dw.Abort(context.WithoutCancel(ctx), w.backend, err)
				return nil, err
			}
			chunk++
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			dw.Abort(context.WithoutCancel(ctx), w.backend, readErr)
			return nil, readErr
		}
	}
	final, err := dw.Close()
	if err != nil {
		return nil, err
	}
	// Размер потока известен только в конце: он и есть «размер диска» для
	// сборки при восстановлении.
	final.VirtualSize = final.LogicalBytes
	return final, nil
}

func (e *Engine) finishCopies(ctx context.Context, job *model.DBDumpJob, primary *model.DBDumpRun) {
	failed := 0
	for _, targetID := range job.StorageTargetIDs[1:] {
		if err := e.replicate(ctx, job, primary, targetID); err != nil {
			failed++
			e.log.Error().Err(err).Str("run", primary.ID).Str("хранилище", targetID).Msg("копия дампа не создана")
		}
	}
	ended := time.Now().UTC()
	primary.EndedAt = &ended
	switch {
	case failed > 0:
		primary.Status = model.RunPartial
		primary.Error = strings.TrimPrefix(primary.Error+fmt.Sprintf("; не создано копий: %d из %d",
			failed, len(job.StorageTargetIDs)-1), "; ")
	case len(primary.Error) > 0:
		primary.Status = model.RunPartial
	default:
		primary.Status = model.RunSucceeded
	}
	_ = e.store.UpdateDBDumpRun(ctx, primary)
}

// replicate переносит объекты запуска байт в байт — без расшифровки и
// пересжатия, — сверяет их SHA-256 и публикует манифест последним.
func (e *Engine) replicate(ctx context.Context, job *model.DBDumpJob, primary *model.DBDumpRun, targetID string) error {
	copyRun := &model.DBDumpRun{
		JobID: primary.JobID, HostID: primary.HostID, Engine: primary.Engine, StorageTargetID: targetID,
		Status: model.RunRunning, ServerVersion: primary.ServerVersion, Entries: primary.Entries,
		LogicalBytes: primary.LogicalBytes, Encrypted: primary.Encrypted, CreatedAt: primary.CreatedAt,
	}
	started := time.Now().UTC()
	copyRun.StartedAt = &started
	if err := e.store.CreateDBDumpRun(ctx, copyRun); err != nil {
		return err
	}
	fail := func(err error) error {
		ended := time.Now().UTC()
		copyRun.Status, copyRun.Error, copyRun.EndedAt = model.RunFailed, err.Error(), &ended
		_ = e.store.UpdateDBDumpRun(context.WithoutCancel(ctx), copyRun)
		return err
	}
	source, err := e.openTarget(ctx, primary.StorageTargetID)
	if err != nil {
		return fail(err)
	}
	defer source.Close()
	destination, err := e.openTarget(ctx, targetID)
	if err != nil {
		return fail(err)
	}
	defer destination.Close()

	manifest, err := readManifest(ctx, source, primary.ManifestKey)
	if err != nil {
		return fail(err)
	}
	for _, entry := range manifest.Entries {
		n, err := copyVerified(ctx, source, destination, entry.Data.DataKey, entry.Data.DataSHA256)
		if err != nil {
			return fail(fmt.Errorf("база %s: %w", entry.Database, err))
		}
		copyRun.StoredBytes += n
	}
	n, err := copyVerified(ctx, source, destination, primary.ManifestKey, "")
	if err != nil {
		return fail(err)
	}
	copyRun.StoredBytes += n
	copyRun.ManifestKey = primary.ManifestKey
	ended := time.Now().UTC()
	copyRun.Status, copyRun.EndedAt = model.RunSucceeded, &ended
	if primary.Error != "" {
		copyRun.Status, copyRun.Error = model.RunPartial, primary.Error
	}
	return e.store.UpdateDBDumpRun(ctx, copyRun)
}

func (e *Engine) openTarget(ctx context.Context, targetID string) (repo.Backend, error) {
	target, err := e.store.GetStorageTarget(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("хранилище %s: %w", targetID, err)
	}
	if !target.Enabled {
		return nil, fmt.Errorf("хранилище %q отключено", target.Name)
	}
	return repo.Open(ctx, target)
}

// copyVerified копирует объект и перечитывает назначение: реплика, которую
// никто не сверил, — это надежда, а не копия.
func copyVerified(ctx context.Context, source, destination repo.Backend, key, wantSHA string) (int64, error) {
	info, err := source.Stat(ctx, key)
	if err != nil {
		return 0, err
	}
	r, err := source.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	written, err := destination.Put(ctx, key, r, info.Size)
	_ = r.Close()
	if err != nil {
		return 0, err
	}
	if wantSHA == "" {
		return written, nil
	}
	got, err := objectSHA256(ctx, destination, key)
	if err != nil {
		return 0, err
	}
	if got != wantSHA {
		return 0, fmt.Errorf("объект %s в назначении не совпал по SHA-256", key)
	}
	return written, nil
}

func readManifest(ctx context.Context, backend repo.Backend, key string) (*Manifest, error) {
	if key == "" {
		return nil, errors.New("у запуска нет манифеста")
	}
	r, err := backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var m Manifest
	if err := backup.DecodeManifest(r, &m); err != nil {
		return nil, fmt.Errorf("манифест дампов %s: %w", key, err)
	}
	if m.Format != FormatName {
		return nil, fmt.Errorf("чужой формат манифеста: %q", m.Format)
	}
	if m.Version > ManifestVersion {
		return nil, fmt.Errorf("манифест дампов версии %d создан более новой версией программы", m.Version)
	}
	return &m, nil
}

// Manifest читает манифест запуска из его хранилища.
func (e *Engine) Manifest(ctx context.Context, runID string) (*Manifest, error) {
	run, err := e.store.GetDBDumpRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	backend, err := e.openTarget(ctx, run.StorageTargetID)
	if err != nil {
		return nil, err
	}
	defer backend.Close()
	return readManifest(ctx, backend, run.ManifestKey)
}

// RestoreRequest — что и куда восстановить.
type RestoreRequest struct {
	RunID    string `json:"run_id"`
	Database string `json:"database"`
	// NewName — имя новой базы. Существующая база не перезаписывается никогда:
	// хелпер создаёт базу и откажет, если она уже есть.
	NewName string `json:"new_name"`
}

// Restore отдаёт дамп хелперу, который создаёт новую базу и загружает его.
func (e *Engine) Restore(ctx context.Context, req RestoreRequest) error {
	plan, err := e.prepareRestore(ctx, req)
	if err != nil {
		return err
	}
	defer plan.backend.Close()
	return plan.perform(ctx, e.cipher)
}

type restorePlan struct {
	backend   repo.Backend
	transport Transport
	engine    model.DBEngine
	entry     ManifestEntry
	newName   string
}

// prepareRestore проверяет всё, что можно проверить до передачи данных:
// ошибка в имени или точке видна оператору сразу, а не в журнале через час.
func (e *Engine) prepareRestore(ctx context.Context, req RestoreRequest) (*restorePlan, error) {
	if !model.ValidDBName(req.NewName) {
		return nil, fmt.Errorf("имя новой базы %q вне допустимого алфавита", req.NewName)
	}
	run, err := e.store.GetDBDumpRun(ctx, req.RunID)
	if err != nil {
		return nil, err
	}
	if run.Status != model.RunSucceeded && run.Status != model.RunPartial {
		return nil, fmt.Errorf("запуск %s не завершён успешно (%s)", run.ID, run.Status)
	}
	backend, err := e.openTarget(ctx, run.StorageTargetID)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*restorePlan, error) {
		_ = backend.Close()
		return nil, err
	}
	manifest, err := readManifest(ctx, backend, run.ManifestKey)
	if err != nil {
		return fail(err)
	}
	plan := &restorePlan{backend: backend, engine: run.Engine, newName: req.NewName}
	found := false
	for _, entry := range manifest.Entries {
		if entry.Database == req.Database && entry.Kind == KindDatabase {
			plan.entry, found = entry, true
		}
	}
	if !found {
		return fail(fmt.Errorf("в точке нет дампа базы %q", req.Database))
	}
	host, err := e.store.GetDBHost(ctx, run.HostID)
	if err != nil {
		return fail(fmt.Errorf("хост СУБД: %w", err))
	}
	if plan.transport, err = e.dial(host); err != nil {
		return fail(err)
	}
	return plan, nil
}

func (p *restorePlan) perform(ctx context.Context, cipher *secret.Cipher) error {
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(StreamDump(ctx, p.backend, cipher, p.entry.Data, pw)) }()
	err := p.transport.Restore(ctx, p.engine, p.newName, pr)
	_ = pr.CloseWithError(err)
	return err
}

// RestoreStatus — ход восстановления для интерфейса. Хранится в памяти:
// итог каждого восстановления остаётся и в журнале аудита.
type RestoreStatus struct {
	ID        string     `json:"id"`
	RunID     string     `json:"run_id"`
	Database  string     `json:"database"`
	NewName   string     `json:"new_name"`
	Status    string     `json:"status"`
	Error     string     `json:"error,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// StartRestore проверяет запрос и восстанавливает в фоне: загрузка большой
// базы длится дольше любого HTTP-запроса. done вызывается с итогом.
func (e *Engine) StartRestore(ctx context.Context, req RestoreRequest, done func(*RestoreStatus)) (*RestoreStatus, error) {
	plan, err := e.prepareRestore(ctx, req)
	if err != nil {
		return nil, err
	}
	status := &RestoreStatus{ID: uuid.NewString(), RunID: req.RunID, Database: req.Database,
		NewName: req.NewName, Status: "running", StartedAt: time.Now().UTC()}
	e.mu.Lock()
	e.restores = append([]*RestoreStatus{status}, e.restores...)
	if len(e.restores) > 50 {
		e.restores = e.restores[:50]
	}
	e.mu.Unlock()
	go func() {
		defer plan.backend.Close()
		err := plan.perform(context.WithoutCancel(ctx), e.cipher)
		ended := time.Now().UTC()
		e.mu.Lock()
		status.EndedAt, status.Status = &ended, "succeeded"
		if err != nil {
			status.Status, status.Error = "failed", err.Error()
		}
		snapshot := *status
		e.mu.Unlock()
		if done != nil {
			done(&snapshot)
		}
	}()
	snapshot := *status
	return &snapshot, nil
}

// Restores возвращает последние восстановления, новые первыми.
func (e *Engine) Restores() []RestoreStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]RestoreStatus, 0, len(e.restores))
	for _, r := range e.restores {
		out = append(out, *r)
	}
	return out
}

// StreamDump собирает объект данных обратно в поток, сверяя SHA-256 каждого
// чанка по дороге: подменённый или испорченный чанк останавливает поток до
// того, как СУБД получит испорченные данные.
func StreamDump(ctx context.Context, backend repo.Backend, cipher *secret.Cipher, data *backup.DiskManifest,
	out io.Writer) error {
	reader, err := backup.NewChainReader(backend, cipher, []*backup.DiskManifest{data})
	if err != nil {
		return err
	}
	defer reader.Close()
	var position int64
	zeros := make([]byte, 64<<10)
	return reader.Stream(ctx, func(_ context.Context, offset int64, chunk []byte, zeroLength int64) error {
		if offset != position {
			return fmt.Errorf("разрыв в потоке дампа на смещении %d", position)
		}
		if chunk != nil {
			n, err := out.Write(chunk)
			position += int64(n)
			return err
		}
		for zeroLength > 0 {
			step := min(zeroLength, int64(len(zeros)))
			n, err := out.Write(zeros[:step])
			position += int64(n)
			zeroLength -= int64(n)
			if err != nil {
				return err
			}
		}
		return nil
	}, nil)
}

// ApplyRetention применяет правила хранения задания в каждом хранилище.
func (e *Engine) ApplyRetention(ctx context.Context) error {
	jobs, err := e.store.ListDBDumpJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		runs, err := e.store.ListDBDumpRuns(ctx, job.ID, 0)
		if err != nil {
			return err
		}
		byTarget := map[string][]*model.BackupRun{}
		for _, run := range runs {
			byTarget[run.StorageTargetID] = append(byTarget[run.StorageTargetID], &model.BackupRun{
				ID: run.ID, Type: model.BackupFull, Status: run.Status,
				StoredBytes: run.StoredBytes, CreatedAt: run.CreatedAt,
			})
		}
		for _, targetRuns := range byTarget {
			decision := retention.Apply(job.Retention, targetRuns, time.Now().UTC())
			for _, runID := range decision.Delete {
				if err := e.DeleteRun(ctx, runID); err != nil {
					e.log.Warn().Err(err).Str("run", runID).Msg("просроченный дамп СУБД не удалён")
				}
			}
		}
	}
	return nil
}

// DeleteRun удаляет данные запуска и запись о нём.
func (e *Engine) DeleteRun(ctx context.Context, runID string) error {
	run, err := e.store.GetDBDumpRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status == model.RunRunning || run.Status == model.RunPending || run.Status == model.RunWaitingCopies {
		return fmt.Errorf("%w: запуск ещё выполняется", store.ErrConflict)
	}
	if run.ManifestKey != "" {
		backend, err := e.openTarget(ctx, run.StorageTargetID)
		if err != nil {
			return err
		}
		defer backend.Close()
		if slash := strings.LastIndex(run.ManifestKey, "/"); slash > 0 {
			if _, err := backend.DeletePrefix(ctx, run.ManifestKey[:slash+1]); err != nil {
				return err
			}
		}
	}
	return e.store.DeleteDBDumpRun(ctx, run.ID)
}

func objectSHA256(ctx context.Context, backend repo.Backend, key string) (string, error) {
	r, err := backend.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer r.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// verifyEntry читает дамп целиком тем же путём, что восстановление, но в
// никуда: каждый чанк расшифровывается, распаковывается и сверяется по
// SHA-256, а итоговая длина — с манифестом. Так проверяется то, что будет
// восстанавливаться, а не только то, что объект на месте.
func verifyEntry(ctx context.Context, backend repo.Backend, cipher *secret.Cipher, entry ManifestEntry) error {
	if entry.Data == nil {
		return fmt.Errorf("база %s: в манифесте нет описания данных", entry.Database)
	}
	var counted countingWriter
	if err := StreamDump(ctx, backend, cipher, entry.Data, &counted); err != nil {
		return fmt.Errorf("база %s: %w", entry.Database, err)
	}
	if counted.n != entry.Data.LogicalBytes {
		return fmt.Errorf("база %s: прочитано %d байт вместо %d", entry.Database, counted.n, entry.Data.LogicalBytes)
	}
	return nil
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// Verify перечитывает точку и записывает итог в запуск. Неудача поднимает
// оповещение «Ошибка проверки», успех — снимает его.
func (e *Engine) Verify(ctx context.Context, runID string) error {
	run, err := e.store.GetDBDumpRun(ctx, runID)
	if err != nil {
		return err
	}
	_ = e.store.SetDBDumpVerify(ctx, run.ID, model.RunRunning, "")
	verifyErr := e.verifyRun(ctx, run)
	objectID := "db:" + run.ID
	if verifyErr != nil {
		_ = e.store.SetDBDumpVerify(context.WithoutCancel(ctx), run.ID, model.RunFailed, verifyErr.Error())
		_ = e.store.RaiseAlert(context.WithoutCancel(ctx), &model.Alert{
			Scope: model.ScopeBackup, ObjectID: objectID, ObjectName: run.JobID,
			Kind: model.AlertVerifyFailed, Severity: model.SeverityCritical,
			Message: fmt.Sprintf("проверка дампа СУБД %s не пройдена: %v", run.ID, verifyErr),
		})
		e.log.Error().Err(verifyErr).Str("run", run.ID).Msg("проверка дампа СУБД не пройдена")
		return verifyErr
	}
	_ = e.store.SetDBDumpVerify(ctx, run.ID, model.RunSucceeded, "")
	_ = e.store.ResolveAlert(ctx, "", model.ScopeBackup, objectID, model.AlertVerifyFailed)
	return nil
}

func (e *Engine) verifyRun(ctx context.Context, run *model.DBDumpRun) error {
	if run.Status != model.RunSucceeded && run.Status != model.RunPartial && run.Status != model.RunWaitingCopies {
		return fmt.Errorf("запуск %s не содержит точки (%s)", run.ID, run.Status)
	}
	backend, err := e.openTarget(ctx, run.StorageTargetID)
	if err != nil {
		return err
	}
	defer backend.Close()
	manifest, err := readManifest(ctx, backend, run.ManifestKey)
	if err != nil {
		return err
	}
	var failures []string
	for _, entry := range manifest.Entries {
		if err := verifyEntry(ctx, backend, e.cipher, entry); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// StartVerify проверяет точку в фоне: чтение большого дампа дольше запроса.
func (e *Engine) StartVerify(ctx context.Context, runID string) error {
	run, err := e.store.GetDBDumpRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != model.RunSucceeded && run.Status != model.RunPartial {
		return fmt.Errorf("запуск %s не содержит завершённой точки (%s)", run.ID, run.Status)
	}
	key := "verify:" + run.ID
	e.mu.Lock()
	if e.running[key] {
		e.mu.Unlock()
		return fmt.Errorf("%w: проверка этой точки уже идёт", store.ErrConflict)
	}
	e.running[key] = true
	e.mu.Unlock()
	go func() {
		defer e.release(key)
		_ = e.Verify(context.WithoutCancel(ctx), run.ID)
	}()
	return nil
}
