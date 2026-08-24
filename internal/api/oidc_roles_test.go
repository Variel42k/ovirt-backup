package api

import (
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func testOIDC() config.OIDCConfig {
	return config.OIDCConfig{
		RoleMapping: map[string]string{
			"virt-admins":    "admin",
			"virt-operators": "operator",
			"virt-readers":   "viewer",
		},
	}
}

// Старшая роль побеждает: членство в группе администраторов — это решение
// выдать администраторские права, и другие группы его не отменяют. Порядок
// элементов в токене на результат влиять не должен вовсе.
func TestOIDCRoleTakesTheStrongestGroup(t *testing.T) {
	cfg := testOIDC()
	for _, groups := range [][]string{
		{"virt-readers", "virt-admins", "virt-operators"},
		{"virt-admins", "virt-readers"},
		{"virt-operators", "virt-admins"},
	} {
		role, err := mapOIDCRole(cfg, groups)
		if err != nil {
			t.Fatalf("%v: %v", groups, err)
		}
		if role != model.RoleAdmin {
			t.Errorf("%v дало роль %q, ожидалась %q", groups, role, model.RoleAdmin)
		}
	}
}

// Каталоги пишут имена групп в разном регистре, а оператор переносит их руками.
// Расхождение в регистре не должно оборачиваться отказом во входе без внятной
// причины.
func TestOIDCRoleIgnoresCase(t *testing.T) {
	role, err := mapOIDCRole(testOIDC(), []string{"VIRT-Operators"})
	if err != nil {
		t.Fatalf("сопоставление: %v", err)
	}
	if role != model.RoleOperator {
		t.Errorf("роль %q, ожидалась %q", role, model.RoleOperator)
	}
}

// Неизвестный пользователь не получает прав молча. Отказ — правильный исход, и
// в нём должно быть видно, какие группы пришли: иначе разбор занимает полдня.
func TestOIDCUnmappedGroupsAreRefused(t *testing.T) {
	if _, err := mapOIDCRole(testOIDC(), []string{"домен-пользователи"}); err == nil {
		t.Fatal("вход разрешён без подходящей группы")
	} else if !strings.Contains(err.Error(), "домен-пользователи") {
		t.Errorf("в отказе не названы группы: %v", err)
	}

	if _, err := mapOIDCRole(testOIDC(), nil); err == nil {
		t.Error("вход разрешён при отсутствии групп в токене")
	}
}

// Группа может отображаться и в настраиваемую роль. До появления таких ролей
// перебор шёл только по трём встроенным, и своя роль из role_mapping молча
// проваливалась в default_role — то есть завести её было можно, а войти с ней
// нельзя.
func TestOIDCRoleAcceptsCustomRole(t *testing.T) {
	cfg := testOIDC()
	cfg.RoleMapping["backup-team"] = "backup-operator"

	role, err := mapOIDCRole(cfg, []string{"backup-team"})
	if err != nil {
		t.Fatalf("сопоставление: %v", err)
	}
	if role != model.Role("backup-operator") {
		t.Errorf("роль %q, ожидалась backup-operator", role)
	}
}

// Встроенная роль сильнее настраиваемой: членство в группе администраторов
// остаётся решением выдать администраторские права.
func TestOIDCBuiltinRoleWinsOverCustom(t *testing.T) {
	cfg := testOIDC()
	cfg.RoleMapping["backup-team"] = "backup-operator"

	role, err := mapOIDCRole(cfg, []string{"backup-team", "virt-admins"})
	if err != nil {
		t.Fatalf("сопоставление: %v", err)
	}
	if role != model.RoleAdmin {
		t.Errorf("роль %q, ожидалась %q", role, model.RoleAdmin)
	}
}

// Две своих роли сразу — выбор должен быть один и тот же при каждом входе.
//
// Прогоняется много раз намеренно: перебор map в Go случаен, и без сортировки
// человек получал бы то одну роль, то другую от входа к входу, без единого
// изменения в настройках. Один прогон такое пропускает.
func TestOIDCCustomRoleChoiceIsStable(t *testing.T) {
	cfg := testOIDC()
	cfg.RoleMapping["team-b"] = "zeta-role"
	cfg.RoleMapping["team-a"] = "alpha-role"

	for range 200 {
		role, err := mapOIDCRole(cfg, []string{"team-a", "team-b"})
		if err != nil {
			t.Fatalf("сопоставление: %v", err)
		}
		if role != model.Role("alpha-role") {
			t.Fatalf("выбор роли неустойчив: получено %q, ожидалось alpha-role", role)
		}
	}
}

// Умолчание — осознанный выбор администратора, и тогда оно применяется.
func TestOIDCDefaultRoleApplies(t *testing.T) {
	cfg := testOIDC()
	cfg.DefaultRole = string(model.RoleViewer)
	role, err := mapOIDCRole(cfg, []string{"кто-то-ещё"})
	if err != nil {
		t.Fatalf("сопоставление: %v", err)
	}
	if role != model.RoleViewer {
		t.Errorf("роль %q, ожидалась %q", role, model.RoleViewer)
	}
}
