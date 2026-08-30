package api

import (
	"errors"
	"net/http"
	"strconv"

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
			roots: func(*Server, *http.Request) []fsbrowse.Root {
				var out []fsbrowse.Root
				for _, mount := range repo.WritableMounts() {
					out = append(out, fsbrowse.NewRoot(mount, mount, mount))
				}
				return out
			},
			emptyHint: "Служба не нашла ни одного смонтированного каталога, доступного ей на " +
				"запись. В установке из контейнера каталог под копии подключается монтированием.",
		},
		scopeFileBackup: {
			permission: model.PermJobsAdmin,
			roots: func(s *Server, _ *http.Request) []fsbrowse.Root {
				var out []fsbrowse.Root
				for _, root := range s.cfg.FileBackup.Roots {
					out = append(out, fsbrowse.NewNamedRoot(root.ID, root.Name, root.Path))
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
				root, ok := s.cfg.FileBackup.Root(r.URL.Query().Get("owner"))
				if !ok {
					return nil
				}
				var out []fsbrowse.Root
				for index, dir := range root.RestoreRoots {
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
