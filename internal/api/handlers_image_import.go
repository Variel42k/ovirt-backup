package api

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Импорт образов дисков из подключённого хранилища в движок.

type storageFileEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type storageFilesResponse struct {
	StorageTargetID string `json:"storage_target_id"`
	Path            string `json:"path"`
	// Parent пуст в корне хранилища: выше подниматься некуда.
	Parent  *string            `json:"parent"`
	Entries []storageFileEntry `json:"entries"`
}

// handleListStorageFiles: GET /storages/{id}/files?path= — один уровень
// каталогов и файлов хранилища, чтобы образ можно было выбрать, а не набирать
// путь по памяти.
func (s *Server) handleListStorageFiles(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.GetStorageTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !target.Enabled {
		s.writeError(w, r, badRequest("хранилище %q отключено", target.Name))
		return
	}
	if !repo.ListsDirs(target.Kind) {
		s.writeError(w, r, badRequest("хранилище %q (%s) нельзя просматривать как каталоги: "+
			"укажите путь к образу вручную", target.Name, target.Kind))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storageBrowseTimeout)
	defer cancel()
	backend, err := repo.Open(ctx, target)
	if err != nil {
		s.writeError(w, r, badRequest("подключение к хранилищу %q: %v", target.Name, err))
		return
	}
	defer backend.Close()

	dir := repo.CleanDir(r.URL.Query().Get("path"))
	entries, err := repo.ListDir(ctx, backend, dir)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	response := storageFilesResponse{StorageTargetID: target.ID, Path: dir, Entries: []storageFileEntry{}}
	if dir != "" {
		parent := path.Dir(dir)
		if parent == "." {
			parent = ""
		}
		response.Parent = &parent
	}
	for _, entry := range entries {
		// Скрытые и служебные имена (в том числе пробы самой службы) не
		// показываются: выбирать среди них нечего.
		if strings.HasPrefix(entry.Name, ".") || strings.HasPrefix(entry.Name, "$") {
			continue
		}
		response.Entries = append(response.Entries, storageFileEntry{
			Name: entry.Name, Path: path.Join(dir, entry.Name), IsDir: entry.IsDir,
			Size: entry.Size, Modified: entry.Modified,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

type imageInspectRequest struct {
	StorageTargetID string `json:"storage_target_id"`
	Path            string `json:"path"`
}

// handleInspectImage: POST /image-imports/inspect — формат и размер образа до
// запуска импорта.
func (s *Server) handleInspectImage(w http.ResponseWriter, r *http.Request) {
	var req imageInspectRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storageBrowseTimeout)
	defer cancel()
	info, err := s.engine.InspectImage(ctx, req.StorageTargetID, req.Path)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleStartImageImport: POST /image-imports — создать диск (и ВМ) из образа.
func (s *Server) handleStartImageImport(w http.ResponseWriter, r *http.Request) {
	var req model.ImageImportRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	req.TriggeredBy = runtimeActor(r)
	record, err := s.engine.StartImageImport(r.Context(), req)
	if err != nil {
		s.audit(r, "image.import", model.ScopeBackup, req.Path, false, err.Error())
		// Отказ на этом шаге — всегда о запросе: не тот образ, не тот домен,
		// не хватает места. Ничего ещё не создано.
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, r, err)
			return
		}
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	detail := record.StorageTargetName + ":" + record.Path + " → " + record.ServerName + "/" + record.DomainName
	if record.CreateVM {
		detail += ", ВМ " + record.VMName
	}
	s.audit(r, "image.import", model.ScopeBackup, record.ID, true, detail)
	writeJSON(w, http.StatusAccepted, record)
}

func (s *Server) handleListImageImports(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListImageImports(r.Context(), queryInt(r, "limit", 100))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetImageImport(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.GetImageImport(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// handleCancelImageImport: POST /image-imports/{id}/cancel — остановить
// загрузку; созданные диск и ВМ импорт уберёт сам.
func (s *Server) handleCancelImageImport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.engine.CancelImageImport(id); err != nil {
		if errors.Is(err, backup.ErrImageImportNotRunning) {
			s.writeError(w, r, badRequest("импорт уже завершён или выполнялся до перезапуска службы"))
			return
		}
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "image.import.cancel", model.ScopeBackup, id, true, "")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "canceling"})
}
