package model

// Consistency — насколько согласовано состояние гостя в момент точки.
//
// Уровни упорядочены: копия более высокого уровня годится везде, где
// достаточно более низкого. Сама копия дисков от уровня не меняется — меняется
// только то, что происходило в госте в момент, когда точка фиксировалась.
type Consistency string

const (
	// ConsistencyCrash — как после выключения питания. Журналируемые ФС и СУБД
	// восстанавливаются по журналу при запуске.
	ConsistencyCrash Consistency = "crash"
	// ConsistencyFilesystem — файловые системы заморожены агентом: метаданные
	// ФС целы, но СУБД всё равно проходит восстановление при старте.
	ConsistencyFilesystem Consistency = "filesystem"
	// ConsistencyApplication — перед заморозкой сценарии fsfreeze-hook
	// перевели СУБД в согласованное состояние (сброс буферов, блокировка
	// записи), в Windows — VSS-писатели.
	ConsistencyApplication Consistency = "application"
)

// FreezeBy — кто замораживает гостя для уровней filesystem и application.
//
// Служба (service) замораживает гостя сама до запроса бэкапа и держит
// заморозку, пока гипервизор не зафиксирует точку. На oVirt это вся фаза
// initializing — подготовка scratch-дисков или снапшота, десятки секунд.
//
// Движок (engine) — только для oVirt через Backup API: служба передаёт
// require_consistency, и движок замораживает гостя сам, на доли секунды
// вокруг фиксации точки. Сценарии fsfreeze-hook в госте вызываются так же:
// их запускает гостевой агент, кто бы ни попросил заморозку.
//
// Смешанный (mixed) — тоже только oVirt: замораживает служба, а движок
// подключается, если служба не смогла заморозить гостя или не уложилась в
// предел заморозки. Во втором случае режим запоминает, что на этой ВМ служба
// не справляется, и какое-то время сразу отдаёт заморозку движку.
type FreezeBy string

const (
	FreezeByService FreezeBy = "service"
	FreezeByEngine  FreezeBy = "engine"
	FreezeByMixed   FreezeBy = "mixed"
)

// Valid сообщает, известно ли значение; пусто — служба, как до появления выбора.
func (f FreezeBy) Valid() bool {
	return f == "" || f == FreezeByService || f == FreezeByEngine || f == FreezeByMixed
}

// Engine сообщает, что заморозку выполняет движок.
func (f FreezeBy) Engine() bool { return f == FreezeByEngine }

// Mixed сообщает, что замораживает служба, а движок её подстраховывает.
func (f FreezeBy) Mixed() bool { return f == FreezeByMixed }

// NeedsEngine сообщает, что режиму нужна заморозка силами движка — она есть
// только у Backup API oVirt и его производных.
func (f FreezeBy) NeedsEngine() bool { return f == FreezeByEngine || f == FreezeByMixed }

// Valid сообщает, известен ли уровень.
func (c Consistency) Valid() bool {
	switch c {
	case ConsistencyCrash, ConsistencyFilesystem, ConsistencyApplication:
		return true
	}
	return false
}

// NeedsFreeze сообщает, требует ли уровень заморозки гостя.
func (c Consistency) NeedsFreeze() bool {
	return c == ConsistencyFilesystem || c == ConsistencyApplication
}

// Rank упорядочивает уровни; неизвестный уровень ниже любого известного.
func (c Consistency) Rank() int {
	switch c {
	case ConsistencyCrash:
		return 1
	case ConsistencyFilesystem:
		return 2
	case ConsistencyApplication:
		return 3
	}
	return 0
}

// Below сообщает, что достигнутый уровень c ниже заявленного target.
//
// Неизвестный достигнутый уровень ниже не считается: у точек, снятых до
// появления уровней, его просто нет, и тревожить из-за них незачем.
func (c Consistency) Below(target Consistency) bool {
	if !c.Valid() || !target.Valid() {
		return false
	}
	return c.Rank() < target.Rank()
}

// Title — название уровня для сообщений.
func (c Consistency) Title() string {
	switch c {
	case ConsistencyCrash:
		return "как после сбоя питания"
	case ConsistencyFilesystem:
		return "файловые системы"
	case ConsistencyApplication:
		return "приложения"
	}
	return "неизвестно"
}
