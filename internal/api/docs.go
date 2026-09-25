package api

import (
	"bufio"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Variel42k/ovirt-backup/docs"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Руководства из docs/ в веб-интерфейсе.
//
// Справочник в help.go объясняет понятия рядом с полями формы. Руководства —
// другое: установка, настройка, эксплуатация, диагностика. Они живут в
// репозитории в Markdown и раньше были видны только там или в распакованном
// комплекте. Теперь те же файлы встроены в бинарь и отдаются интерфейсу как
// есть: одна версия текста на всё, без пересказа, который разойдётся с
// оригиналом при первой же правке.

// guidePlacement задаёт место руководства в оглавлении.
type guidePlacement struct {
	category string
	order    int
	icon     string
}

// guideCategoryRank задаёт порядок разделов оглавления.
var guideCategoryRank = map[string]int{
	"Начало работы":         10,
	"Настройка":             20,
	"Доступ и безопасность": 30,
	"Эксплуатация":          40,
	"Архитектура":           50,
	"Справочник API":        60,
}

// otherGuidesCategory собирает файлы, которых нет в guidePlacements. Новое
// руководство не должно молча пропадать из интерфейса только потому, что его
// забыли вписать в раскладку.
const otherGuidesCategory = "Прочее"

var guidePlacements = map[string]guidePlacement{
	"GUIDE.md":             {"Начало работы", 10, "rocket_launch"},
	"DEPLOY.md":            {"Начало работы", 20, "install_desktop"},
	"DNS.md":               {"Начало работы", 30, "dns"},
	"BUILD.md":             {"Начало работы", 40, "construction"},
	"CONFIGURATION.md":     {"Настройка", 10, "tune"},
	"INTEGRATIONS.md":      {"Настройка", 20, "hub"},
	"KEYCLOAK-AD.md":       {"Доступ и безопасность", 10, "badge"},
	"ROLES.md":             {"Доступ и безопасность", 20, "admin_panel_settings"},
	"SECURITY.md":          {"Доступ и безопасность", 30, "shield"},
	"OPERATIONS.md":        {"Эксплуатация", 10, "settings_suggest"},
	"TROUBLESHOOTING.md":   {"Эксплуатация", 20, "build_circle"},
	"LIMITATIONS.md":       {"Эксплуатация", 30, "report"},
	"ARCHITECTURE.md":      {"Архитектура", 10, "account_tree"},
	"DISK-ARCHITECTURE.md": {"Архитектура", 20, "storage"},
	"GUEST-FREEZE.md":      {"Архитектура", 25, "ac_unit"},
	"BACKUP-FORMAT.md":     {"Архитектура", 30, "inventory_2"},
	"PLAN-RECOVERY.md":     {"Архитектура", 40, "timeline"},
	"API.md":               {"Справочник API", 10, "api"},
}

// docGuide — строка оглавления руководств.
type docGuide struct {
	Slug     string `json:"slug"`
	File     string `json:"file"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Category string `json:"category"`
	Icon     string `json:"icon"`
	// Words — слова вне блоков кода: по ним интерфейс оценивает время чтения.
	Words int `json:"words"`
}

type docGuideContent struct {
	docGuide
	Markdown string `json:"markdown"`
}

type guideCatalog struct {
	list    []docGuide
	bySlug  map[string]docGuide
	content map[string]string
}

// guides разбирает встроенные файлы один раз: пока работает процесс, они не
// меняются.
var guides = sync.OnceValue(func() guideCatalog { return buildGuideCatalog(docs.Files) })

func buildGuideCatalog(files fs.FS) guideCatalog {
	catalog := guideCatalog{bySlug: map[string]docGuide{}, content: map[string]string{}}
	names, err := fs.Glob(files, "*.md")
	if err != nil {
		return catalog
	}

	type ranked struct {
		guide       docGuide
		rank, order int
	}
	var all []ranked
	for _, name := range names {
		raw, err := fs.ReadFile(files, name)
		if err != nil {
			continue
		}
		text := string(raw)
		category, order, icon := otherGuidesCategory, 1000, "description"
		if placement, ok := guidePlacements[name]; ok {
			category, order, icon = placement.category, placement.order, placement.icon
		}
		rank, ok := guideCategoryRank[category]
		if !ok {
			rank = 1000
		}
		title, summary, words := guideOutline(text)
		if title == "" {
			title = strings.TrimSuffix(name, ".md")
		}
		guide := docGuide{
			Slug: guideSlug(name), File: name, Title: title, Summary: summary,
			Category: category, Icon: icon, Words: words,
		}
		catalog.bySlug[guide.Slug] = guide
		catalog.content[guide.Slug] = text
		all = append(all, ranked{guide: guide, rank: rank, order: order})
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].rank != all[j].rank {
			return all[i].rank < all[j].rank
		}
		if all[i].order != all[j].order {
			return all[i].order < all[j].order
		}
		return all[i].guide.Title < all[j].guide.Title
	})
	for _, item := range all {
		catalog.list = append(catalog.list, item.guide)
	}
	return catalog
}

// guideSlug превращает имя файла в идентификатор для адреса: KEYCLOAK-AD.md →
// keycloak-ad.
func guideSlug(file string) string {
	return strings.ToLower(strings.TrimSuffix(path.Base(file), ".md"))
}

// Состояния разбора вводной части руководства.
const (
	outlineSeekParagraph = iota // ещё не встретили обычного текста
	outlineParagraph            // собираем первый абзац
	outlineSeekList             // абзац кончился двоеточием — ждём список
	outlineList                 // собираем этот список
	outlineDone
)

// guideOutline достаёт заголовок первого уровня, вводный абзац под ним — им
// руководство представлено в оглавлении — и число слов вне блоков кода.
//
// Вводный абзац часто обрывается двоеточием перед списком: «поддерживаются два
// варианта:». Без списка такое описание на карточке выглядит поломанным,
// поэтому пункты сразу за ним подхватываются.
func guideOutline(text string) (title, summary string, words int) {
	var paragraph, items []string
	state := outlineSeekParagraph
	fence := ""

	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if fence != "" {
			if strings.HasPrefix(line, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fence = line[:3]
			if state != outlineSeekParagraph {
				state = outlineDone
			}
			continue
		}
		words += len(strings.Fields(line))

		if title == "" {
			if strings.HasPrefix(line, "# ") {
				title = plainInlineMarkdown(strings.TrimPrefix(line, "# "))
			}
			continue
		}

		switch state {
		case outlineSeekParagraph:
			// Структура до первого абзаца — таблица, подзаголовок — пропускается.
			if line != "" && !isStructuralMarkdownLine(line) {
				paragraph = append(paragraph, line)
				state = outlineParagraph
			}
		case outlineParagraph:
			switch {
			case line != "" && !isStructuralMarkdownLine(line):
				paragraph = append(paragraph, line)
			case strings.HasSuffix(paragraph[len(paragraph)-1], ":"):
				state = outlineSeekList
				if item, ok := listItemText(line); ok {
					items, state = append(items, item), outlineList
				}
			default:
				state = outlineDone
			}
		case outlineSeekList:
			if line == "" {
				continue
			}
			if item, ok := listItemText(line); ok {
				items, state = append(items, item), outlineList
			} else {
				state = outlineDone
			}
		case outlineList:
			switch item, ok := listItemText(line); {
			case line == "":
				state = outlineSeekList
			case ok:
				items = append(items, item)
			case raw != line:
				// Отступ — продолжение текущего пункта.
				items[len(items)-1] += " " + line
			default:
				state = outlineDone
			}
		}
	}

	summary = strings.Join(append(paragraph, items...), " ")
	return title, clipRunes(plainInlineMarkdown(summary), 240), words
}

var (
	orderedListItem   = regexp.MustCompile(`^\d+[.)]\s`)
	unorderedListItem = regexp.MustCompile(`^[-*+]\s`)
)

// listItemText возвращает текст пункта списка без маркера.
func listItemText(line string) (string, bool) {
	for _, marker := range []*regexp.Regexp{orderedListItem, unorderedListItem} {
		if loc := marker.FindStringIndex(line); loc != nil {
			return strings.TrimSpace(line[loc[1]:]), true
		}
	}
	return "", false
}

func isStructuralMarkdownLine(line string) bool {
	for _, prefix := range []string{"#", ">", "|", "- ", "* ", "+ ", "---", "***", "<"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return orderedListItem.MatchString(line)
}

var (
	markdownLink = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	extraSpaces  = regexp.MustCompile(`\s+`)
)

// plainInlineMarkdown убирает строчную разметку: оглавление показывает текст,
// а не звёздочки и обратные кавычки.
func plainInlineMarkdown(text string) string {
	text = markdownLink.ReplaceAllString(text, "$1")
	text = strings.NewReplacer("**", "", "`", "").Replace(text)
	return strings.TrimSpace(extraSpaces.ReplaceAllString(text, " "))
}

// clipRunes обрезает текст по границе слова, не разрезая многобайтные символы.
func clipRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)[:limit]
	cut := string(runes)
	if i := strings.LastIndex(cut, " "); i > limit/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:—-") + "…"
}

func (s *Server) handleListGuides(w http.ResponseWriter, _ *http.Request) {
	writeList(w, guides().list)
}

func (s *Server) handleGetGuide(w http.ResponseWriter, r *http.Request) {
	catalog := guides()
	slug := strings.ToLower(r.PathValue("slug"))
	guide, ok := catalog.bySlug[slug]
	if !ok {
		// Поиск идёт по готовому словарю, а не по пути в файловой системе, так
		// что «../» в адресе ничего не даёт: такого ключа просто нет.
		s.writeError(w, r, fmt.Errorf("%w: руководство %q", store.ErrNotFound, slug))
		return
	}
	writeJSON(w, http.StatusOK, docGuideContent{docGuide: guide, Markdown: catalog.content[slug]})
}
