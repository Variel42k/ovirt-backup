package model

// AlertAudience — кому адресовано оповещение.
//
// Делить их понадобилось потому, что одна общая лента одинаково будит и того,
// кто отвечает за бэкапы, и того, кто отвечает за гипервизоры. Человек,
// которому девять из десяти сообщений не адресованы, перестаёт читать все
// десять — и пропускает то единственное, что касалось его.
//
// Адресат выводится из типа оповещения и в базе не хранится. Колонка означала
// бы, что у старых записей она пуста, а у новых заполнена, и любое изменение
// раскладки требовало бы миграции. Тип известен всегда, раскладка — здесь.
type AlertAudience string

const (
	// AudienceBackup — то, что чинит оператор резервного копирования: задание
	// не прошло, копия устарела, проверка провалилась.
	AudienceBackup AlertAudience = "backup"
	// AudienceInfrastructure — состояние чужой инфраструктуры: движок, хосты,
	// виртуальные машины, домены хранения. Чинит это администратор
	// виртуализации, и чаще всего не тот же человек.
	AudienceInfrastructure AlertAudience = "infrastructure"
	// AudienceService — состояние самой службы резервного копирования: её
	// хранилища копий, место в них, готовность к восстановлению.
	AudienceService AlertAudience = "service"
	// AudienceSecurity — доступ и согласование: заявки на опасные действия,
	// выполнение в обход согласования.
	//
	// Отдельно от службы намеренно. За место в хранилище и за то, кто удаляет
	// копии, отвечают разные люди, и смешивать эти два потока — значит хоронить
	// сообщение о попытке удалить хранилище среди предупреждений о свободном
	// месте.
	AudienceSecurity AlertAudience = "security"
)

// AudienceInfo описывает адресата для интерфейса.
type AudienceInfo struct {
	Key         AlertAudience `json:"key"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
}

// AlertAudiences перечисляет адресатов в порядке показа.
func AlertAudiences() []AudienceInfo {
	return []AudienceInfo{
		{AudienceBackup, "Резервное копирование",
			"Задания, копии и проверки — то, что чинит оператор бэкапов"},
		{AudienceInfrastructure, "Инфраструктура",
			"Движки, хосты, виртуальные машины и домены хранения — зона администратора виртуализации"},
		{AudienceService, "Служба",
			"Хранилища копий, свободное место и аварийная готовность самой службы"},
		{AudienceSecurity, "Доступ и согласование",
			"Заявки на опасные действия и выполнение в обход согласования"},
	}
}

// alertAudienceByKind — раскладка типов оповещений по адресатам.
//
// Перечислено поимённо, а не выведено по префиксу имени. Префикс обманчив:
// storage_domain_low_space — это место в домене хранения гипервизора, а
// storage_capacity_low — место в хранилище копий, и чинят их разные люди в
// разных системах, хотя оба начинаются на storage.
var alertAudienceByKind = map[string]AlertAudience{
	AlertBackupFailed:         AudienceBackup,
	AlertBackupUnprotected:    AudienceBackup,
	AlertBackupStale:          AudienceBackup,
	AlertBackupReplicaFailed:  AudienceBackup,
	AlertBackupVerifyStale:    AudienceBackup,
	AlertBackupPerformance:    AudienceBackup,
	AlertBackupScheduleMissed: AudienceBackup,
	AlertVerifyFailed:         AudienceBackup,
	AlertCBTUnavailable:       AudienceBackup,

	AlertEngineUnreachable:   AudienceInfrastructure,
	AlertHostNonResponsive:   AudienceInfrastructure,
	AlertHostDown:            AudienceInfrastructure,
	AlertHostMaintenance:     AudienceInfrastructure,
	AlertVMDown:              AudienceInfrastructure,
	AlertVMPaused:            AudienceInfrastructure,
	AlertVMUnknown:           AudienceInfrastructure,
	AlertStorageDomainDown:   AudienceInfrastructure,
	AlertStorageDomainFull:   AudienceInfrastructure,
	AlertStoragePathDegraded: AudienceInfrastructure,
	AlertDiskIOErrors:        AudienceInfrastructure,

	AlertStorageTargetDown:    AudienceService,
	AlertStorageCapacityLow:   AudienceService,
	AlertStorageCapacityTrend: AudienceService,
	AlertDRNotReady:           AudienceService,

	AlertInsecureTLSExpired: AudienceSecurity,

	AlertApprovalPending: AudienceSecurity,
	AlertApprovalFailed:  AudienceSecurity,
	AlertBreakGlassUsed:  AudienceSecurity,
}

// AudienceOf возвращает адресата оповещения.
//
// Неизвестный тип достаётся службе, а не теряется: новый вид оповещения
// добавляют вместе с кодом, который его поднимает, и забыть про раскладку
// легко. Пусть лучше он попадёт не в ту ленту, чем не попадёт ни в одну.
func AudienceOf(kind string) AlertAudience {
	if audience, ok := alertAudienceByKind[kind]; ok {
		return audience
	}
	return AudienceService
}

// KnownAlertKinds перечисляет типы оповещений, у которых есть адресат.
func KnownAlertKinds() []string {
	kinds := make([]string, 0, len(alertAudienceByKind))
	for kind := range alertAudienceByKind {
		kinds = append(kinds, kind)
	}
	return kinds
}
