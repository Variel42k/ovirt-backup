// Package fsbrowse lets an operator pick a directory from the interface
// instead of typing a path from memory.
//
// The point is not convenience alone. A typed path is checked once, when the
// backup runs at night; a picked path is checked now, against the same rule
// that will apply then. And the rule is the part that matters: the picker never
// shows the filesystem, only the roots policy already allows for that purpose.
//
// Everything here is addressed as (root, relative path) rather than as an
// absolute path. That is not decoration. Named roots for file backups hide
// their location on purpose — the configuration marks it json:"-" — and a
// browser that answered in absolute paths would hand that back on the first
// request. Relative addressing also removes a whole class of mistakes: there is
// no way to name a path outside a root, so refusing one is not the only line of
// defence.
package fsbrowse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrOutsideRoots means the requested path is not inside anything the operator
// is allowed to browse for this purpose.
var ErrOutsideRoots = errors.New("путь вне разрешённых каталогов")

// Root is a directory the operator may browse into.
type Root struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Path is shown only for roots whose location is itself the setting being
	// chosen — a storage directory, a restore area. Named file backup roots
	// leave it empty: the operator picks "Документы", not /srv/docs.
	Path     string `json:"path,omitempty"`
	Writable bool   `json:"writable"`

	// dir is the real location, never serialised.
	dir string
}

// NewRoot builds a root whose location is shown to the operator.
func NewRoot(id, name, dir string) Root {
	return Root{ID: id, Name: name, Path: dir, dir: dir}
}

// NewNamedRoot builds a root whose location stays hidden.
func NewNamedRoot(id, name, dir string) Root {
	return Root{ID: id, Name: name, dir: dir}
}

// Dir exposes the real location to the caller that configured the root.
func (r Root) Dir() string { return r.dir }

// Entry is one directory inside the listing.
type Entry struct {
	Name string `json:"name"`
	// Path is relative to the root, using forward slashes.
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	// Empty отличает пустой каталог от того, в который просто нельзя войти:
	// первый годится под новое хранилище, второй — повод разобраться с правами.
	Empty bool `json:"empty"`
}

// Listing is one directory rendered for the picker.
type Listing struct {
	Roots []Root `json:"roots"`
	// RootID пуст, когда показывается сам список корней.
	RootID string `json:"root_id,omitempty"`
	// Path — путь внутри корня; пусто означает сам корень.
	Path string `json:"path"`
	// Parent пуст в корне: подниматься выше некуда, и кнопка «вверх» там не
	// должна существовать вовсе, а не отказывать при нажатии.
	Parent *string `json:"parent"`
	// Absolute — полный путь, и только для корней, показывающих своё
	// расположение. Для именованных пусто.
	Absolute string  `json:"absolute,omitempty"`
	Writable bool    `json:"writable"`
	Entries  []Entry `json:"entries"`
}

// List enumerates the directories inside one root.
//
// An empty rootID returns just the roots, which is what the picker opens with.
func List(roots []Root, rootID, path string) (*Listing, error) {
	out := &Listing{Roots: roots, Entries: []Entry{}}
	if len(roots) == 0 || strings.TrimSpace(rootID) == "" {
		return out, nil
	}

	root, resolved, err := Resolve(roots, rootID, path)
	if err != nil {
		return nil, err
	}
	rel := relativeTo(root, resolved)

	out.RootID = root.ID
	out.Path = rel
	out.Writable = writable(resolved)
	if root.Path != "" {
		out.Absolute = resolved
	}
	if rel != "" {
		parent := parentOf(rel)
		out.Parent = &parent
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, fmt.Errorf("чтение каталога: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		full := filepath.Join(resolved, entry.Name())
		// Ссылка, ведущая из корня наружу, в список не попадает: иначе выбор
		// каталога стал бы способом обойти те самые корни.
		real, err := filepath.EvalSymlinks(full)
		if err != nil || !withinRoot(root, real) {
			continue
		}
		out.Entries = append(out.Entries, Entry{
			Name: entry.Name(), Path: joinRel(rel, entry.Name()),
			Writable: writable(full), Empty: isEmpty(full),
		})
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	return out, nil
}

// Resolve turns (root, relative path) into a real location, refusing anything
// that leaves the root.
//
// Symlinks are followed before the check, not after: a link inside a root
// pointing at /etc would otherwise pass a prefix comparison and hand back
// whatever it points to.
func Resolve(roots []Root, rootID, path string) (Root, string, error) {
	var root Root
	found := false
	for _, candidate := range roots {
		if candidate.ID == rootID {
			root, found = candidate, true
			break
		}
	}
	if !found {
		return Root{}, "", ErrOutsideRoots
	}

	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))
	switch {
	case clean == "." || clean == string(filepath.Separator):
		clean = ""
	case filepath.IsAbs(clean):
		// Абсолютный путь здесь не отвергается сразу: форма редактирования уже
		// хранит его, и повторное открытие выбиралки должно начинаться там же.
		// Проверка ниже всё равно потребует, чтобы он лежал внутри корня.
	case strings.HasPrefix(clean, ".."):
		return Root{}, "", ErrOutsideRoots
	}

	realRoot, err := filepath.EvalSymlinks(root.dir)
	if err != nil {
		return Root{}, "", ErrOutsideRoots
	}
	target := clean
	if !filepath.IsAbs(target) {
		target = filepath.Join(realRoot, clean)
	}

	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		// Несуществующий путь и путь наружу возвращают один ответ намеренно:
		// разница между ними сама по себе рассказывает о файловой системе.
		return Root{}, "", ErrOutsideRoots
	}
	root.dir = realRoot
	if !withinRoot(root, resolved) {
		return Root{}, "", ErrOutsideRoots
	}
	return root, resolved, nil
}

func withinRoot(root Root, path string) bool {
	return path == root.dir || strings.HasPrefix(path, root.dir+string(filepath.Separator))
}

func relativeTo(root Root, path string) string {
	if path == root.dir {
		return ""
	}
	return filepath.ToSlash(strings.TrimPrefix(path, root.dir+string(filepath.Separator)))
}

func parentOf(rel string) string {
	if idx := strings.LastIndex(rel, "/"); idx > 0 {
		return rel[:idx]
	}
	return ""
}

func joinRel(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}

// writable answers by trying, not by reading the mode.
//
// Внутри контейнера действуют и uid, и capabilities, и права на самой файловой
// системе, и совпадение владельца ничего не гарантирует. Проба стоит одного
// создания каталога и даёт ответ, который совпадёт с ночным.
func writable(path string) bool {
	probe, err := os.MkdirTemp(path, ".jhv-probe-")
	if err != nil {
		return false
	}
	_ = os.RemoveAll(probe)
	return true
}

func isEmpty(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	return err != nil || len(names) == 0
}
