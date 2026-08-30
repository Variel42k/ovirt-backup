package api

import (
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Назначение без права — открытый список каталогов хоста для любого, кто вошёл.
// Обработчик один на все назначения, поэтому забыть право при добавлении нового
// легко, а заметить нечем: запрос просто начнёт отвечать.
func TestEveryBrowseScopeRequiresAPermission(t *testing.T) {
	known := map[model.Permission]bool{}
	for _, perm := range model.AllPermissions() {
		known[perm] = true
	}

	for scope, rule := range browseScopes() {
		if rule.permission == "" {
			t.Errorf("назначение %q не требует никакого права", scope)
			continue
		}
		if !known[rule.permission] {
			t.Errorf("назначение %q требует несуществующего права %q", scope, rule.permission)
		}
		if rule.roots == nil {
			t.Errorf("у назначения %q не задан набор корней", scope)
		}
		// Пустой список корней оператор увидит как пустое окно. Без объяснения
		// он не поймёт, сломалось что-то или так задумано, и пойдёт искать
		// несуществующую поломку.
		if rule.emptyHint == "" {
			t.Errorf("назначение %q не объясняет пустой список корней", scope)
		}
	}
}

// Выбор каталога — не редактирование: право на чтение списка должно быть тем
// же, что и на настройку, ради которой каталог выбирают. Иначе смотреть в
// файловую систему хоста сможет тот, кому нельзя ничего менять.
func TestBrowseScopesUseAdminOrWritePermissions(t *testing.T) {
	expected := map[string]model.Permission{
		scopeStorage:     model.PermStoragesAdmin,
		scopeFileBackup:  model.PermJobsAdmin,
		scopeFileRestore: model.PermBackupsWrite,
		scopeRestore:     model.PermBackupsWrite,
	}

	scopes := browseScopes()
	if len(scopes) != len(expected) {
		t.Fatalf("назначений %d, а описано %d: новое добавлено без разбора прав",
			len(scopes), len(expected))
	}
	for scope, want := range expected {
		rule, ok := scopes[scope]
		if !ok {
			t.Errorf("назначение %q пропало", scope)
			continue
		}
		if rule.permission != want {
			t.Errorf("назначение %q требует %q, ожидалось %q", scope, rule.permission, want)
		}
	}
}
