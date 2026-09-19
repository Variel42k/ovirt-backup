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
