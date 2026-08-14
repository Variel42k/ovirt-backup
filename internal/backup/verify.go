package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"adveng/jh_virt/internal/imageio"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/ovirt"
	"adveng/jh_virt/internal/repo"
)

// VerifyReport is the machine-readable result stored on a verification run and
// rendered in the UI.
type VerifyReport struct {
	Mode      model.VerifyMode `json:"mode"`
	RunID     string           `json:"run_id"`
	ChainRuns []string         `json:"chain_runs"`
	Disks     []DiskReport     `json:"disks"`
	Summary   string           `json:"summary"`
	Problems  []string         `json:"problems,omitempty"`
	Duration  string           `json:"duration"`
	// Boot заполняется режимом boot: что произошло с проверочной ВМ.
	Boot *BootReport `json:"boot,omitempty"`
}

// BootReport is the outcome of a test boot.
//
// It duplicates the driver's result type instead of embedding it because the
// driver lives in a package that imports this one; a plain struct here keeps
// the dependency pointing one way and the stored JSON stable regardless of
// which driver produced it.
type BootReport struct {
	// Host — подключение, на котором поднималась ВМ.
	Host       string `json:"host"`
	DomainName string `json:"domain_name"`
	// Started — ВМ создана и запущена гипервизором.
	Started bool `json:"started"`
	// AgentReplied — гостевой агент внутри восстановленной системы ответил.
	// Без агента доказать загрузку невозможно, и отчёт говорит об этом прямо.
	AgentReplied bool     `json:"agent_replied"`
	Elapsed      string   `json:"elapsed,omitempty"`
	GuestOS      string   `json:"guest_os,omitempty"`
	Hostname     string   `json:"hostname,omitempty"`
	ImageBytes   int64    `json:"image_bytes,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

// DiskReport is the per-disk part of a verification.
type DiskReport struct {
	DiskID         string   `json:"disk_id"`
	Alias          string   `json:"alias"`
	ObjectsChecked int      `json:"objects_checked"`
	ChunksChecked  int      `json:"chunks_checked"`
	BytesChecked   int64    `json:"bytes_checked"`
	CoveragePct    int      `json:"coverage_pct"`
	OK             bool     `json:"ok"`
	Problems       []string `json:"problems,omitempty"`
	// Layout заполняется режимом structure: что за разделы и файловые
	// системы нашлись внутри собранного образа.
	Layout *ImageLayout `json:"layout,omitempty"`
}

// ExternalVerifier performs a verification mode the engine cannot run itself.
//
// The boot test is the only one: it needs a hypervisor to start a VM on, and
// that code lives in internal/kvm, which imports this package for the storage
// format. Registering a function here keeps the bookkeeping — the record, its
// progress, the stored report — in one place instead of duplicating it in
// whoever owns the hypervisor connection.
type ExternalVerifier func(ctx context.Context, req ExternalVerifyRequest) error

// ExternalVerifyRequest is everything an external mode gets to work with.
type ExternalVerifyRequest struct {
	// Set — разобранная цепочка: из неё берутся диски и читается образ.
	Set *ChainSet
	// Report заполняется обработчиком: диски, итог, найденные проблемы.
	Report *VerifyReport
	// Record можно обновлять по ходу, чтобы в интерфейсе шёл прогресс.
	Record  *model.VerifyRun
	Options model.VerifyOptions
}

// UpdateProgress publishes intermediate progress of a long external check.
func (e *Engine) UpdateProgress(ctx context.Context, record *model.VerifyRun, percent int) {
	if record == nil {
		return
	}
	record.Progress = minInt(percent, 99)
	_ = e.store.UpdateVerifyRun(ctx, record)
}

// RegisterVerifier installs a handler for a mode the engine does not implement.
func (e *Engine) RegisterVerifier(mode model.VerifyMode, fn ExternalVerifier) {
	if e.external == nil {
		e.external = map[model.VerifyMode]ExternalVerifier{}
	}
	e.external[mode] = fn
}

// Verify checks a stored backup and records the outcome.
//
// Every mode answers a different question, and the cheap ones do not imply the
// expensive ones:
//
//	quick    — объекты на месте и нужного размера
//	manifest — данные в хранилище не испорчены (пересчёт SHA-256 всех чанков)
//	chain    — цепочку можно собрать: все звенья на месте, покрытие полное
//	restore  — образ действительно собирается, байт в байт по манифестам
//	source   — сохранённое совпадает с тем, что отдавал гипервизор
//	qemu     — восстановленный образ проходит qemu-img check
//	boot     — образ поднимается как ВМ и гость отвечает (внешний обработчик)
func (e *Engine) Verify(ctx context.Context, runID string, mode model.VerifyMode, opts model.VerifyOptions) (*model.VerifyRun, error) {
	return e.VerifyCopy(ctx, runID, "", mode, opts)
}

// VerifyCopy checks a specific physical copy. An empty copyID uses the normal
// primary-then-replica selection rule.
func (e *Engine) VerifyCopy(ctx context.Context, runID, copyID string, mode model.VerifyMode, opts model.VerifyOptions) (*model.VerifyRun, error) {
	if mode == "" {
		mode = model.VerifyManifest
	}

	// Запись создаётся до ожидания очереди и со статусом «ожидает»: оператор
	// нажал кнопку и должен увидеть результат нажатия сразу, а не гадать,
	// дошло ли оно, пока впереди стоят две другие проверки.
	record := &model.VerifyRun{
		ID:        uuid.NewString(),
		RunID:     runID,
		CopyID:    copyID,
		Mode:      mode,
		Status:    model.RunPending,
		CreatedAt: time.Now().UTC(),
	}
	if err := e.store.CreateVerifyRun(ctx, record); err != nil {
		return nil, err
	}

	log := e.log.With().Str("verify", record.ID).Str("backup", runID).Str("режим", string(mode)).Logger()

	if err := e.acquireHeavy(ctx); err != nil {
		record.Status = model.RunFailed
		record.Error = "отменено в очереди: " + err.Error()
		_ = e.store.UpdateVerifyRun(context.WithoutCancel(ctx), record)
		return record, err
	}
	defer e.releaseHeavy()

	started := time.Now().UTC()
	record.StartedAt = &started
	record.Status = model.RunRunning
	_ = e.store.UpdateVerifyRun(ctx, record)
	log.Info().Msg("проверка бэкапа запущена")

	report, err := e.verify(ctx, runID, copyID, mode, opts, record)
	ended := time.Now().UTC()
	record.EndedAt = &ended
	if report != nil {
		report.Duration = ended.Sub(started).Round(time.Second).String()
		if body, mErr := json.Marshal(report); mErr == nil {
			record.Details = string(body)
		}
	}

	if err != nil {
		record.Status = model.RunFailed
		record.Error = err.Error()
		_ = e.store.UpdateVerifyRun(context.WithoutCancel(ctx), record)
		e.markRunVerified(ctx, runID, copyID, model.RunFailed)
		log.Warn().Err(err).Msg("проверка выявила проблему")
		return record, err
	}

	record.Status = model.RunSucceeded
	record.Progress = 100
	if err := e.store.UpdateVerifyRun(ctx, record); err != nil {
		log.Warn().Err(err).Msg("не удалось сохранить результат проверки")
	}
	e.markRunVerified(ctx, runID, record.CopyID, model.RunSucceeded)
	log.Info().Dur("длительность", ended.Sub(started)).Msg("проверка пройдена")
	return record, nil
}

func (e *Engine) markRunVerified(ctx context.Context, runID, copyID string, status model.RunStatus) {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	run, err := e.store.GetBackupRun(saveCtx, runID)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	if copyID != "" && status == model.RunSucceeded {
		_ = e.store.MarkBackupCopyVerified(saveCtx, copyID, now)
	}
	run.VerifyStatus = status
	run.VerifiedAt = &now
	_ = e.store.UpdateBackupRun(saveCtx, run)
}

func (e *Engine) verify(ctx context.Context, runID, copyID string, mode model.VerifyMode,
	opts model.VerifyOptions, record *model.VerifyRun) (*VerifyReport, error) {
	set, err := e.loadChainCopy(ctx, runID, copyID, copyID != "")
	if err != nil {
		return nil, err
	}
	defer set.Close()
	record.CopyID = set.Copy.ID
	_ = e.store.UpdateVerifyRun(ctx, record)

	report := &VerifyReport{Mode: mode, RunID: runID}
	for _, r := range set.Runs {
		report.ChainRuns = append(report.ChainRuns, r.ID)
	}

	switch mode {
	case model.VerifyQuick:
		err = e.verifyQuick(ctx, set, report)
	case model.VerifyManifest:
		err = e.verifyManifest(ctx, set, report, record)
	case model.VerifyChain:
		err = e.verifyChain(ctx, set, report)
	case model.VerifyRestore, model.VerifyQemu:
		err = e.verifyRestore(ctx, set, report, record, mode == model.VerifyQemu)
	case model.VerifySource:
		err = e.verifySource(ctx, set, report)
	case model.VerifyStructure:
		err = e.verifyStructure(ctx, set, report)
	default:
		fn, ok := e.external[mode]
		if !ok {
			err = fmt.Errorf("неизвестный режим проверки: %q", mode)
			break
		}
		err = fn(ctx, ExternalVerifyRequest{Set: set, Report: report, Record: record, Options: opts})
	}
	if err != nil {
		return report, err
	}
	if len(report.Problems) > 0 {
		return report, fmt.Errorf("%s", strings.Join(report.Problems, "; "))
	}
	return report, nil
}

// verifyQuick confirms every object exists with the recorded size. It costs a
// handful of metadata calls and catches the most common real-world failure:
// somebody cleaned up the bucket.
func (e *Engine) verifyQuick(ctx context.Context, set *ChainSet, report *VerifyReport) error {
	for _, diskID := range set.DiskOrder {
		dr := DiskReport{DiskID: diskID, OK: true}
		for _, m := range set.Manifests[diskID] {
			dr.Alias = m.Alias
			for _, key := range []string{m.DataKey} {
				info, err := set.Backend.Stat(ctx, key)
				if err != nil {
					dr.OK = false
					dr.Problems = append(dr.Problems, fmt.Sprintf("объект %s недоступен: %v", key, err))
					continue
				}
				dr.ObjectsChecked++
				if info.Size != m.StoredBytes {
					dr.OK = false
					dr.Problems = append(dr.Problems,
						fmt.Sprintf("размер %s: %d вместо %d", key, info.Size, m.StoredBytes))
				}
				dr.BytesChecked += info.Size
			}
		}
		if !dr.OK {
			report.Problems = append(report.Problems, dr.Problems...)
		}
		report.Disks = append(report.Disks, dr)
	}
	report.Summary = fmt.Sprintf("проверено объектов: %d", totalObjects(report))
	return nil
}

// verifyManifest recomputes the digest of every chunk in the chain.
func (e *Engine) verifyManifest(ctx context.Context, set *ChainSet, report *VerifyReport, record *model.VerifyRun) error {
	totalChunks := 0
	for _, diskID := range set.DiskOrder {
		for _, m := range set.Manifests[diskID] {
			totalChunks += m.ChunkCount()
		}
	}
	checked := 0
	lastReport := time.Now()

	for _, diskID := range set.DiskOrder {
		dr := DiskReport{DiskID: diskID, OK: true}

		for _, m := range set.Manifests[diskID] {
			dr.Alias = m.Alias

			// The blob-level digest is a single streaming read that catches
			// truncation and bit rot without decoding anything.
			if err := VerifyDataObject(ctx, set.Backend, m); err != nil {
				dr.OK = false
				dr.Problems = append(dr.Problems, err.Error())
				continue
			}
			dr.ObjectsChecked++

			reader, err := NewChainReader(set.Backend, e.cipher, []*DiskManifest{m})
			if err != nil {
				dr.OK = false
				dr.Problems = append(dr.Problems, err.Error())
				continue
			}
			for _, ch := range m.Chunks {
				if err := ctx.Err(); err != nil {
					reader.Close()
					return err
				}
				// ReadChunk decodes and compares the digest; an error here is
				// exactly the corruption we are looking for.
				if _, err := reader.ReadChunk(ctx, ch.Index); err != nil {
					dr.OK = false
					dr.Problems = append(dr.Problems, err.Error())
					if len(dr.Problems) > 20 {
						dr.Problems = append(dr.Problems, "…дальнейшие ошибки не выводятся")
						break
					}
					continue
				}
				dr.ChunksChecked++
				dr.BytesChecked += int64(ch.Length)
				checked++

				if time.Since(lastReport) > 3*time.Second {
					lastReport = time.Now()
					if totalChunks > 0 {
						record.Progress = minInt(checked*100/totalChunks, 99)
						_ = e.store.UpdateVerifyRun(ctx, record)
					}
				}
			}
			reader.Close()
		}

		if !dr.OK {
			report.Problems = append(report.Problems, dr.Problems...)
		}
		report.Disks = append(report.Disks, dr)
	}

	report.Summary = fmt.Sprintf("пересчитаны контрольные суммы %d чанков (%s)",
		checked, humanBytes(totalBytes(report)))
	return nil
}

// verifyChain confirms the chain can be assembled and that its merged extent
// map is self-consistent.
func (e *Engine) verifyChain(ctx context.Context, set *ChainSet, report *VerifyReport) error {
	for i, run := range set.Runs {
		if i == 0 {
			continue
		}
		if run.ParentRunID != set.Runs[i-1].ID {
			report.Problems = append(report.Problems,
				fmt.Sprintf("бэкап %s ссылается на родителя %s, а в цепочке перед ним %s",
					run.ID, run.ParentRunID, set.Runs[i-1].ID))
		}
		// The engine computes an increment against the parent's checkpoint;
		// a break here means the delta was taken from somewhere else.
		if run.Type.NeedsParent() && run.FromCheckpointID != "" &&
			set.Runs[i-1].ToCheckpointID != "" &&
			run.FromCheckpointID != set.Runs[i-1].ToCheckpointID {
			report.Problems = append(report.Problems,
				fmt.Sprintf("бэкап %s посчитан от checkpoint %s, а родитель закончился на %s",
					run.ID, run.FromCheckpointID, set.Runs[i-1].ToCheckpointID))
		}
	}

	for _, diskID := range set.DiskOrder {
		dr := DiskReport{DiskID: diskID, OK: true}
		reader, err := e.ReaderFor(set, diskID)
		if err != nil {
			dr.OK = false
			dr.Problems = append(dr.Problems, err.Error())
			report.Problems = append(report.Problems, err.Error())
			report.Disks = append(report.Disks, dr)
			continue
		}
		chain := set.Manifests[diskID]
		dr.Alias = chain[len(chain)-1].Alias
		dr.ObjectsChecked = len(chain)
		dr.ChunksChecked = reader.PresentChunks()

		grid := reader.GridChunks()
		if grid > 0 {
			dr.CoveragePct = int(int64(reader.PresentChunks()) * 100 / grid)
		}
		// Every object the merged map will read from must exist; a missing one
		// turns into a failed restore at the worst possible moment.
		for _, m := range chain {
			if _, err := set.Backend.Stat(ctx, m.DataKey); err != nil {
				dr.OK = false
				dr.Problems = append(dr.Problems, fmt.Sprintf("нет объекта %s: %v", m.DataKey, err))
			}
		}
		reader.Close()

		if !dr.OK {
			report.Problems = append(report.Problems, dr.Problems...)
		}
		report.Disks = append(report.Disks, dr)
	}

	report.Summary = fmt.Sprintf("цепочка из %d звеньев проверена", len(set.Runs))
	return nil
}

// verifyRestore reconstructs the image into a temporary file. It is the
// strongest local check: it exercises the exact code path a real restore uses,
// including the merge across the chain.
func (e *Engine) verifyRestore(ctx context.Context, set *ChainSet, report *VerifyReport,
	record *model.VerifyRun, runQemuCheck bool) error {

	tempDir := e.cfg.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o750); err != nil {
		return fmt.Errorf("создание каталога для пробного восстановления: %w", err)
	}
	workDir, err := os.MkdirTemp(tempDir, "jhv-verify-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			e.log.Warn().Err(err).Str("каталог", workDir).Msg("не удалён каталог пробного восстановления")
		}
	}()

	if runQemuCheck {
		if _, err := FindQemuImg(e.cfg.QemuImgPath); err != nil {
			return fmt.Errorf("режим проверки qemu недоступен: %w", err)
		}
	}

	var totalBytesAll int64
	for _, diskID := range set.DiskOrder {
		chain := set.Manifests[diskID]
		totalBytesAll += chain[len(chain)-1].VirtualSize
	}
	var doneBytes int64
	lastReport := time.Now()

	for _, diskID := range set.DiskOrder {
		dr := DiskReport{DiskID: diskID, OK: true}
		reader, err := e.ReaderFor(set, diskID)
		if err != nil {
			return err
		}
		chain := set.Manifests[diskID]
		dr.Alias = chain[len(chain)-1].Alias
		dr.ObjectsChecked = len(chain)

		rawPath := filepath.Join(workDir, repo.Segment(dr.Alias)+".raw")
		f, err := os.OpenFile(rawPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			reader.Close()
			return err
		}
		if err := f.Truncate(reader.VirtualSize()); err != nil {
			_ = f.Close()
			reader.Close()
			return fmt.Errorf("резервирование размера пробного образа: %w", err)
		}

		base := doneBytes
		err = reader.Stream(ctx, func(ctx context.Context, offset int64, data []byte, zeroLength int64) error {
			if data == nil {
				return nil
			}
			if _, err := f.WriteAt(data, offset); err != nil {
				return err
			}
			dr.ChunksChecked++
			dr.BytesChecked += int64(len(data))
			return nil
		}, func(done int64) {
			if time.Since(lastReport) < 3*time.Second {
				return
			}
			lastReport = time.Now()
			if totalBytesAll > 0 {
				record.Progress = minInt(int((base+done)*100/totalBytesAll), 99)
				_ = e.store.UpdateVerifyRun(ctx, record)
			}
		})
		doneBytes += reader.VirtualSize()
		reader.Close()

		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			dr.OK = false
			dr.Problems = append(dr.Problems, err.Error())
			report.Problems = append(report.Problems, err.Error())
			report.Disks = append(report.Disks, dr)
			continue
		}

		if runQemuCheck {
			qcowPath := filepath.Join(workDir, repo.Segment(dr.Alias)+".qcow2")
			if err := ConvertToQcow2(ctx, e.cfg.QemuImgPath, rawPath, qcowPath); err != nil {
				dr.OK = false
				dr.Problems = append(dr.Problems, err.Error())
			} else if out, err := QemuImgCheck(ctx, e.cfg.QemuImgPath, qcowPath); err != nil {
				dr.OK = false
				dr.Problems = append(dr.Problems, fmt.Sprintf("qemu-img check: %v: %s", err, out))
			}
			_ = os.Remove(qcowPath)
		}
		// Free the space before restoring the next disk: a VM with several
		// terabyte disks would otherwise need all of them at once.
		_ = os.Remove(rawPath)

		if !dr.OK {
			report.Problems = append(report.Problems, dr.Problems...)
		}
		report.Disks = append(report.Disks, dr)
	}

	report.Summary = fmt.Sprintf("образ собран целиком: %d дисков, %s данных",
		len(set.DiskOrder), humanBytes(totalBytes(report)))
	if runQemuCheck {
		report.Summary += "; qemu-img check пройден"
	}
	return nil
}

// verifyStructure reads the partition table and filesystem superblocks of each
// restored disk.
//
// This is the only check that can fail on a backup whose every checksum is
// correct. A hash proves the copy is faithful; it cannot tell a faithful copy
// of a working disk from a faithful copy of an empty one. Reading the
// structures the way a bootloader would is what closes that gap, and it costs
// a few kilobytes per disk rather than a full read.
func (e *Engine) verifyStructure(ctx context.Context, set *ChainSet, report *VerifyReport) error {
	for _, diskID := range set.DiskOrder {
		dr := DiskReport{DiskID: diskID, OK: true}
		chain := set.Manifests[diskID]
		dr.Alias = chain[len(chain)-1].Alias
		dr.ObjectsChecked = len(chain)

		reader, err := e.ReaderFor(set, diskID)
		if err != nil {
			dr.OK = false
			dr.Problems = append(dr.Problems, err.Error())
			report.Problems = append(report.Problems, err.Error())
			report.Disks = append(report.Disks, dr)
			continue
		}

		layout, err := InspectImage(reader.ReaderAt(ctx), reader.VirtualSize())
		reader.Close()
		if err != nil {
			dr.OK = false
			dr.Problems = append(dr.Problems, err.Error())
			report.Problems = append(report.Problems, err.Error())
			report.Disks = append(report.Disks, dr)
			continue
		}

		dr.Layout = layout
		dr.BytesChecked = 68 << 10 // структуры читаются с начала образа

		switch layout.Verdict {
		case VerdictEmpty:
			dr.OK = false
			problem := fmt.Sprintf("диск %s: %s", dr.Alias, layout.Summary())
			dr.Problems = append(dr.Problems, problem)
			report.Problems = append(report.Problems, problem)
		case VerdictSuspicious:
			// Not every unrecognised disk is broken: raw data volumes and
			// exotic filesystems are legitimate. Report it without failing.
			note := fmt.Sprintf("диск %s: %s", dr.Alias, layout.Summary())
			dr.Problems = append(dr.Problems, note)
			e.log.Warn().Str("диск", dr.Alias).Msg(layout.Summary())
		}

		report.Disks = append(report.Disks, dr)
	}

	var usable int
	for _, d := range report.Disks {
		if d.Layout != nil && d.Layout.Usable() {
			usable++
		}
	}
	report.Summary = fmt.Sprintf("структура распознана на %d дисках из %d", usable, len(report.Disks))
	return nil
}

// verifySource compares the digest recorded at backup time against what
// ovirt-imageio reports for the disk now.
//
// This answers "is the backup still an exact image of the live disk", which is
// only a yes for a disk that has not been written to since. On a running VM a
// mismatch is expected and does not mean the backup is bad — that is what the
// manifest and restore modes are for.
func (e *Engine) verifySource(ctx context.Context, set *ChainSet, report *VerifyReport) error {
	srv, err := e.store.GetServer(ctx, set.Leaf.ServerID)
	if err != nil {
		return err
	}
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return err
	}

	for _, diskID := range set.DiskOrder {
		chain := set.Manifests[diskID]
		leaf := chain[len(chain)-1]
		dr := DiskReport{DiskID: diskID, Alias: leaf.Alias, OK: true}

		if leaf.SourceChecksum == "" {
			dr.OK = false
			dr.Problems = append(dr.Problems,
				"контрольная сумма источника не сохранялась: включите режим проверки «сверка с исходным диском» в задании, "+
					"и она будет посчитана при следующем бэкапе")
			report.Problems = append(report.Problems, dr.Problems...)
			report.Disks = append(report.Disks, dr)
			continue
		}

		transfer, err := client.CreateTransfer(ctx, ovirt.TransferRequest{
			DiskID:            diskID,
			Direction:         "download",
			Format:            "raw",
			InactivityTimeout: e.cfg.Transfer.InactivityTimeout,
		})
		if err != nil {
			return fmt.Errorf("открытие передачи для сверки диска %s: %w", leaf.Alias, err)
		}

		func() {
			defer func() {
				closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
				defer cancel()
				_ = client.CloseTransfer(closeCtx, transfer.ID, true)
			}()

			ready, err := client.WaitTransferReady(ctx, transfer.ID, 10*time.Minute)
			if err != nil {
				dr.OK = false
				dr.Problems = append(dr.Problems, err.Error())
				return
			}
			src := imageio.New(ovirt.DataURL(ready, e.cfg.Transfer.PreferProxy), client.HTTPClient())

			sum, err := src.ChecksumOf(ctx, leaf.SourceChecksumAlgo, leaf.SourceBlockSize)
			if err != nil {
				dr.OK = false
				dr.Problems = append(dr.Problems, fmt.Sprintf("ovirt-imageio не отдал контрольную сумму: %v", err))
				return
			}
			dr.ObjectsChecked++
			if sum.Checksum != leaf.SourceChecksum {
				dr.OK = false
				dr.Problems = append(dr.Problems, fmt.Sprintf(
					"диск изменился с момента бэкапа: сейчас %s, на момент бэкапа %s "+
						"(для работающей ВМ это ожидаемо)", sum.Checksum, leaf.SourceChecksum))
			}
		}()

		if !dr.OK {
			report.Problems = append(report.Problems, dr.Problems...)
		}
		report.Disks = append(report.Disks, dr)
	}

	report.Summary = "сверка с исходными дисками выполнена"
	return nil
}

// VerifyRepositoryOnly checks a backup without consulting the database, by
// reading the run manifest from the repository. It is what makes a repository
// usable after the service's own database is lost.
func (e *Engine) VerifyRepositoryOnly(ctx context.Context, target *model.StorageTarget, runPrefix string) (*RunManifest, error) {
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return nil, err
	}
	defer backend.Close()

	rc, err := backend.Get(ctx, repo.RunManifestKey(runPrefix))
	if err != nil {
		if errors.Is(err, repo.ErrNotExist) {
			return nil, fmt.Errorf("в %s нет манифеста запуска — бэкап не был завершён", runPrefix)
		}
		return nil, err
	}
	defer rc.Close()

	var doc RunManifest
	if err := DecodeManifest(rc, &doc); err != nil {
		return nil, err
	}
	if doc.Format != FormatName {
		return nil, fmt.Errorf("чужой формат манифеста: %q", doc.Format)
	}
	return &doc, nil
}

func totalObjects(r *VerifyReport) int {
	n := 0
	for _, d := range r.Disks {
		n += d.ObjectsChecked
	}
	return n
}

func totalBytes(r *VerifyReport) int64 {
	var n int64
	for _, d := range r.Disks {
		n += d.BytesChecked
	}
	return n
}
