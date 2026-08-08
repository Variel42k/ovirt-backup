package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
)

// storagePayload is the write shape of a backup repository. Secrets are
// write-only: they go in through this struct and never come back out.
type storagePayload struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Enabled *bool  `json:"enabled"`

	BasePath string `json:"base_path"`

	Endpoint     string `json:"endpoint"`
	Region       string `json:"region"`
	Bucket       string `json:"bucket"`
	Prefix       string `json:"prefix"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	UseSSL       *bool  `json:"use_ssl"`
	PathStyle    bool   `json:"path_style"`
	StorageClass string `json:"storage_class"`

	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
	HostKey    string `json:"host_key"`

	RateLimit int64 `json:"rate_limit"`
}

func (p storagePayload) apply(dst *model.StorageTarget) {
	dst.Name = p.Name
	dst.Kind = model.StorageKind(p.Kind)
	dst.BasePath = p.BasePath
	dst.Endpoint = p.Endpoint
	dst.Region = p.Region
	dst.Bucket = p.Bucket
	dst.Prefix = p.Prefix
	dst.AccessKey = p.AccessKey
	dst.SecretKey = p.SecretKey
	dst.PathStyle = p.PathStyle
	dst.StorageClass = p.StorageClass
	dst.Host = p.Host
	dst.Port = p.Port
	dst.Username = p.Username
	dst.Password = p.Password
	dst.PrivateKey = p.PrivateKey
	dst.HostKey = p.HostKey
	dst.RateLimit = p.RateLimit
	if p.Enabled != nil {
		dst.Enabled = *p.Enabled
	}
	if p.UseSSL != nil {
		dst.UseSSL = *p.UseSSL
	}
}

func (p storagePayload) validate(isNew bool) error {
	if p.Name == "" {
		return badRequest("не указано имя хранилища")
	}
	switch model.StorageKind(p.Kind) {
	case model.StorageLocal:
		if p.BasePath == "" {
			return badRequest("для локального хранилища нужен путь")
		}
	case model.StorageS3:
		if p.Endpoint == "" || p.Bucket == "" {
			return badRequest("для S3 нужны endpoint и bucket")
		}
		if isNew && (p.AccessKey == "" || p.SecretKey == "") {
			return badRequest("для S3 нужны ключи доступа")
		}
	case model.StorageSFTP:
		if p.Host == "" || p.Username == "" {
			return badRequest("для SFTP нужны хост и пользователь")
		}
		if isNew && p.Password == "" && p.PrivateKey == "" {
			return badRequest("для SFTP нужен пароль или приватный ключ")
		}
	default:
		return badRequest("неизвестный тип хранилища: %q", p.Kind)
	}
	return nil
}

func (s *Server) handleListStorages(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListStorageTargets(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetStorage(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.GetStorageTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, target)
}

func (s *Server) handleCreateStorage(w http.ResponseWriter, r *http.Request) {
	var payload storagePayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := payload.validate(true); err != nil {
		s.writeError(w, r, err)
		return
	}

	target := &model.StorageTarget{Enabled: true, UseSSL: true}
	payload.apply(target)

	if err := s.store.CreateStorageTarget(r.Context(), target); err != nil {
		s.audit(r, "storage.create", model.ScopeStorageTarget, target.Name, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "storage.create", model.ScopeStorageTarget, target.ID, true, target.Name)

	// Check straight away: a repository that turns out to be unreachable is
	// better discovered now than during the first nightly backup.
	go s.checkStorage(context.WithoutCancel(r.Context()), target.ID)

	writeJSON(w, http.StatusCreated, target)
}

func (s *Server) handleUpdateStorage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetStorageTarget(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	var payload storagePayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := payload.validate(false); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.apply(existing)

	if err := s.store.UpdateStorageTarget(r.Context(), existing); err != nil {
		s.audit(r, "storage.update", model.ScopeStorageTarget, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "storage.update", model.ScopeStorageTarget, id, true, existing.Name)

	go s.checkStorage(context.WithoutCancel(r.Context()), id)
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteStorage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	force := queryBool(r, "force")

	// Deleting the definition would orphan the data: the backups stay in the
	// bucket, but nothing knows how to read them any more.
	count, err := s.store.CountRunsOnTarget(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if count > 0 && !force {
		s.writeError(w, r, badRequest(
			"в этом хранилище ещё лежит %d бэкапов; удалите их или повторите запрос с force=true "+
				"(данные в хранилище при этом останутся, но станут недоступны через интерфейс)", count))
		return
	}

	// A target with no backups yet can still be where a nightly job writes.
	// Deleting it silently turns that job into a failure at two in the morning.
	jobs, err := s.store.JobsOnTarget(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(jobs) > 0 && !force {
		s.writeError(w, r, badRequest(
			"в это хранилище пишут задания: %s. Уберите его из них или повторите запрос с force=true",
			strings.Join(jobs, ", ")))
		return
	}

	if err := s.store.DeleteStorageTarget(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "storage.delete", model.ScopeStorageTarget, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// storageCheckResult reports whether a repository is usable.
type storageCheckResult struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	FreeBytes int64  `json:"free_bytes"`
	UsedBytes int64  `json:"used_bytes"`
	Latency   string `json:"latency"`
}

func (s *Server) handleCheckStorage(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.GetStorageTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	started := time.Now()
	result := storageCheckResult{}

	backend, err := repo.Open(ctx, target)
	if err != nil {
		result.Error = err.Error()
	} else {
		defer backend.Close()
		if err := backend.Check(ctx); err != nil {
			result.Error = err.Error()
		} else {
			result.OK = true
			result.FreeBytes, result.UsedBytes, _ = backend.Usage(ctx)
		}
	}
	result.Latency = time.Since(started).Round(time.Millisecond).String()

	if err := s.store.UpdateStorageTargetHealth(r.Context(), target.ID, result.OK, result.Error,
		result.FreeBytes, result.UsedBytes); err != nil {
		s.log.Debug().Err(err).Msg("не удалось сохранить результат проверки хранилища")
	}
	writeJSON(w, http.StatusOK, result)
}

// checkStorage runs a repository check in the background.
func (s *Server) checkStorage(ctx context.Context, id string) {
	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	target, err := s.store.GetStorageTarget(checkCtx, id)
	if err != nil {
		return
	}
	backend, err := repo.Open(checkCtx, target)
	if err != nil {
		_ = s.store.UpdateStorageTargetHealth(checkCtx, id, false, err.Error(), 0, 0)
		return
	}
	defer backend.Close()

	if err := backend.Check(checkCtx); err != nil {
		_ = s.store.UpdateStorageTargetHealth(checkCtx, id, false, err.Error(), 0, 0)
		return
	}
	free, used, _ := backend.Usage(checkCtx)
	_ = s.store.UpdateStorageTargetHealth(checkCtx, id, true, "", free, used)
}
