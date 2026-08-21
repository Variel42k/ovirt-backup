package api

import (
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// A reference that quietly falls behind the code is worse than none: it teaches
// the operator to distrust it. Adding a backup type without explaining it is
// exactly how that starts, so the test fails instead.
func TestEveryBackupTypeIsDocumented(t *testing.T) {
	documented := map[string]backupTypeHelp{}
	for _, entry := range backupTypeHelpEntries() {
		documented[entry.Value] = entry
	}

	for _, bt := range model.AllBackupTypes() {
		entry, ok := documented[string(bt)]
		if !ok {
			t.Errorf("тип бэкапа %q нет в справке — добавьте его в backupTypeHelpEntries", bt)
			continue
		}
		if entry.Title != bt.Title() {
			t.Errorf("заголовок типа %q в справке (%q) разошёлся с моделью (%q)",
				bt, entry.Title, bt.Title())
		}
		for name, value := range map[string]string{
			"Summary":    entry.Summary,
			"HowItWorks": entry.HowItWorks,
			"Restore":    entry.Restore,
			"GoodFor":    entry.GoodFor,
		} {
			if value == "" {
				t.Errorf("у типа %q не заполнено поле %s", bt, name)
			}
		}
	}

	for value := range documented {
		if model.BackupType(value).Title() == value {
			t.Errorf("в справке описан несуществующий тип %q", value)
		}
	}
}

// A "see also" pointing at nothing is a dead end in the UI.
func TestRelatedArticlesExist(t *testing.T) {
	known := map[string]bool{}
	for _, a := range helpArticles() {
		known[a.ID] = true
	}

	for _, entry := range backupTypeHelpEntries() {
		for _, id := range entry.Related {
			if !known[id] {
				t.Errorf("тип %q ссылается на несуществующую статью %q", entry.Value, id)
			}
		}
	}
}

// Blocks are rendered by kind; an unknown kind would silently disappear.
func TestArticleBlocksAreRenderable(t *testing.T) {
	renderable := map[string]bool{
		"text": true, "list": true, "table": true, "flow": true, "layers": true, "note": true, "warning": true,
	}

	for _, a := range helpArticles() {
		if a.ID == "" || a.Title == "" || a.Summary == "" {
			t.Errorf("статья %+v заполнена не полностью", a)
		}
		if len(a.Blocks) == 0 {
			t.Errorf("статья %q пустая", a.ID)
		}
		for i, b := range a.Blocks {
			if !renderable[b.Kind] {
				t.Errorf("статья %q, блок %d: интерфейс не умеет рисовать вид %q", a.ID, i, b.Kind)
			}
			if b.Kind == "table" {
				if len(b.Columns) == 0 {
					t.Errorf("статья %q, блок %d: у таблицы нет заголовков", a.ID, i)
				}
				for r, row := range b.Rows {
					if len(row) != len(b.Columns) {
						t.Errorf("статья %q, блок %d, строка %d: %d ячеек при %d столбцах",
							a.ID, i, r, len(row), len(b.Columns))
					}
				}
			} else if b.Kind == "flow" || b.Kind == "layers" {
				if len(b.Steps) < 2 {
					t.Errorf("статья %q, блок %d: в схеме должно быть не меньше двух элементов", a.ID, i)
				}
				for step, value := range b.Steps {
					if value.Title == "" || value.Detail == "" {
						t.Errorf("статья %q, блок %d, шаг %d заполнен не полностью", a.ID, i, step)
					}
				}
			} else if b.Text == "" && len(b.Items) == 0 {
				t.Errorf("статья %q, блок %d: нет содержимого", a.ID, i)
			}
		}
	}
}

// The three questions this reference exists to answer must have an address the
// UI can link to.
func TestKeyTopicsArePresent(t *testing.T) {
	known := map[string]bool{}
	for _, a := range helpArticles() {
		known[a.ID] = true
	}
	for _, id := range []string{
		"backup-pipeline", "disk-layers", "disk-formats", "ovirt-data-path", "kvm-data-path", "consistency-levels",
		"changed-blocks", "quiesce", "hot-backup", "retention", "verify", "chains",
		"physical-copies", "storage-modes", "restore-new-vm", "file-backups", "managed-artifacts",
		"outside-alerts", "server-migration", "system-time-and-theme",
	} {
		if !known[id] {
			t.Errorf("в справке нет статьи %q", id)
		}
	}
}

func TestUserFacingHelpAvoidsUnexplainedRecoveryAcronyms(t *testing.T) {
	for _, entry := range backupTypeHelpEntries() {
		for _, text := range append([]string{entry.Title, entry.Summary, entry.HowItWorks}, entry.Requires...) {
			if strings.Contains(text, "CBT") || strings.Contains(text, "RPO") {
				t.Errorf("тип %q содержит внутреннее сокращение: %q", entry.Value, text)
			}
		}
	}
	for _, article := range helpArticles() {
		for _, block := range article.Blocks {
			for _, text := range append([]string{article.Title, article.Summary, block.Heading, block.Text}, block.Items...) {
				if strings.Contains(text, "CBT") || strings.Contains(text, "RPO") {
					t.Errorf("статья %q содержит внутреннее сокращение: %q", article.ID, text)
				}
			}
		}
	}
}
