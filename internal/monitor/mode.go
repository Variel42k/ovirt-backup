package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"adveng/jh_virt/internal/model"
)

// Switching auto-remediation between check and live mode, and the archive that
// closing a check period leaves behind.
//
// The archive is the point of the whole arrangement. Check mode is not a
// setting, it is an experiment: the operator turns it on to find out what the
// automation would do to production, watches for a while, and then decides. The
// decision deserves evidence, and after the switch the live records start
// mixing in — so the observations are collected at the moment the period ends,
// while their boundaries are still exact.

// ModeArchive is the document written when a check period closes.
type ModeArchive struct {
	PeriodID  string    `json:"period_id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Duration  string    `json:"duration"`
	// ClosedBy — кто вывел систему из режима проверки.
	ClosedBy string `json:"closed_by"`
	OpenedBy string `json:"opened_by"`
	Note     string `json:"note,omitempty"`
	// CloseNote — обоснование перехода в боевой режим.
	CloseNote string                    `json:"close_note,omitempty"`
	Summary   model.RemediationDigest   `json:"summary"`
	Decisions []model.RemediationRecord `json:"decisions"`
}

// SwitchMode changes the remediation mode and returns the period it closed.
//
// Closing and opening happen together: there is always exactly one period in
// force, so a decision recorded a millisecond after the switch belongs
// unambiguously to the new mode.
func (r *Remediator) SwitchMode(ctx context.Context, dryRun bool, changedBy, note string) (*model.RemediationPeriod, *model.RemediationPeriod, error) {
	current, err := r.store.CurrentRemediationPeriod(ctx, r.cfg.DryRun)
	if err != nil {
		return nil, nil, err
	}
	if current.DryRun == dryRun {
		return current, nil, fmt.Errorf("режим уже %s", modeName(dryRun))
	}

	var archivePath string
	var digest *model.RemediationDigest

	// Only a check period produces an archive: for a live period the record of
	// what happened is the remediation history itself.
	if current.DryRun {
		archivePath, digest, err = r.archivePeriod(ctx, current, changedBy, note)
		if err != nil {
			// A failed archive must not trap the operator in check mode — but it
			// must be loud, because the evidence they were collecting is gone.
			r.log.Error().Err(err).Str("период", current.ID).
				Msg("НЕ УДАЛОСЬ СОХРАНИТЬ АРХИВ РЕЖИМА ПРОВЕРКИ — режим переключён, но наблюдения не сохранены")
		}
	}

	if err := r.store.CloseRemediationPeriod(ctx, current.ID, archivePath, digest); err != nil {
		return nil, nil, err
	}
	// Reflect the close on the returned object: the caller renders it straight
	// away, and a freshly archived period that reports no archive would send
	// the operator looking for a file the interface just told them not to
	// expect.
	closedAt := time.Now().UTC()
	current.EndedAt = &closedAt
	current.ArchivePath = archivePath
	current.Summary = digest
	opened, err := r.store.OpenRemediationPeriod(ctx, dryRun, changedBy, note)
	if err != nil {
		return nil, nil, err
	}
	r.SetMode(dryRun, opened.ID)

	entry := r.log.Warn().
		Str("режим", modeName(dryRun)).
		Str("переключил", changedBy).
		Str("предыдущий период", current.ID).
		Str("длился", current.Duration().Round(time.Second).String())
	if note != "" {
		entry = entry.Str("пояснение", note)
	}
	if digest != nil {
		entry = entry.Int("решений за период", digest.Total).
			Int("подавлено", digest.Suppressed).
			Int("пропущено", digest.Skipped)
	}
	if archivePath != "" {
		entry = entry.Str("архив", archivePath)
	}
	entry.Msg(modeMessage(dryRun))

	return current, opened, nil
}

// archivePeriod collects the decisions of a period and writes them to disk.
func (r *Remediator) archivePeriod(ctx context.Context, period *model.RemediationPeriod,
	closedBy, closeNote string) (string, *model.RemediationDigest, error) {

	now := time.Now().UTC()
	records, err := r.store.RemediationsBetween(ctx, period.StartedAt, &now)
	if err != nil {
		return "", nil, fmt.Errorf("выборка решений за период: %w", err)
	}

	archive := ModeArchive{
		PeriodID: period.ID, StartedAt: period.StartedAt, EndedAt: now,
		Duration: now.Sub(period.StartedAt).Round(time.Second).String(),
		ClosedBy: closedBy, OpenedBy: period.ChangedBy, Note: period.Note, CloseNote: closeNote,
		Decisions: make([]model.RemediationRecord, 0, len(records)),
	}
	digest := model.RemediationDigest{ByAction: map[string]int{}}
	objects := map[string]bool{}

	for _, rec := range records {
		archive.Decisions = append(archive.Decisions, *rec)
		digest.Total++
		digest.ByAction[rec.Action.Title()]++
		objects[rec.ServerID+"/"+rec.ObjectID] = true

		switch rec.Status {
		case model.RemDryRun:
			digest.Suppressed++
		case model.RemSkipped:
			digest.Skipped++
		case model.RemSucceeded:
			digest.Succeeded++
		case model.RemFailed:
			digest.Failed++
		}
	}
	digest.Objects = len(objects)
	archive.Summary = digest

	path, err := r.writeArchive(period, archive)
	if err != nil {
		return "", &digest, err
	}
	return path, &digest, nil
}

// writeArchive stores the document as indented JSON.
//
// Plain JSON rather than a compressed blob: this is an audit artefact that
// somebody may have to read a year later, possibly without this service
// running, and the volume is decisions rather than disk images.
func (r *Remediator) writeArchive(period *model.RemediationPeriod, archive ModeArchive) (string, error) {
	dir := r.cfg.ArchiveDir
	if dir == "" {
		dir = filepath.Join("data", "remediation-archives")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("создание каталога архивов %s: %w", dir, err)
	}

	name := fmt.Sprintf("dry-run-%s-%s.json",
		period.StartedAt.Format("20060102-150405"), shortID(period.ID))
	path := filepath.Join(dir, name)

	body, err := json.MarshalIndent(archive, "", "  ")
	if err != nil {
		return "", err
	}
	// 0o640: an archive names the VMs and hosts of the installation, which is
	// not something to leave world-readable.
	if err := os.WriteFile(path, body, 0o640); err != nil {
		return "", fmt.Errorf("запись архива %s: %w", path, err)
	}
	return path, nil
}

// ReadArchive loads a stored archive.
func ReadArchive(path string) (*ModeArchive, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var archive ModeArchive
	if err := json.Unmarshal(body, &archive); err != nil {
		return nil, fmt.Errorf("разбор архива %s: %w", path, err)
	}
	return &archive, nil
}

// RestoreMode re-applies the stored mode at startup.
//
// The stored mode wins over the configured one: an operator who left check mode
// last month should not be put back into it by an unrelated restart, and — more
// importantly — one who is still observing should not have the automation start
// acting because the service happened to restart.
func (r *Remediator) RestoreMode(ctx context.Context) error {
	period, err := r.store.CurrentRemediationPeriod(ctx, r.cfg.DryRun)
	if err != nil {
		return err
	}
	r.SetMode(period.DryRun, period.ID)

	if period.DryRun {
		r.log.Warn().
			Str("период", period.ID).
			Time("с", period.StartedAt).
			Str("включил", period.ChangedBy).
			Msg("авто-восстановление в режиме проверки: действия только записываются, но не выполняются")
	} else {
		r.log.Info().Str("период", period.ID).Msg("авто-восстановление в боевом режиме: действия выполняются")
	}
	return nil
}

func modeName(dryRun bool) string {
	if dryRun {
		return "проверка"
	}
	return "боевой"
}

func modeMessage(dryRun bool) string {
	if dryRun {
		return "АВТО-ВОССТАНОВЛЕНИЕ ПЕРЕВЕДЕНО В РЕЖИМ ПРОВЕРКИ: действия больше не выполняются, только записываются"
	}
	return "АВТО-ВОССТАНОВЛЕНИЕ ПЕРЕВЕДЕНО В БОЕВОЙ РЕЖИМ: действия начнут выполняться по-настоящему"
}

func shortID(id string) string {
	if idx := strings.IndexByte(id, '-'); idx > 0 {
		return id[:idx]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
