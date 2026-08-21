package model

import "time"

// RoleDefinition — роль как именованный набор прав.
//
// Имя роли (Name) — то же значение, что лежит в users.role и в сессии. Поэтому
// добавление своих ролей ничего не ломает в уже выданных сессиях: они ссылаются
// на роль по имени, а набор прав берётся при проверке.
type RoleDefinition struct {
	ID   string `json:"id"`
	Name Role   `json:"name"`
	// Title и Description — для человека, который раздаёт права. Имя роли
	// латиницей нужно API и внешнему провайдеру, а в списке пользователей
	// читается заголовок.
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Builtin — роль, которая была всегда. Её нельзя удалить и нельзя
	// переименовать: на неё ссылаются существующие учётные записи, настройки
	// сопоставления групп провайдера и чужие скрипты.
	Builtin     bool         `json:"builtin"`
	Permissions []Permission `json:"permissions"`
	CreatedAt   time.Time    `json:"created_at,omitzero"`
	UpdatedAt   time.Time    `json:"updated_at,omitzero"`
}

// Has сообщает, входит ли право в роль.
func (r RoleDefinition) Has(p Permission) bool {
	for _, own := range r.Permissions {
		if own == p {
			return true
		}
	}
	return false
}

// viewerPermissions — то, что видел наблюдатель до появления ролей.
//
// Список задан поимённо, а не «все права на чтение». Разница существенная:
// журнал службы, журнал аудита, параметры службы и аварийная готовность были
// закрыты от наблюдателя и раньше. Собери этот набор по действию — и переход
// на роли молча раздал бы наблюдателям доступ к журналу, где перечислены все
// ВМ, хосты и операторы установки.
var viewerPermissions = []Permission{
	PermServersRead,
	PermJobsRead,
	PermBackupsRead,
	PermStoragesRead,
	PermFileBackupsRead,
	PermEngineConfigRead,
	PermMonitoringRead,
	PermAlertsRead,
}

// operatorPermissions — наблюдатель плюс повседневные действия.
var operatorPermissions = append(append([]Permission{}, viewerPermissions...),
	PermServersWrite,
	PermJobsWrite,
	PermBackupsWrite,
	PermStoragesWrite,
	PermFileBackupsWrite,
	PermAlertsWrite,
)

// BuiltinRoles возвращает роли, существовавшие до настраиваемых.
//
// Набор прав каждой в точности повторяет то, что эти роли могли раньше. Это не
// дань совместимости ради совместимости: установка, обновившаяся до версии с
// ролями, не должна ни у кого отобрать доступ и тем более никому его добавить.
func BuiltinRoles() []RoleDefinition {
	return []RoleDefinition{
		{
			Name: RoleAdmin, Title: "Администратор", Builtin: true,
			Description: "Полный доступ, включая выдачу прав",
			Permissions: AllPermissions(),
		},
		{
			Name: RoleOperator, Title: "Оператор", Builtin: true,
			Description: "Управление ВМ и бэкапами без правки подключений и хранилищ",
			Permissions: operatorPermissions,
		},
		{
			Name: RoleViewer, Title: "Наблюдатель", Builtin: true,
			Description: "Только чтение",
			Permissions: viewerPermissions,
		},
	}
}

// IsBuiltinRole сообщает, встроенная ли это роль.
func IsBuiltinRole(name Role) bool {
	switch name {
	case RoleAdmin, RoleOperator, RoleViewer:
		return true
	default:
		return false
	}
}
