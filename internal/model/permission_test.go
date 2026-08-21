package model

import "testing"

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
			case ActionRead, ActionWrite, ActionAdmin:
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
		PermServersAdmin, PermStoragesAdmin, PermJobsAdmin,
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
