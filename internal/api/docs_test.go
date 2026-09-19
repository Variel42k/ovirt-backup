package api

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Variel42k/ovirt-backup/docs"
)

// Новое руководство без места в оглавлении всё равно появится — в «Прочем».
// Но оказаться там оно должно по решению, а не по забывчивости, поэтому тест
// требует явной раскладки. Запись о несуществующем файле — тоже ошибка: значит,
// файл переименовали, а оглавление осталось со старым именем.
func TestEveryEmbeddedGuideIsPlaced(t *testing.T) {
	names, err := fs.Glob(docs.Files, "*.md")
	if err != nil || len(names) == 0 {
		t.Fatalf("в сборку не попало ни одного руководства: %v", err)
	}
	present := map[string]bool{}
	for _, name := range names {
		present[name] = true
		placement, ok := guidePlacements[name]
		if !ok {
			t.Errorf("руководство %s не разложено по разделам — добавьте его в guidePlacements", name)
			continue
		}
		if _, ok := guideCategoryRank[placement.category]; !ok {
			t.Errorf("руководство %s отнесено к неизвестному разделу %q", name, placement.category)
		}
		if placement.icon == "" {
			t.Errorf("у руководства %s не задан значок", name)
		}
	}
	for name := range guidePlacements {
		if !present[name] {
			t.Errorf("в раскладке есть %s, но такого файла в docs/ нет", name)
		}
	}
}

// Оглавление показывает заголовок и первый абзац. Пустое место или сырые
// звёздочки со ссылками в карточке выглядят как поломка, а не как текст.
func TestGuideCatalogDescribesEveryGuide(t *testing.T) {
	catalog := guides()
	names, _ := fs.Glob(docs.Files, "*.md")
	if len(catalog.list) != len(names) {
		t.Fatalf("в оглавлении %d руководств, а файлов %d", len(catalog.list), len(names))
	}
	seen := map[string]bool{}
	for _, guide := range catalog.list {
		if seen[guide.Slug] {
			t.Errorf("идентификатор %q повторяется", guide.Slug)
		}
		seen[guide.Slug] = true
		if guide.Title == "" || guide.Title == strings.TrimSuffix(guide.File, ".md") {
			t.Errorf("%s: нет заголовка первого уровня", guide.File)
		}
		if guide.Summary == "" {
			t.Errorf("%s: нет вводного абзаца под заголовком", guide.File)
		}
		if strings.Contains(guide.Summary, "](") || strings.Contains(guide.Summary, "**") {
			t.Errorf("%s: в кратком описании осталась разметка: %q", guide.File, guide.Summary)
		}
		if guide.Words == 0 {
			t.Errorf("%s: не посчитан объём", guide.File)
		}
		if catalog.content[guide.Slug] == "" {
			t.Errorf("%s: нет текста", guide.File)
		}
	}
}

func TestGuideOutlineSkipsCodeAndMarkup(t *testing.T) {
	files := fstest.MapFS{
		"SAMPLE.md": {Data: []byte("# Пример `руководства`\n\n" +
			"```bash\nignored words here\n```\n\n" +
			"Первый **абзац** со [ссылкой](OTHER.md#якорь)\nи продолжением.\n\n" +
			"Второй абзац в описание не попадает.\n")},
	}
	catalog := buildGuideCatalog(files)
	guide, ok := catalog.bySlug["sample"]
	if !ok {
		t.Fatal("руководство не попало в оглавление")
	}
	if guide.Title != "Пример руководства" {
		t.Errorf("заголовок = %q", guide.Title)
	}
	if guide.Summary != "Первый абзац со ссылкой и продолжением." {
		t.Errorf("описание = %q", guide.Summary)
	}
	if guide.Category != otherGuidesCategory {
		t.Errorf("неразложенный файл должен попасть в %q, а попал в %q", otherGuidesCategory, guide.Category)
	}
	// Слова из блока кода в оценку времени чтения не входят: 3 в заголовке,
	// 6 в первом абзаце и 6 во втором.
	if guide.Words != 15 {
		t.Errorf("слов = %d, ожидалось 15", guide.Words)
	}
}

// Вводный абзац, оборванный двоеточием, без следующего за ним списка выглядит на
// карточке как поломка — пункты должны попасть в описание.
func TestGuideOutlineIncludesListAfterColon(t *testing.T) {
	files := fstest.MapFS{
		"LIST.md": {Data: []byte("# Список\n\nВарианты:\n\n1. первый вариант;\n2. второй,\n   с продолжением.\n\nДальше уже не описание.\n")},
	}
	guide := buildGuideCatalog(files).bySlug["list"]
	want := "Варианты: первый вариант; второй, с продолжением."
	if guide.Summary != want {
		t.Errorf("описание = %q, ожидалось %q", guide.Summary, want)
	}
}

func TestClipRunesKeepsWholeWordsAndCharacters(t *testing.T) {
	text := strings.Repeat("слово ", 60)
	clipped := clipRunes(text, 50)
	if !strings.HasSuffix(clipped, "…") {
		t.Fatalf("обрезанный текст должен заканчиваться многоточием: %q", clipped)
	}
	if strings.HasSuffix(strings.TrimSuffix(clipped, "…"), "сло") {
		t.Fatalf("слово разрезано посередине: %q", clipped)
	}
	if got := clipRunes("коротко", 50); got != "коротко" {
		t.Fatalf("короткий текст изменён: %q", got)
	}
}

func TestGuideHandlers(t *testing.T) {
	srv := &Server{}

	rec := httptest.NewRecorder()
	srv.handleListGuides(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /docs = %d", rec.Code)
	}
	var list listResponse[docGuide]
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) == 0 {
		t.Fatalf("оглавление не разобралось: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/docs/DEPLOY", nil)
	req.SetPathValue("slug", "DEPLOY")
	rec = httptest.NewRecorder()
	srv.handleGetGuide(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /docs/DEPLOY = %d", rec.Code)
	}
	var guide docGuideContent
	if err := json.Unmarshal(rec.Body.Bytes(), &guide); err != nil {
		t.Fatal(err)
	}
	if guide.Slug != "deploy" || !strings.HasPrefix(guide.Markdown, "# ") {
		t.Fatalf("руководство отдано не целиком: slug=%q, начало=%q", guide.Slug, guide.Markdown[:min(20, len(guide.Markdown))])
	}

	// Только встроенные руководства: ни исходник пакета, ни путь наружу.
	for _, slug := range []string{"embed", "../go", "..%2fgo", "readme", ""} {
		req := httptest.NewRequest(http.MethodGet, "/docs/x", nil)
		req.SetPathValue("slug", slug)
		rec := httptest.NewRecorder()
		srv.handleGetGuide(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("slug %q: ожидался 404, получен %d", slug, rec.Code)
		}
	}
}
