package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// storagePayload is the write shape of a backup repository. Secrets are
// write-only: they go in through this struct and never come back out.
type storagePayload struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Enabled *bool  `json:"enabled"`

	BasePath string `json:"base_path"`

	Endpoint          string `json:"endpoint"`
	Region            string `json:"region"`
	Bucket            string `json:"bucket"`
	Prefix            string `json:"prefix"`
	AccessKey         string `json:"access_key"`
	SecretKey         string `json:"secret_key"`
	UseSSL            *bool  `json:"use_ssl"`
	PathStyle         bool   `json:"path_style"`
	StorageClass      string `json:"storage_class"`
	ObjectLockEnabled bool   `json:"object_lock_enabled"`
	ObjectLockDays    int    `json:"object_lock_days"`

	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
	HostKey    string `json:"host_key"`

	Share       string `json:"share"`
	Domain      string `json:"domain"`
	InsecureTLS bool   `json:"insecure_tls"`

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
	dst.ObjectLockEnabled = p.ObjectLockEnabled
	dst.ObjectLockDays = p.ObjectLockDays
	dst.Host = p.Host
	dst.Port = p.Port
	dst.Username = p.Username
	dst.Password = p.Password
	dst.PrivateKey = p.PrivateKey
	dst.HostKey = p.HostKey
	dst.Share = p.Share
	dst.Domain = p.Domain
	dst.InsecureTLS = p.InsecureTLS
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
	if p.RateLimit < 0 {
		return badRequest("ограничение скорости не может быть отрицательным")
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
		if p.ObjectLockEnabled && (p.ObjectLockDays < 1 || p.ObjectLockDays > 36500) {
			return badRequest("для Object Lock укажите срок от 1 до 36500 дней")
		}
	case model.StorageSFTP:
		if p.Host == "" || p.Username == "" {
			return badRequest("для SFTP нужны хост и пользователь")
		}
		if isNew && p.Password == "" && p.PrivateKey == "" {
			return badRequest("для SFTP нужен пароль или приватный ключ")
		}
	case model.StorageSMB:
		if p.Host == "" {
			return badRequest("для SMB нужен адрес сервера")
		}
		if strings.Trim(p.Share, `\/ `) == "" {
			return badRequest("для SMB нужно имя сетевой папки")
		}
		// Оператор копирует адрес целиком, в виде \\nas\backups, и вставляет его
		// в поле имени папки. Отказ с объяснением дешевле, чем подключение,
		// которое падает на невнятной ошибке сервера.
		if strings.ContainsAny(strings.Trim(p.Share, `\/`), `\/`) {
			return badRequest("в имени сетевой папки не должно быть разделителей: " +
				"имя сервера задаётся отдельно, путь внутри папки — тоже")
		}
		if p.Username == "" {
			return badRequest("для SMB нужен пользователь: анонимный доступ к шаре не поддерживается")
		}
		if isNew && p.Password == "" {
			return badRequest("для SMB нужен пароль")
		}
	case model.StorageWebDAV:
		if p.Endpoint == "" {
			return badRequest("для WebDAV нужен адрес коллекции")
		}
		if p.Username == "" {
			return badRequest("для WebDAV нужен пользователь")
		}
		if isNew && p.Password == "" {
			return badRequest("для WebDAV нужен пароль")
		}
	default:
		return badRequest("неизвестный тип хранилища: %q", p.Kind)
	}
	if p.InsecureTLS && model.StorageKind(p.Kind) != model.StorageWebDAV {
		return badRequest("отключение проверки сертификата доступно только для WebDAV")
	}
	if model.StorageKind(p.Kind) != model.StorageS3 && p.ObjectLockEnabled {
		return badRequest("Object Lock доступен только для S3")
	}
	if !p.ObjectLockEnabled && p.ObjectLockDays != 0 {
		return badRequest("срок Object Lock должен быть 0, когда блокировка выключена")
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

	if err := ensureLocalPathUsable(r.Context(), target); err != nil {
		s.audit(r, "storage.create", model.ScopeStorageTarget, target.Name, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	if err := ensureObjectLockUsable(r.Context(), target); err != nil {
		s.audit(r, "storage.create", model.ScopeStorageTarget, target.Name, false, err.Error())
		s.writeError(w, r, err)
		return
	}

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
	secretKey, password, privateKey := existing.SecretKey, existing.Password, existing.PrivateKey
	objectLockWasEnabled := existing.ObjectLockEnabled
	// Копия до правки: по ней решается, менялась ли сама цель — куда пишутся
	// копии и под какими учётными данными.
	before := *existing
	payload.apply(existing)
	if payload.SecretKey == "" {
		existing.SecretKey = secretKey
	}
	if payload.Password == "" {
		existing.Password = password
	}
	if payload.PrivateKey == "" {
		existing.PrivateKey = privateKey
	}
	if objectLockWasEnabled && !existing.ObjectLockEnabled {
		count, countErr := s.store.CountRunsOnTarget(r.Context(), id)
		if countErr != nil {
			s.writeError(w, r, countErr)
			return
		}
		if count > 0 {
			s.writeError(w, r, badRequest(
				"нельзя выключить Object Lock: в хранилище учтено %d физических копий; "+
					"сначала дождитесь срока блокировки и удалите их", count))
			return
		}
	}

	if err := ensureLocalPathUsable(r.Context(), existing); err != nil {
		s.audit(r, "storage.update", model.ScopeStorageTarget, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	if err := ensureObjectLockUsable(r.Context(), existing); err != nil {
		s.audit(r, "storage.update", model.ScopeStorageTarget, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}

	// Продолжение не читает тело: оно уже разобрано выше, а согласование
	// может вызвать его позже — повторным запросом со ссылкой на заявку.
	save := func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.UpdateStorageTarget(r.Context(), existing); err != nil {
			s.audit(r, "storage.update", model.ScopeStorageTarget, id, false, err.Error())
			s.writeError(w, r, err)
			return
		}
		s.audit(r, "storage.update", model.ScopeStorageTarget, id, true, existing.Name)

		go s.checkStorage(context.WithoutCancel(r.Context()), id)
		writeJSON(w, http.StatusOK, existing)
	}

	// Смена адреса или учётных данных уводит будущие копии в другое место, а
	// старые делает недоступными. Переименование — нет, и требовать за него
	// кворум значило бы сделать согласование помехой вместо защиты.
	if before.Retargeted(existing) {
		s.guardAction(w, r, model.GuardStorageRetarget, retargetTarget(&before, existing), save)
		return
	}
	save(w, r)
}

// retargetTarget описывает смену цели словами: согласующий должен видеть, что
// именно подтверждает.
//
// Секреты в описание не попадают — ни старые, ни новые. Заявка живёт в базе и
// уходит в оповещения, то есть ключ из неё оказался бы сразу в двух местах,
// откуда его не отозвать.
func retargetTarget(before, after *model.StorageTarget) guardedTarget {
	changes := []string{}
	add := func(what, was, now string) {
		if was != now {
			changes = append(changes, what+": «"+was+"» → «"+now+"»")
		}
	}
	add("тип", string(before.Kind), string(after.Kind))
	add("каталог", before.BasePath, after.BasePath)
	add("адрес", before.Endpoint, after.Endpoint)
	add("регион", before.Region, after.Region)
	add("bucket", before.Bucket, after.Bucket)
	add("префикс", before.Prefix, after.Prefix)
	add("ключ доступа", before.AccessKey, after.AccessKey)
	add("узел", before.Host, after.Host)
	add("пользователь", before.Username, after.Username)
	add("шара", before.Share, after.Share)
	add("домен", before.Domain, after.Domain)
	if before.Port != after.Port {
		changes = append(changes, fmt.Sprintf("порт: %d → %d", before.Port, after.Port))
	}
	if before.SecretKey != after.SecretKey || before.Password != after.Password ||
		before.PrivateKey != after.PrivateKey {
		changes = append(changes, "заменены учётные данные")
	}

	summary := "смена цели хранилища «" + before.Name + "»"
	if len(changes) > 0 {
		summary += " — " + strings.Join(changes, "; ")
	}
	return guardedTarget{ID: before.ID, Name: before.Name, Summary: summary}
}

// ensureLocalPathUsable refuses to save a local repository whose directory
// cannot be created.
//
// Для сетевых хранилищ такая проверка была бы вредной: S3 или SFTP бывают
// временно недоступны, и запрет на сохранение из-за этого означал бы, что
// настроить хранилище можно только при работающей сети. С локальным каталогом
// иначе: он либо создаётся сразу, либо не создастся никогда, и отказ здесь —
// это опечатка в пути, а не сбой.
//
// Раньше запись сохранялась, а отказ приходил позже, из фоновой проверки. Со
// стороны оператора это выглядело как неисправное хранилище, а не как неверный
// путь: он снова шёл в форму создания, хотя править нужно было уже сохранённую
// запись, которая продолжала выдавать ту же ошибку при каждой проверке.
func ensureLocalPathUsable(ctx context.Context, target *model.StorageTarget) error {
	if target.Kind != model.StorageLocal {
		return nil
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return badRequest("%v", err)
	}
	return backend.Close()
}

func ensureObjectLockUsable(ctx context.Context, target *model.StorageTarget) error {
	if !target.ObjectLockEnabled {
		return nil
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return badRequest("Object Lock: %v", err)
	}
	defer backend.Close()
	validator, ok := backend.(repo.ObjectLockValidator)
	if !ok {
		return badRequest("выбранное хранилище не поддерживает Object Lock")
	}
	if err := validator.CheckObjectLock(ctx); err != nil {
		return badRequest("Object Lock: %v", err)
	}
	return nil
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
	OK                bool   `json:"ok"`
	Error             string `json:"error,omitempty"`
	FreeBytes         int64  `json:"free_bytes"`
	UsedBytes         int64  `json:"used_bytes"`
	Latency           string `json:"latency"`
	ObjectLockEnabled bool   `json:"object_lock_enabled"`
	ObjectLockOK      bool   `json:"object_lock_ok"`
}

// handleCheckImmutability выясняет, может ли служба стереть записанное.
//
// Проверка отдельная от обычной, а не часть её, по двум причинам. Она пишет
// пробный объект, который на защищённом хранилище останется лежать до конца
// срока удержания — делать это при каждой периодической проверке доступности
// значило бы засыпать хранилище неудаляемым мусором. И она отвечает на другой
// вопрос: обычная проверяет, что хранилище работает, эта — что оно защищает.
func (s *Server) handleCheckImmutability(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.GetStorageTarget(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	backend, err := repo.Open(ctx, target)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer backend.Close()

	report := repo.CheckImmutability(ctx, backend)

	if err := s.store.SetStorageImmutability(context.WithoutCancel(ctx), target.ID,
		string(report.State), report.Detail, report.CheckedAt); err != nil {
		// Итог уже получен: не отдать его из-за неудачной записи в базу было бы
		// хуже, чем отдать несохранённым.
		s.log.Warn().Err(err).Str("хранилище", target.Name).
			Msg("итог проверки неизменяемости не сохранён")
	}

	s.audit(r, "storage.immutability_check", model.ScopeStorageTarget, target.ID, true,
		report.Describe())
	writeJSON(w, http.StatusOK, report)
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
	result := storageCheckResult{ObjectLockEnabled: target.ObjectLockEnabled}

	backend, err := repo.Open(ctx, target)
	if err != nil {
		result.Error = err.Error()
	} else {
		defer backend.Close()
		if err := backend.Check(ctx); err != nil {
			result.Error = err.Error()
		} else {
			result.OK = true
			result.ObjectLockOK = !target.ObjectLockEnabled
			if validator, ok := backend.(repo.ObjectLockValidator); ok && target.ObjectLockEnabled {
				if lockErr := validator.CheckObjectLock(ctx); lockErr != nil {
					result.OK, result.Error = false, lockErr.Error()
				} else {
					result.ObjectLockOK = true
				}
			}
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
