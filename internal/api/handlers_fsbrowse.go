package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/filebackup"
	"github.com/Variel42k/ovirt-backup/internal/fsbrowse"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// Выбор каталога мышью вместо пути, набранного по памяти.
//
// Список каталогов — сведения о хосте, поэтому «показать» здесь такое же
// действие, как «записать»: право спрашивается то же, что и у настройки, ради
// которой каталог выбирают. Отсюда назначение (scope) в запросе — оно
// определяет и права, и корни, за пределы которых выйти нельзя.
const (
	// scopeStorage — куда класть копии: точки монтирования, доступные службе на
	// запись. Список тот же, что подсказывает форма хранилища, и получен так же
	// — пробой, а не разбором прав.
	scopeStorage = "storage"
	// scopeFileBackup — что бэкапить: именованные корни из конфигурации. Их
	// расположение наружу не отдаётся, оператор выбирает «Документы», а не
	// /srv/docs.
	scopeFileBackup = "file-backup"
	// scopeFileRestore — куда восстанавливать файлы: разрешённые области
	// выбранного именованного корня.
	scopeFileRestore = "file-restore"
	// scopeRestore — куда восстанавливать диски: backup.restore_dirs и temp_dir.
	scopeRestore = "restore"
)

// scopeRule ties a purpose to the permission it needs and the roots it may show.
type scopeRule struct {
	permission model.Permission
	// roots получает запрос: у восстановления файлов набор областей зависит от
	// выбранного корня, и он приходит параметром.
	roots func(s *Server, r *http.Request) []fsbrowse.Root
	// emptyHint объясняет пустой список корней. Без него оператор видит пустое
	// окно и не понимает, сломалось что-то или так задумано.
	emptyHint string
}

func browseScopes() map[string]scopeRule {
	return map[string]scopeRule{
		scopeStorage: {
			permission: model.PermStoragesAdmin,
			roots: func(s *Server, _ *http.Request) []fsbrowse.Root {
				var out []fsbrowse.Root
				for _, mount := range s.storageMounts() {
					out = append(out, fsbrowse.NewRoot(mount, mount, mount))
				}
				return out
			},
			emptyHint: "Служба не видит ни одного смонтированного каталога для копий. В установке " +
				"из контейнера такой каталог подключается монтированием (bind mount); сам корень " +
				"может быть только для чтения — копии кладут в его подкаталог с правом записи.",
		},
		scopeFileBackup: {
			permission: model.PermJobsAdmin,
			roots: func(s *Server, r *http.Request) []fsbrowse.Root {
				var out []fsbrowse.Root
				for _, root := range s.cfg.FileBackup.Roots {
					out = append(out, fsbrowse.NewNamedRoot(root.ID, root.Name, root.Path))
				}
				// Подключённые хранилища — тоже источники. Их каталоги читает
				// не файловая система службы, а само хранилище, поэтому у
				// таких корней нет расположения: handleBrowse разбирает их сам.
				if storages, err := s.fileBackupStorageRoots(r.Context()); err == nil {
					for _, item := range storages {
						out = append(out, fsbrowse.NewNamedRoot(item.ID, item.Name, ""))
					}
				}
				return out
			},
			emptyHint: "Именованные корни не заданы. Их задаёт администратор в конфигурации " +
				"службы, в file_backup.roots: выбирать произвольный каталог на хосте из " +
				"интерфейса нельзя намеренно.",
		},
		scopeFileRestore: {
			permission: model.PermBackupsWrite,
			roots: func(s *Server, r *http.Request) []fsbrowse.Root {
				// У хранилища-источника своих областей нет: файлы возвращаются
				// в области первого именованного корня.
				owner := r.URL.Query().Get("owner")
				var dirs []string
				if s.fileBackup != nil {
					dirs = s.fileBackup.RestoreRoots(owner)
				} else if root, ok := s.cfg.FileBackup.Root(owner); ok {
					dirs = root.RestoreRoots
				}
				if len(dirs) == 0 {
					return nil
				}
				var out []fsbrowse.Root
				for index, dir := range dirs {
					// Идентификатор — номер области: именно его ждёт
					// восстановление, и придумывать второй способ адресации
					// значило бы разойтись с ним при первой же правке.
					out = append(out, fsbrowse.NewNamedRoot(strconv.Itoa(index),
						"Разрешённая область "+strconv.Itoa(index+1), dir))
				}
				return out
			},
			emptyHint: "У этого корня не задано ни одной области для восстановления: " +
				"restore_roots в его настройке.",
		},
		scopeRestore: {
			permission: model.PermBackupsWrite,
			roots: func(s *Server, _ *http.Request) []fsbrowse.Root {
				var out []fsbrowse.Root
				for _, dir := range s.cfg.Backup.RestoreRoots() {
					out = append(out, fsbrowse.NewRoot(dir, dir, dir))
				}
				return out
			},
			emptyHint: "Каталоги для восстановления не заданы: backup.restore_dirs и " +
				"backup.temp_dir в конфигурации службы.",
		},
	}
}

type browseResponse struct {
	*fsbrowse.Listing
	Scope string `json:"scope"`
	Hint  string `json:"hint,omitempty"`
}

// handleBrowse lists the directories inside one allowed root.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	rule, ok := browseScopes()[scope]
	if !ok {
		s.writeError(w, r, badRequest("неизвестное назначение: %q", scope))
		return
	}

	// Право проверяется здесь, а не маршрутом: у каждого назначения оно своё,
	// и общий обработчик не должен быть дырой в обход этой разницы.
	principal := principalFrom(r.Context())
	if principal == nil || !principal.Can(rule.permission) {
		writeJSON(w, http.StatusForbidden, errorResponse{
			Error: "нет права " + string(rule.permission), Code: "forbidden",
		})
		return
	}

	roots := rule.roots(s, r)
	if targetID, ok := filebackup.StorageRootTarget(r.URL.Query().Get("root")); ok && scope == scopeFileBackup {
		listing, err := s.browseStorageRoot(r.Context(), roots, targetID, r.URL.Query().Get("path"))
		if err != nil {
			s.writeError(w, r, badRequest("%v", err))
			return
		}
		writeJSON(w, http.StatusOK, browseResponse{Listing: listing, Scope: scope})
		return
	}
	listing, err := fsbrowse.List(roots, r.URL.Query().Get("root"), r.URL.Query().Get("path"))
	if err != nil {
		if errors.Is(err, fsbrowse.ErrOutsideRoots) {
			s.audit(r, "fs.browse", model.ScopeStorageTarget, scope, false,
				"попытка выйти за разрешённые каталоги: "+r.URL.Query().Get("path"))
			s.writeError(w, r, badRequest("%v", err))
			return
		}
		s.writeError(w, r, err)
		return
	}

	response := browseResponse{Listing: listing, Scope: scope}
	if len(roots) == 0 {
		response.Hint = rule.emptyHint
	}
	writeJSON(w, http.StatusOK, response)
}

// storageBrowseTimeout ограничивает чтение одного каталога хранилища: окно
// выбора не должно висеть, пока сетевая папка не отвечает.
const storageBrowseTimeout = 45 * time.Second

// browseStorageRoot lists the directories of one level of a connected storage
// for the picker.
//
// Каталоги читает само хранилище, теми же учётными данными, что и проверка
// его доступности. Выйти за его корень путём нельзя: путь нормализуется от
// корня. Скрытые каталоги и служебные данные репозитория не показываются.
func (s *Server) browseStorageRoot(ctx context.Context, roots []fsbrowse.Root, targetID, dir string) (*fsbrowse.Listing, error) {
	if err := s.requireFileBackup(); err != nil {
		return nil, err
	}
	rootID := filebackup.StorageRootID(targetID)
	known := false
	for _, root := range roots {
		known = known || root.ID == rootID
	}
	if !known {
		return nil, fmt.Errorf("хранилище недоступно как источник: оно отключено или не отдаёт каталоги")
	}
	ctx, cancel := context.WithTimeout(ctx, storageBrowseTimeout)
	defer cancel()
	backend, _, err := s.fileBackup.OpenStorageSource(ctx, targetID)
	if err != nil {
		return nil, err
	}
	defer backend.Close()

	rel := repo.CleanDir(dir)
	entries, err := repo.ListDir(ctx, backend, rel)
	if err != nil {
		return nil, err
	}
	listing := &fsbrowse.Listing{Roots: roots, RootID: rootID, Path: rel, Entries: []fsbrowse.Entry{}}
	if rel != "" {
		parent := path.Dir(rel)
		if parent == "." {
			parent = ""
		}
		listing.Parent = &parent
	}
	for _, entry := range entries {
		if !entry.IsDir || strings.HasPrefix(entry.Name, ".") || strings.HasPrefix(entry.Name, "$") {
			continue
		}
		listing.Entries = append(listing.Entries, fsbrowse.Entry{Name: entry.Name, Path: path.Join(rel, entry.Name)})
	}
	return listing, nil
}

// resolveBrowsable reports where a chosen path really is, refusing anything
// outside the roots of its scope.
//
// Нужна затем, что выбор мышью не отменяет проверки при сохранении: форма — это
// обычный HTTP-запрос, и отправить её можно с любым путём.
func (s *Server) resolveBrowsable(r *http.Request, scope, path string) (string, error) {
	rule, ok := browseScopes()[scope]
	if !ok {
		return "", badRequest("неизвестное назначение: %q", scope)
	}
	roots := rule.roots(s, r)
	for _, root := range roots {
		if _, resolved, err := fsbrowse.Resolve(roots, root.ID, path); err == nil {
			return resolved, nil
		}
	}
	return "", badRequest("%v", fsbrowse.ErrOutsideRoots)
}
