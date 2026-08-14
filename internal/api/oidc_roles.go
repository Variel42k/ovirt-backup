package api

import (
	"fmt"
	"strings"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/model"
)

// roleOrder ranks roles from most to least powerful.
//
// Пользователь обычно состоит в нескольких группах, и они отображаются в разные
// роли. Брать первую попавшуюся — значит выдавать права по случайному порядку
// элементов в токене: сегодня администратор, завтра наблюдатель. Берём старшую,
// потому что членство в группе администраторов — это и есть решение выдать
// администраторские права, а остальные группы его не отменяют.
var roleOrder = []model.Role{model.RoleAdmin, model.RoleOperator, model.RoleViewer}

// mapOIDCRole turns the groups from the token into the role of the session.
//
// Пустой результат — не ошибка настройки, а отказ во входе: пользователь
// провайдеру известен, но этой системе не назначен. Молча подставлять роль
// нельзя — это система, которая управляет чужими виртуальными машинами и
// хранит ключ шифрования копий.
func mapOIDCRole(cfg config.OIDCConfig, groups []string) (model.Role, error) {
	// Сопоставление без учёта регистра: каталоги пишут группы по-разному, а
	// оператор берёт их из своей консоли и переносит руками. Расхождение в
	// регистре здесь означало бы отказ во входе с необъяснимой причиной.
	mapped := map[model.Role]bool{}
	for _, group := range groups {
		for pattern, role := range cfg.RoleMapping {
			if strings.EqualFold(strings.TrimSpace(pattern), strings.TrimSpace(group)) {
				mapped[model.Role(role)] = true
			}
		}
	}

	for _, role := range roleOrder {
		if mapped[role] {
			return role, nil
		}
	}

	if cfg.DefaultRole != "" {
		return model.Role(cfg.DefaultRole), nil
	}

	// Причина называется целиком: разбирать отказ входа по журналу провайдера и
	// по журналу службы одновременно — занятие на полдня.
	return "", fmt.Errorf("группы пользователя (%s) не отображаются ни в одну роль, "+
		"а auth.oidc.default_role не задан", describeGroups(groups))
}

func describeGroups(groups []string) string {
	if len(groups) == 0 {
		return "их нет в токене"
	}
	return strings.Join(groups, ", ")
}
