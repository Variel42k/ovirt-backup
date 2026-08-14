package api

import (
	"strings"
	"testing"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/model"
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
