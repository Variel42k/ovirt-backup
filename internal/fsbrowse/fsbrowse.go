// Пакет fsbrowse позволяет оператору выбрать каталог из интерфейса вместо ввода пути из
// памяти.
//
// Выбранный путь проверяется сразу по тем же правилам, которые применяются при запуске
// бэкапа. В диалоге видны только корни, разрешённые для этой операции.
//
// Путь задаётся парой «корень и относительный путь». Пути именованных корней скрыты через
// json:"-". Возврат абсолютного пути выдал бы их при первом запросе. Относительная адресация
// также ограничивает запросы выбранным корнем.
package fsbrowse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrOutsideRoots означает, что запрашиваемый путь не находится внутри чего-либо, что
// оператору разрешено просматривать для этой цели.
var ErrOutsideRoots = errors.New("путь вне разрешённых каталогов")

// Root — это каталог, в который оператор может просматривать.
type Root struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Путь показан только для корней, местоположение которых само является выбранной
	// настройкой — каталог хранения, область восстановления. Именованные корни резервной
	// копии файла
	// leave it empty: the operator picks "Документы", not /srv/docs.
	Path     string `json:"path,omitempty"`
	Writable bool   `json:"writable"`

	// Dir — это реальное местоположение, никогда не сериализованное.
	dir string
}

// NewRoot строит корень, местоположение которого показано оператору.
func NewRoot(id, name, dir string) Root {
	return Root{ID: id, Name: name, Path: dir, dir: dir}
}

// NewNamedRoot постро́ка ж. корень, местопребывание которого остается скрытым.
func NewNamedRoot(id, name, dir string) Root {
	return Root{ID: id, Name: name, dir: dir}
}

// Dir показывает реальное местоположение вызывающему коду, который настроил корень.
func (r Root) Dir() string { return r.dir }

// Entry является одним каталогом внутри списка.
type Entry struct {
	Name string `json:"name"`
	// Путь относительно корня, используя передние косые черты.
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	// Empty отличает пустой каталог от того, в который просто нельзя войти:
	// первый годится под новое хранилище, второй — повод разобраться с правами.
	Empty bool `json:"empty"`
}

// Listing является одним каталогом, отображаемым для пикера.
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

// Список перечисляет каталоги внутри одного корня.
//
// Пустой rootID возвращает только корни, чем и открывается собиратель.
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

// Resolve превращается (корень, относительный путь) в реальное место, отказываясь от всего,
// что оставляет корень.
//
// Ссылки следуют до проверки, а не после: ссылка внутри корня, указывающая на /etc, в
// противном случае передавала бы сравнение префиксов и возвращала бы все, на что она
// указывает.
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
	// Корень без расположения — не каталог на этом сервере (например,
	// подключённое хранилище): пустой путь разрешился бы относительно рабочего
	// каталога службы.
	if strings.TrimSpace(root.dir) == "" {
		return Root{}, "", ErrOutsideRoots
	}

	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))
	switch {
	case clean == ".":
		clean = ""
	case clean == string(filepath.Separator) && filepath.Clean(root.dir) == string(filepath.Separator):
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

// Записывайте ответы, пытаясь, а не читая режим.
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
