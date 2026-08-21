package model

// Permission — одно право вида «раздел.действие».
//
// Разделы совпадают с пунктами меню намеренно. Администратор, раздающий права,
// думает не о маршрутах HTTP, а о том, что человек видит на экране; список,
// собранный по внутреннему устройству API, пришлось бы каждый раз мысленно
// переводить в «а что он сможет открыть».
type Permission string

// Действия. Их три, и добавлять четвёртое не следует без крайней нужды: любая
// новая ступень умножается на число разделов и превращает выдачу прав в
// занятие, требующее сосредоточенности.
//
//	read  — видеть раздел и его содержимое;
//	write — выполнять повседневные действия: запустить, отменить, подтвердить;
//	admin — менять то, что настраивается однажды: подключения, хранилища,
//	        расписания репликации, параметры службы.
const (
	ActionRead  = "read"
	ActionWrite = "write"
	ActionAdmin = "admin"
)

// Разделы.
const (
	SectionServers      = "servers"
	SectionJobs         = "jobs"
	SectionBackups      = "backups"
	SectionStorages     = "storages"
	SectionFileBackups  = "file_backups"
	SectionEngineConfig = "engine_config"
	SectionMonitoring   = "monitoring"
	SectionAlerts       = "alerts"
	SectionDR           = "dr"
	SectionLogs         = "logs"
	SectionSettings     = "settings"
	SectionUsers        = "users"
	SectionAudit        = "audit"
)

// Права. Перечислены явно, а не собираются склейкой строк: опечатка в
// склеенном ключе превращается в право, которого нет ни у кого, и проверка
// молча запрещает всё подряд.
const (
	PermServersRead  Permission = "servers.read"
	PermServersWrite Permission = "servers.write"
	PermServersAdmin Permission = "servers.admin"

	PermJobsRead  Permission = "jobs.read"
	PermJobsWrite Permission = "jobs.write"
	PermJobsAdmin Permission = "jobs.admin"

	PermBackupsRead  Permission = "backups.read"
	PermBackupsWrite Permission = "backups.write"

	PermStoragesRead  Permission = "storages.read"
	PermStoragesWrite Permission = "storages.write"
	PermStoragesAdmin Permission = "storages.admin"

	PermFileBackupsRead  Permission = "file_backups.read"
	PermFileBackupsWrite Permission = "file_backups.write"
	PermFileBackupsAdmin Permission = "file_backups.admin"

	PermEngineConfigRead  Permission = "engine_config.read"
	PermEngineConfigAdmin Permission = "engine_config.admin"

	PermMonitoringRead Permission = "monitoring.read"

	PermAlertsRead  Permission = "alerts.read"
	PermAlertsWrite Permission = "alerts.write"
	PermAlertsAdmin Permission = "alerts.admin"

	PermDRRead  Permission = "dr.read"
	PermDRAdmin Permission = "dr.admin"

	PermLogsRead  Permission = "logs.read"
	PermLogsAdmin Permission = "logs.admin"

	PermSettingsRead  Permission = "settings.read"
	PermSettingsAdmin Permission = "settings.admin"

	PermUsersAdmin Permission = "users.admin"
	PermAuditRead  Permission = "audit.read"
)

// PermissionInfo описывает одно право для того, кто его выдаёт.
type PermissionInfo struct {
	Key    Permission `json:"key"`
	Action string     `json:"action"`
	Title  string     `json:"title"`
	Hint   string     `json:"hint,omitempty"`
}

// SectionInfo — раздел со своими правами.
type SectionInfo struct {
	Key         string           `json:"key"`
	Title       string           `json:"title"`
	Permissions []PermissionInfo `json:"permissions"`
}

// permissionCatalog — единственный источник правды о правах.
//
// Из него собираются и проверка на маршруте, и список для интерфейса, и
// подсказки в редакторе ролей. Второй список рядом разошёлся бы с первым молча
// и ровно в том месте, где право что-то закрывает.
var permissionCatalog = []SectionInfo{
	{Key: SectionServers, Title: "Серверы", Permissions: []PermissionInfo{
		{PermServersRead, ActionRead, "Видеть подключения и инвентарь",
			"Список серверов, кластеров, хостов, ВМ и дисков"},
		{PermServersWrite, ActionWrite, "Управлять ВМ и хостами",
			"Пуск, остановка, миграция, теги, режим бэкапа диска, обновление инвентаря"},
		{PermServersAdmin, ActionAdmin, "Настраивать подключения",
			"Добавление и удаление движков, пароли и сертификаты"},
	}},
	{Key: SectionJobs, Title: "Задания бэкапа", Permissions: []PermissionInfo{
		{PermJobsRead, ActionRead, "Видеть задания", ""},
		{PermJobsWrite, ActionWrite, "Создавать и запускать задания",
			"Правка расписаний, запуск вне очереди, удаление"},
		{PermJobsAdmin, ActionAdmin, "Управлять репликацией",
			"Включение репликации и смена основной копии"},
	}},
	{Key: SectionBackups, Title: "Бэкапы", Permissions: []PermissionInfo{
		{PermBackupsRead, ActionRead, "Видеть копии и восстановления", ""},
		{PermBackupsWrite, ActionWrite, "Восстанавливать, проверять, удалять",
			"Сюда же входит ретенция и отмена выполняющихся операций"},
	}},
	{Key: SectionStorages, Title: "Хранилища", Permissions: []PermissionInfo{
		{PermStoragesRead, ActionRead, "Видеть хранилища", ""},
		{PermStoragesWrite, ActionWrite, "Проверять доступность", ""},
		{PermStoragesAdmin, ActionAdmin, "Настраивать хранилища",
			"Учётные данные, ключи шифрования, сканирование каталога"},
	}},
	{Key: SectionFileBackups, Title: "Файловые бэкапы", Permissions: []PermissionInfo{
		{PermFileBackupsRead, ActionRead, "Видеть задания и копии", ""},
		{PermFileBackupsWrite, ActionWrite, "Запускать и восстанавливать", ""},
		{PermFileBackupsAdmin, ActionAdmin, "Настраивать задания", ""},
	}},
	{Key: SectionEngineConfig, Title: "Конфигурация Engine", Permissions: []PermissionInfo{
		{PermEngineConfigRead, ActionRead, "Видеть снимки конфигурации", ""},
		{PermEngineConfigAdmin, ActionAdmin, "Снимать и настраивать", ""},
	}},
	{Key: SectionMonitoring, Title: "Обзор и защита", Permissions: []PermissionInfo{
		{PermMonitoringRead, ActionRead, "Видеть обзор, покрытие и показатели", ""},
	}},
	{Key: SectionAlerts, Title: "Оповещения", Permissions: []PermissionInfo{
		{PermAlertsRead, ActionRead, "Видеть оповещения", ""},
		{PermAlertsWrite, ActionWrite, "Подтверждать и заглушать", ""},
		{PermAlertsAdmin, ActionAdmin, "Настраивать доставку",
			"Каналы, пороги, повторы, авто-восстановление"},
	}},
	{Key: SectionDR, Title: "Аварийная готовность", Permissions: []PermissionInfo{
		{PermDRRead, ActionRead, "Видеть готовность", ""},
		{PermDRAdmin, ActionAdmin, "Запускать проверку", ""},
	}},
	{Key: SectionLogs, Title: "Журнал службы", Permissions: []PermissionInfo{
		{PermLogsRead, ActionRead, "Читать журнал",
			"Журнал перечисляет все ВМ, хосты и операторов установки"},
		{PermLogsAdmin, ActionAdmin, "Менять уровень и ротацию", ""},
	}},
	{Key: SectionSettings, Title: "Настройки", Permissions: []PermissionInfo{
		{PermSettingsRead, ActionRead, "Видеть параметры службы", ""},
		{PermSettingsAdmin, ActionAdmin, "Менять параметры службы",
			"Сжатие, часовой пояс, ротация журнала, пороги качества"},
	}},
	{Key: SectionUsers, Title: "Пользователи и доступ", Permissions: []PermissionInfo{
		{PermUsersAdmin, ActionAdmin, "Управлять учётными записями, ролями и токенами",
			"Право выдавать права. Кто его имеет, может выдать себе любое другое"},
	}},
	{Key: SectionAudit, Title: "Аудит", Permissions: []PermissionInfo{
		{PermAuditRead, ActionRead, "Читать журнал аудита",
			"Кто и что менял, включая неудачные попытки входа"},
	}},
}

// PermissionCatalog возвращает разделы и права для интерфейса.
func PermissionCatalog() []SectionInfo { return permissionCatalog }

// AllPermissions возвращает все существующие права.
func AllPermissions() []Permission {
	out := make([]Permission, 0, 32)
	for _, section := range permissionCatalog {
		for _, p := range section.Permissions {
			out = append(out, p.Key)
		}
	}
	return out
}

// HasAnyAction сообщает, есть ли в наборе хоть одно право с таким действием.
//
// Нужна для полей can_write и can_administer в /auth/me. Считать их по имени
// роли, как было до настраиваемых ролей, больше нельзя: у своей роли имя
// произвольное, и сравнение с «admin» дало бы ложь для любой из них.
func HasAnyAction(perms []Permission, action string) bool {
	for _, section := range permissionCatalog {
		for _, info := range section.Permissions {
			if info.Action != action {
				continue
			}
			for _, own := range perms {
				if own == info.Key {
					return true
				}
			}
		}
	}
	return false
}

// ValidPermission сообщает, существует ли такое право.
//
// Нужна затем, что роль с несуществующим правом выглядит настроенной, а
// работает как роль без него: проверка сравнивает с каталогом и не находит
// совпадения. Отказывать надо при сохранении роли, а не при обращении.
func ValidPermission(p Permission) bool {
	for _, section := range permissionCatalog {
		for _, info := range section.Permissions {
			if info.Key == p {
				return true
			}
		}
	}
	return false
}
