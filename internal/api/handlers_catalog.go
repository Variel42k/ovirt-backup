package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/store"
)

type catalogCandidate struct {
	prefix   string
	doc      *backup.RunManifest
	manifest string
	hash     string
	status   string
	details  string
}

func (s *Server) handleStartCatalogScan(w http.ResponseWriter, r *http.Request) {
	targetID := r.PathValue("id")
	if _, err := s.store.GetStorageTarget(r.Context(), targetID); err != nil {
		s.writeError(w, r, err)
		return
	}
	scan := &model.CatalogScan{StorageTargetID: targetID, Status: model.RunPending}
	if err := s.store.CreateCatalogScan(r.Context(), scan); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "storage.catalog.scan", model.ScopeBackup, targetID, true, scan.ID)
	go s.runCatalogScan(context.WithoutCancel(r.Context()), scan)
	writeJSON(w, http.StatusAccepted, scan)
}

func (s *Server) handleListCatalogScans(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListCatalogScans(r.Context(), r.PathValue("id"), queryInt(r, "limit", 50))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetCatalogScan(w http.ResponseWriter, r *http.Request) {
	scan, err := s.store.GetCatalogScan(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	entries, err := s.store.ListCatalogEntries(r.Context(), scan.ID, r.URL.Query().Get("status"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scan": scan, "entries": entries})
}

func (s *Server) runCatalogScan(parent context.Context, scan *model.CatalogScan) {
	ctx, cancel := context.WithTimeout(parent, 12*time.Hour)
	defer cancel()
	started := time.Now().UTC()
	scan.Status, scan.StartedAt = model.RunRunning, &started
	_ = s.store.UpdateCatalogScan(ctx, scan)

	if err := s.scanCatalog(ctx, scan); err != nil {
		ended := time.Now().UTC()
		scan.Status, scan.Error, scan.EndedAt = model.RunFailed, err.Error(), &ended
		_ = s.store.UpdateCatalogScan(context.WithoutCancel(ctx), scan)
		s.log.Error().Err(err).Str("scan", scan.ID).Msg("сканирование каталога не выполнено")
		return
	}
	ended := time.Now().UTC()
	scan.Status, scan.EndedAt = model.RunSucceeded, &ended
	_ = s.store.UpdateCatalogScan(context.WithoutCancel(ctx), scan)
}

func (s *Server) scanCatalog(ctx context.Context, scan *model.CatalogScan) error {
	target, err := s.store.GetStorageTarget(ctx, scan.StorageTargetID)
	if err != nil {
		return err
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return err
	}
	defer backend.Close()
	objects, err := backend.List(ctx, repo.Root+"/")
	if err != nil {
		return err
	}
	byKey := make(map[string]repo.ObjectInfo, len(objects))
	prefixes := map[string]bool{}
	for _, object := range objects {
		byKey[object.Key] = object
		base := path.Base(object.Key)
		if base == "run.json" || base == "vm-config.json" || base == "vm-config.xml" ||
			strings.HasPrefix(base, "disk-") {
			prefixes[strings.TrimSuffix(object.Key, base)] = true
		}
	}

	candidates := make([]*catalogCandidate, 0, len(prefixes))
	byRunID := map[string]*catalogCandidate{}
	for prefix := range prefixes {
		candidate := &catalogCandidate{prefix: prefix, status: "incomplete",
			details: "run.json отсутствует; точка не была опубликована"}
		if _, ok := byKey[repo.RunManifestKey(prefix)]; ok {
			s.loadCatalogCandidate(ctx, backend, byKey, candidate)
			if candidate.doc != nil && candidate.doc.RunID != "" {
				if other := byRunID[candidate.doc.RunID]; other != nil && other.hash != candidate.hash {
					candidate.status, candidate.details = "conflict", "в каталоге найден второй манифест с тем же run_id"
					other.status, other.details = "conflict", candidate.details
				} else {
					byRunID[candidate.doc.RunID] = candidate
				}
			}
		}
		candidates = append(candidates, candidate)
	}

	for _, candidate := range candidates {
		if candidate.doc == nil || candidate.status != "" && candidate.status != "importable" {
			continue
		}
		doc := candidate.doc
		if doc.ParentRunID != "" {
			parent := byRunID[doc.ParentRunID]
			parentAvailable := parent != nil && parent.status != "corrupt" && parent.status != "unsupported" &&
				parent.status != "incomplete" && parent.status != "missing_object" && parent.status != "conflict"
			if !parentAvailable {
				if copy, copyErr := s.store.GetBackupCopyForTarget(ctx, doc.ParentRunID, scan.StorageTargetID); copyErr != nil || !copy.Healthy() {
					candidate.status = "missing_parent"
					candidate.details = "родитель " + doc.ParentRunID + " отсутствует в этом хранилище"
					continue
				}
			}
		}
		existing, getErr := s.store.GetBackupRun(ctx, doc.RunID)
		switch {
		case getErr == nil && existing.ManifestSHA256 != "" && existing.ManifestSHA256 != candidate.hash:
			candidate.status, candidate.details = "conflict", "run_id уже зарегистрирован с другим манифестом"
		case getErr == nil:
			// Точка известна, но без отпечатка: так лежат копии, снятые до его
			// появления. Досчитываем — иначе при следующем разборе сверять их
			// будет не с чем, и подменённый манифест пройдёт как «уже знаем».
			if existing.ManifestSHA256 == "" {
				if filled, fillErr := s.store.SetRunManifestSHA256(ctx, doc.RunID, candidate.hash); fillErr != nil {
					s.log.Warn().Err(fillErr).Str("точка", doc.RunID).
						Msg("не удалось сохранить отпечаток манифеста")
				} else if filled {
					s.log.Info().Str("точка", doc.RunID).
						Msg("отпечаток манифеста досчитан по хранилищу")
				}
			}
			if _, copyErr := s.store.GetBackupCopyForTarget(ctx, doc.RunID, scan.StorageTargetID); copyErr == nil {
				candidate.status, candidate.details = "known", "точка и эта физическая копия уже зарегистрированы"
			} else if errors.Is(copyErr, store.ErrNotFound) {
				candidate.status, candidate.details = "additional_copy", "точка известна; будет добавлена физическая копия"
			} else {
				return copyErr
			}
		case errors.Is(getErr, store.ErrNotFound):
			candidate.status, candidate.details = "importable", "готова к импорту"
		default:
			return getErr
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].prefix < candidates[j].prefix })
	for _, candidate := range candidates {
		entry := &model.CatalogEntry{ScanID: scan.ID, RepoPath: candidate.prefix,
			Status: candidate.status, ManifestSHA256: candidate.hash, Manifest: candidate.manifest,
			Details: candidate.details}
		if candidate.doc != nil {
			entry.RunID = candidate.doc.RunID
		}
		if err := s.store.AddCatalogEntry(ctx, entry); err != nil {
			return err
		}
		scan.TotalEntries++
		if candidate.status == "importable" || candidate.status == "additional_copy" {
			scan.ImportableEntries++
		}
	}
	return s.store.UpdateCatalogScan(ctx, scan)
}

func (s *Server) loadCatalogCandidate(ctx context.Context, backend repo.Backend,
	objects map[string]repo.ObjectInfo, candidate *catalogCandidate) {
	key := repo.RunManifestKey(candidate.prefix)
	raw, err := readRepositoryObject(ctx, backend, key)
	if err != nil {
		candidate.status, candidate.details = "corrupt", err.Error()
		return
	}
	sum := sha256.Sum256(raw)
	candidate.hash = hex.EncodeToString(sum[:])
	var doc backup.RunManifest
	if err := backup.DecodeManifest(strings.NewReader(string(raw)), &doc); err != nil {
		candidate.status, candidate.details = "corrupt", "run.json не читается: "+err.Error()
		return
	}
	if doc.Format != backup.FormatName || doc.Version > backup.FormatVersion {
		candidate.status, candidate.details = "unsupported", fmt.Sprintf("формат %q версии %d не поддерживается", doc.Format, doc.Version)
		return
	}
	if doc.RunID == "" || doc.ServerID == "" || doc.VMID == "" || doc.ChainID == "" {
		candidate.status, candidate.details = "corrupt", "run.json не содержит обязательные идентификаторы"
		return
	}
	candidate.doc = &doc
	manifest, err := json.Marshal(&doc)
	if err != nil {
		candidate.status, candidate.details = "corrupt", err.Error()
		return
	}
	candidate.manifest = string(manifest)
	candidate.status, candidate.details = "importable", "готова к импорту"

	required := []string{}
	if doc.ConfigKey != "" {
		required = append(required, doc.ConfigKey)
	}
	for _, disk := range doc.Disks {
		required = append(required, disk.ManifestKey, disk.DataKey)
	}
	for _, objectKey := range required {
		if objectKey == "" {
			candidate.status, candidate.details = "missing_object", "манифест содержит пустой путь объекта"
			return
		}
		if _, ok := objects[objectKey]; !ok {
			candidate.status, candidate.details = "missing_object", "отсутствует объект "+objectKey
			return
		}
	}
	for _, disk := range doc.Disks {
		body, err := readRepositoryObject(ctx, backend, disk.ManifestKey)
		if err != nil {
			candidate.status, candidate.details = "corrupt", err.Error()
			return
		}
		var diskManifest backup.DiskManifest
		if err := backup.DecodeManifest(strings.NewReader(string(body)), &diskManifest); err != nil || diskManifest.Validate() != nil {
			candidate.status, candidate.details = "corrupt", "повреждён манифест диска "+disk.ManifestKey
			return
		}
		if disk.DataSHA256 != "" {
			hash, err := hashRepositoryObject(ctx, backend, disk.DataKey)
			if err != nil || hash != disk.DataSHA256 {
				candidate.status, candidate.details = "corrupt", "SHA-256 данных не совпал для "+disk.DataKey
				return
			}
		}
	}
}

func readRepositoryObject(ctx context.Context, backend repo.Backend, key string) ([]byte, error) {
	rc, err := backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func hashRepositoryObject(ctx context.Context, backend repo.Backend, key string) (string, error) {
	rc, err := backend.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) handleImportCatalogScan(w http.ResponseWriter, r *http.Request) {
	scan, err := s.store.GetCatalogScan(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if scan.Status != model.RunSucceeded {
		s.writeError(w, r, badRequest("сканирование ещё не завершено"))
		return
	}
	var payload struct {
		EntryIDs []string `json:"entry_ids"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(payload.EntryIDs) == 0 {
		s.writeError(w, r, badRequest("не выбрана ни одна запись каталога"))
		return
	}
	all, err := s.store.ListCatalogEntries(r.Context(), scan.ID, "")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	byID, byRun := map[string]*model.CatalogEntry{}, map[string]*model.CatalogEntry{}
	for _, entry := range all {
		byID[entry.ID], byRun[entry.RunID] = entry, entry
	}
	selected := map[string]*model.CatalogEntry{}
	var includeParents func(string) error
	includeParents = func(entryID string) error {
		entry := byID[entryID]
		if entry == nil {
			return badRequest("запись каталога %s не найдена в этом сканировании", entryID)
		}
		if entry.Status != "importable" && entry.Status != "additional_copy" && entry.Status != "known" {
			return badRequest("запись %s имеет состояние %s и не может быть импортирована", entry.RunID, entry.Status)
		}
		if selected[entry.ID] != nil {
			return nil
		}
		var doc backup.RunManifest
		if err := json.Unmarshal([]byte(entry.Manifest), &doc); err != nil {
			return badRequest("манифест %s повреждён: %v", entry.RunID, err)
		}
		if doc.ParentRunID != "" {
			if parent := byRun[doc.ParentRunID]; parent != nil {
				if err := includeParents(parent.ID); err != nil {
					return err
				}
			} else if copy, err := s.store.GetBackupCopyForTarget(r.Context(), doc.ParentRunID, scan.StorageTargetID); err != nil || !copy.Healthy() {
				return badRequest("родитель %s недоступен", doc.ParentRunID)
			}
		}
		selected[entry.ID] = entry
		return nil
	}
	for _, id := range payload.EntryIDs {
		if err := includeParents(id); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	items := make([]*model.CatalogEntry, 0, len(selected))
	for _, entry := range selected {
		items = append(items, entry)
	}
	sort.Slice(items, func(i, j int) bool {
		var left, right backup.RunManifest
		_ = json.Unmarshal([]byte(items[i].Manifest), &left)
		_ = json.Unmarshal([]byte(items[j].Manifest), &right)
		return left.ChainIndex < right.ChainIndex
	})
	for _, entry := range items {
		if err := s.importCatalogEntry(r.Context(), scan, entry); err != nil {
			s.audit(r, "storage.catalog.import", model.ScopeBackup, entry.RunID, false, err.Error())
			s.writeError(w, r, err)
			return
		}
	}
	s.audit(r, "storage.catalog.import", model.ScopeBackup, scan.StorageTargetID, true, fmt.Sprintf("%d точек", len(items)))
	writeJSON(w, http.StatusOK, map[string]any{"status": "imported", "count": len(items)})
}

func (s *Server) importCatalogEntry(ctx context.Context, scan *model.CatalogScan, entry *model.CatalogEntry) error {
	var doc backup.RunManifest
	if err := json.Unmarshal([]byte(entry.Manifest), &doc); err != nil {
		return err
	}
	started, ended := doc.CreatedAt, doc.EndedAt
	if ended.IsZero() {
		ended = started
	}
	run := &model.BackupRun{ID: doc.RunID, JobID: doc.JobID, JobName: doc.JobName,
		ServerID: doc.ServerID, VMID: doc.VMID, VMName: doc.VMName, Type: doc.Type,
		Status: model.RunSucceeded, ParentRunID: doc.ParentRunID, ChainID: doc.ChainID,
		ChainIndex: doc.ChainIndex, StorageTargetID: scan.StorageTargetID, RepoPath: entry.RepoPath,
		EngineBackupID: doc.EngineBackupID, FromCheckpointID: doc.FromCheckpointID,
		ToCheckpointID: doc.ToCheckpointID, SnapshotID: doc.SnapshotID, DiskCount: len(doc.Disks),
		LogicalBytes: doc.LogicalBytes, ReadBytes: doc.LogicalBytes, StoredBytes: doc.StoredBytes,
		Progress: 100, Encrypted: doc.Encrypted, Compression: doc.Compression,
		// Конфигурация ВМ — такой же объект хранилища, как манифест и данные.
		// Без этого признака импортированная копия считалась бы на объект
		// меньше, чем та же самая точка, зарегистрированная при бэкапе.
		ConfigStored: doc.ConfigKey != "",
		StartedAt:    &started, EndedAt: &ended, CreatedAt: started,
		ManifestSHA256: entry.ManifestSHA256, Imported: true}
	disks := make([]model.BackupDisk, 0, len(doc.Disks))
	for _, disk := range doc.Disks {
		disks = append(disks, model.BackupDisk{RunID: doc.RunID, DiskID: disk.DiskID,
			Alias: disk.Alias, Index: disk.Index, VirtualSize: disk.VirtualSize, Bootable: disk.Bootable,
			ManifestKey: disk.ManifestKey, DataKey: disk.DataKey, LogicalBytes: disk.VirtualSize,
			StoredBytes: disk.StoredBytes, ChunkCount: disk.ChunkCount, Status: model.RunSucceeded})
	}
	return s.store.ImportCatalogRun(ctx, entry.ID, run, disks)
}
