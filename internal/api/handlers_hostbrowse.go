package api

import (
	"net/http"
	"path"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/fsbrowse"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Обзор каталогов на самом гипервизоре.
//
// Отдельно от /fs/browse потому, что читается чужая файловая система через уже
// открытое SSH-соединение, а не своя. Новых доступов при этом не появляется —
// служба и так ходит на этот хост, — но появляется новая возможность: право
// servers:admin позволяет менять учётные данные подключения и не позволяет
// читать файлы хоста. Обзор без границ стёр бы эту разницу, поэтому корни
// заданы списком (backup.scratch_roots), а пустой список запрещает обзор вовсе.

// handleBrowseHost lists directories on a hypervisor for the scratch directory
// field.
func (s *Server) handleBrowseHost(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !srv.Kind.UsesLibvirt() {
		s.writeError(w, r, badRequest("каталоги на хосте доступны только для подключений libvirt/KVM"))
		return
	}

	allowed := s.cfg.Backup.ScratchRoots
	roots := make([]fsbrowse.Root, 0, len(allowed))
	for _, dir := range allowed {
		roots = append(roots, fsbrowse.NewRoot(dir, dir, dir))
	}
	listing := &fsbrowse.Listing{Roots: roots, Entries: []fsbrowse.Entry{}}

	rootID := r.URL.Query().Get("root")
	if len(roots) == 0 || strings.TrimSpace(rootID) == "" {
		hint := ""
		if len(roots) == 0 {
			hint = "Обзор каталогов на гипервизоре выключен: список разрешённых корней пуст " +
				"(backup.scratch_roots). Путь вводится вручную."
		}
		writeJSON(w, http.StatusOK, browseResponse{Listing: listing, Scope: "host", Hint: hint})
		return
	}

	root, ok := findRoot(roots, rootID)
	if !ok {
		s.writeError(w, r, badRequest("%v", fsbrowse.ErrOutsideRoots))
		return
	}

	// Путь собирается здесь, а не приходит целиком: относительная адресация
	// не даёт назвать каталог за пределами корня, и проверка ниже становится
	// вторым рубежом, а не единственным.
	rel := path.Clean("/" + strings.TrimSpace(r.URL.Query().Get("path")))
	target := path.Join(root.Dir(), rel)

	conn, err := s.libvirt.ForServer(r.Context(), srv)
	if err != nil {
		s.writeError(w, r, badRequest("подключение к гипервизору: %v", err))
		return
	}

	remote, err := conn.ListDirectories(r.Context(), target)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}

	// Ссылки гипервизор разрешил у себя; сравнивается именно то, во что путь
	// разрешился, — иначе ссылка внутри корня, ведущая в /etc, прошла бы
	// проверку по префиксу и отдала содержимое чужого каталога.
	if !withinRemoteRoot(root.Dir(), remote.Canonical) {
		s.audit(r, "fs.browse_host", model.ScopeServer, srv.ID, false,
			"путь вне разрешённых каталогов: "+remote.Canonical)
		s.writeError(w, r, badRequest("%v", fsbrowse.ErrOutsideRoots))
		return
	}

	listing.RootID = root.ID
	listing.Path = strings.TrimPrefix(strings.TrimPrefix(remote.Canonical, root.Dir()), "/")
	listing.Absolute = remote.Canonical
	listing.Writable = remote.Writable
	if listing.Path != "" {
		parent := path.Dir(listing.Path)
		if parent == "." {
			parent = ""
		}
		listing.Parent = &parent
	}
	for _, name := range remote.Entries {
		listing.Entries = append(listing.Entries, fsbrowse.Entry{
			Name: name,
			Path: strings.TrimPrefix(listing.Path+"/"+name, "/"),
			// Права каждого вложенного каталога отдельной пробой не выясняются:
			// это ещё один вход по SSH на каждую строку списка. Проверяется тот
			// каталог, который оператор в итоге выберет.
			Writable: remote.Writable,
		})
	}
	writeJSON(w, http.StatusOK, browseResponse{Listing: listing, Scope: "host"})
}

func findRoot(roots []fsbrowse.Root, id string) (fsbrowse.Root, bool) {
	for _, root := range roots {
		if root.ID == id {
			return root, true
		}
	}
	return fsbrowse.Root{}, false
}

func withinRemoteRoot(root, candidate string) bool {
	root = strings.TrimRight(root, "/")
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}
