package model

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Право, которого нет в каталоге, ведёт себя как отсутствующее: проверка
// сравнивает с каталогом и не находит совпадения. Роль при этом выглядит
// настроенной. Опечатка в списке встроенной роли — самый вероятный способ это
// получить.
func TestBuiltinRolesUseKnownPermissions(t *testing.T) {
	for _, role := range BuiltinRoles() {
		for _, p := range role.Permissions {
			if !ValidPermission(p) {
				t.Errorf("роль %q содержит неизвестное право %q", role.Name, p)
			}
		}
	}
}

// Ключи прав должны быть уникальны: два раздела с одинаковым ключом означают,
// что выдача права в одном месте молча открывает другое.
func TestPermissionKeysAreUnique(t *testing.T) {
	seen := map[Permission]string{}
	for _, section := range PermissionCatalog() {
		for _, info := range section.Permissions {
			if where, dup := seen[info.Key]; dup {
				t.Errorf("право %q объявлено дважды: в %q и в %q", info.Key, where, section.Key)
			}
			seen[info.Key] = section.Key
		}
	}
	if len(seen) != len(AllPermissions()) {
		t.Fatalf("каталог и список прав разошлись: %d против %d", len(seen), len(AllPermissions()))
	}
}

// Каждое право должно называть одно из трёх известных действий, а его ключ —
// начинаться с ключа своего раздела. Иначе список в интерфейсе группируется не
// туда, где право на самом деле действует.
func TestPermissionKeysMatchTheirSection(t *testing.T) {
	for _, section := range PermissionCatalog() {
		for _, info := range section.Permissions {
			want := Permission(section.Key + "." + info.Action)
			if info.Key != want {
				t.Errorf("право %q в разделе %q с действием %q: ожидался ключ %q",
					info.Key, section.Key, info.Action, want)
			}
			switch info.Action {
			case ActionRead, ActionWrite, ActionAdmin, ActionDisruptive:
			default:
				t.Errorf("право %q объявляет неизвестное действие %q", info.Key, info.Action)
			}
			if info.Title == "" {
				t.Errorf("право %q без названия: в редакторе ролей оно будет пустой строкой", info.Key)
			}
		}
	}
}

// Встроенные роли обязаны вкладываться одна в другую. Оператор, который
// чего-то не может из доступного наблюдателю, — это не роль, а недоразумение,
// и обнаруживается оно жалобой пользователя, а не проверкой.
func TestBuiltinRolesAreNested(t *testing.T) {
	byName := map[Role]RoleDefinition{}
	for _, r := range BuiltinRoles() {
		byName[r.Name] = r
	}

	for _, pair := range []struct{ wider, narrower Role }{
		{RoleAdmin, RoleOperator},
		{RoleOperator, RoleViewer},
	} {
		wider, narrower := byName[pair.wider], byName[pair.narrower]
		for _, p := range narrower.Permissions {
			if !wider.Has(p) {
				t.Errorf("%q не содержит право %q, которое есть у %q",
					pair.wider, p, pair.narrower)
			}
		}
	}

	// Администратор — единственная роль, которой положено всё. Право,
	// забытое в его списке, закрывает раздел вообще для всех.
	admin := byName[RoleAdmin]
	for _, p := range AllPermissions() {
		if !admin.Has(p) {
			t.Errorf("администратор не имеет права %q", p)
		}
	}
}

// Прежнее поведение ролей должно сохраниться дословно. Журнал службы, журнал
// аудита, параметры и аварийная готовность были закрыты от наблюдателя и
// оператора — переход на права не должен был этого изменить.
func TestBuiltinRolesPreservePreviousAccess(t *testing.T) {
	byName := map[Role]RoleDefinition{}
	for _, r := range BuiltinRoles() {
		byName[r.Name] = r
	}

	closedForNonAdmin := []Permission{
		PermAuditRead, PermLogsRead, PermLogsAdmin,
		PermSettingsRead, PermSettingsAdmin,
		PermDRRead, PermDRAdmin, PermUsersAdmin,
		PermServersAdmin, PermServersDisruptive, PermStoragesAdmin, PermJobsAdmin,
		PermAlertsAdmin, PermEngineConfigAdmin, PermFileBackupsAdmin,
	}
	for _, p := range closedForNonAdmin {
		if byName[RoleOperator].Has(p) {
			t.Errorf("оператор получил право %q, которого у него не было", p)
		}
		if byName[RoleViewer].Has(p) {
			t.Errorf("наблюдатель получил право %q, которого у него не было", p)
		}
	}

	for _, p := range []Permission{
		PermServersWrite, PermJobsWrite, PermBackupsWrite,
		PermStoragesWrite, PermFileBackupsWrite, PermAlertsWrite,
	} {
		if byName[RoleViewer].Has(p) {
			t.Errorf("наблюдатель получил право на изменение %q", p)
		}
	}
}

// TestDisruptiveActionsAgreeAcrossVocabularies ловит расхождение двух словарей
// имён: API инвентаря присылает "reset" и "fence", авто-восстановление знает
// те же операции как "vm_reset" и "host_fence". Разойдись они — право
// servers.disruptive закрыло бы один путь и оставило открытым второй.
func TestDisruptiveActionsAgreeAcrossVocabularies(t *testing.T) {
	if !DisruptiveVMAction("reset") {
		t.Error("сброс ВМ должен считаться разрушительным")
	}
	if !ActionVMReset.Disruptive() {
		t.Error("vm_reset должен считаться разрушительным")
	}
	if !DisruptiveHostAction("fence", "restart") {
		t.Error("фенсинг хоста должен считаться разрушительным")
	}
	if !ActionHostFence.Disruptive() {
		t.Error("host_fence должен считаться разрушительным")
	}

	// Опрос питания ничего не выключает и права на фенсинг не требует:
	// иначе диагностика закрыта для тех, кому она нужна, чтобы понять, звать
	// ли того, у кого право есть.
	if DisruptiveHostAction("fence", "status") {
		t.Error("опрос питания не должен требовать права на фенсинг")
	}

	for _, safe := range []string{"start", "shutdown", "stop", "suspend", "reboot", "migrate"} {
		if DisruptiveVMAction(safe) {
			t.Errorf("действие %q не должно требовать отдельного права", safe)
		}
	}
	for _, safe := range []string{"activate", "deactivate"} {
		if DisruptiveHostAction(safe, "") {
			t.Errorf("действие %q не должно требовать отдельного права", safe)
		}
	}
}

// TestOperatorCannotFence закрепляет смысл отдельного права: оператор бэкапов
// запускает и останавливает машины, но не обесточивает хост со всеми ВМ на нём.
func TestOperatorCannotFence(t *testing.T) {
	var operator, admin RoleDefinition
	for _, role := range BuiltinRoles() {
		switch role.Name {
		case RoleOperator:
			operator = role
		case RoleAdmin:
			admin = role
		}
	}

	if !operator.Has(PermServersWrite) {
		t.Fatal("оператор потерял управление ВМ — проверка бессмысленна")
	}
	if operator.Has(PermServersDisruptive) {
		t.Error("оператор не должен иметь права на фенсинг хоста")
	}
	if !admin.Has(PermServersDisruptive) {
		t.Error("администратор должен сохранить право на фенсинг")
	}
}

// docs/ROLES.md — таблица, по которой раздают доступ. Право, добавленное в
// каталог и забытое в ней, делает документ тихо неполным: администратор
// раздаёт права по таблице и о новом просто не узнает. Обратное так же плохо:
// строка про удалённое право обещает доступ, которого нет.
func TestRolesDocMatchesCatalog(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "ROLES.md"))
	if err != nil {
		t.Fatalf("чтение docs/ROLES.md: %v", err)
	}
	doc := string(body)

	// Сверяется раздел «Матрица», а не весь файл: за его пределами в тех же
	// обратных кавычках стоят настройки вида management.enabled, которые
	// выглядят как право, но им не являются.
	const head = "## Матрица"
	matrix := doc[strings.Index(doc, head):]
	if end := strings.Index(matrix[len(head):], "\n## "); end >= 0 {
		matrix = matrix[:end+len(head)]
	}

	const bt = "`"
	mentioned := map[string]bool{}
	for _, m := range regexp.MustCompile(bt+`([a-z_]+\.[a-z]+)`+bt).FindAllStringSubmatch(matrix, -1) {
		mentioned[m[1]] = true
	}

	known := map[string]bool{}
	for _, p := range AllPermissions() {
		known[string(p)] = true
		if !mentioned[string(p)] {
			t.Errorf("право %s есть в каталоге, но не описано в docs/ROLES.md", p)
		}
	}
	for name := range mentioned {
		if !known[name] {
			t.Errorf("docs/ROLES.md описывает право %s, которого нет в каталоге", name)
		}
	}

	// Итоговые числа в документе набраны руками и пересчёту глазами не
	// подлежат: роль, у которой прав стало больше, а строка осталась прежней,
	// вводит в заблуждение ровно того, кто по этой строке принимает решение.
	for _, role := range BuiltinRoles() {
		want := fmt.Sprintf("%d", len(role.Permissions))
		if !strings.Contains(matrix, want) {
			t.Errorf("в разделе «Матрица» нет числа прав роли %s (%s)", role.Name, want)
		}
	}
}
