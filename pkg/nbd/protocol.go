// Пакет nbd реализует части протокола Network Block Device, нужные для бэкапа. Это
// согласование fixed newstyle, структурированные ответы, контексты метаданных, чтение
// диапазонов и запросы состояния блоков.
//
// Привязка к libnbd потребовала бы cgo и лишила бы проект одного статического бинарника.
// Небольшую часть протокола реализуем на Go, чтобы инструмент работал без внешней
// библиотеки на каждом хосте.
//
// Клиент читает экспорт QEMU, открытый через libvirt pull или через qemu-nbd для снапшота
// либо обычного экспорта только для чтения. Запись не поддерживается.
package nbd

import "fmt"

// Сигнатуры согласования. Для всего сетевого протокола используются числа big-endian.
const (
	magicNBD      uint64 = 0x4e42444d41474943 // "NBDMAGIC"
	magicIHaveOpt uint64 = 0x49484156454F5054 // "IHAVEOPT"
	magicOptReply uint64 = 0x3e889045565a9

	magicRequest         uint32 = 0x25609513
	magicSimpleReply     uint32 = 0x67446698
	magicStructuredReply uint32 = 0x668e33ef
)

// Флаги согласования, отправляемые сервером.
const (
	flagFixedNewstyle uint16 = 1 << 0
	flagNoZeroes      uint16 = 1 << 1
)

// Флаги согласования, отправляемые клиентом в ответ.
const (
	clientFlagFixedNewstyle uint32 = 1 << 0
	clientFlagNoZeroes      uint32 = 1 << 1
)

// Параметры, которыми обмениваются при согласовании.
const (
	optExportName      uint32 = 1
	optAbort           uint32 = 2
	optList            uint32 = 3
	optInfo            uint32 = 6
	optGo              uint32 = 7
	optStructuredReply uint32 = 8
	optSetMetaContext  uint32 = 10
)

// Типы ответов на запрос параметров. У ошибок установлен старший бит.
const (
	repACK         uint32 = 1
	repServer      uint32 = 2
	repInfo        uint32 = 3
	repMetaContext uint32 = 4

	repErrUnsup         uint32 = 0x80000001
	repErrPolicy        uint32 = 0x80000002
	repErrInvalid       uint32 = 0x80000003
	repErrPlatform      uint32 = 0x80000004
	repErrTLSRequired   uint32 = 0x80000005
	repErrUnknown       uint32 = 0x80000006
	repErrShutdown      uint32 = 0x80000007
	repErrBlockSizeReqd uint32 = 0x80000008
	repErrTooBig        uint32 = 0x80000009
)

// Типы сведений, запрашиваемых через NBD_OPT_GO.
const (
	infoExport      uint16 = 0
	infoName        uint16 = 1
	infoDescription uint16 = 2
	infoBlockSize   uint16 = 3
)

// Команды передачи. Реализовано только чтение, чтобы бэкап не мог повредить источник.
const (
	cmdRead        uint16 = 0
	cmdDisconnect  uint16 = 2
	cmdFlush       uint16 = 3
	cmdBlockStatus uint16 = 7
)

// Типы фрагментов структурированного ответа.
const (
	replyTypeNone        uint16 = 0
	replyTypeOffsetData  uint16 = 1
	replyTypeOffsetHole  uint16 = 2
	replyTypeBlockStatus uint16 = 5
	replyTypeError       uint16 = 0x8001
	replyTypeErrorOffset uint16 = 0x8002
)

// replyFlagDone отмечает последний фрагмент структурированного ответа.
const replyFlagDone uint16 = 1 << 0

// Флаги передачи, сообщаемые сервером для экспорта.
const (
	// TransmissionReadOnly означает, что экспорт доступен только для чтения. Доступный на
	// запись экспорт указывает, что открыт неверный источник.
	TransmissionReadOnly     uint16 = 1 << 1
	TransmissionSendFlush    uint16 = 1 << 2
	TransmissionSendTrim     uint16 = 1 << 5
	TransmissionCanMultiConn uint16 = 1 << 8
)

// Стандартные имена контекстов метаданных.
const (
	// ContextBaseAllocation указывает выделенные области и области, которые читаются как
	// нули. Полный бэкап пропускает по этому контексту невыделенные диапазоны.
	ContextBaseAllocation = "base:allocation"

	// ContextDirtyBitmapPrefix и имя битовой карты задают контекст изменённых областей с
	// момента соответствующего checkpoint. Этот контекст нужен инкрементальному бэкапу.
	ContextDirtyBitmapPrefix = "qemu:dirty-bitmap:"

	// ContextAllocationDepth сообщает, из какого слоя цепочки образов берётся блок. Для
	// бэкапа не используется.
	ContextAllocationDepth = "qemu:allocation-depth"
)

// DirtyBitmapContext формирует имя контекста метаданных для битовой карты.
func DirtyBitmapContext(bitmap string) string {
	return ContextDirtyBitmapPrefix + bitmap
}

// Биты состояния в диапазоне base:allocation.
const (
	// StateHole — область не выделена в образе.
	StateHole uint32 = 1 << 0
	// StateZero — область гарантированно читается как нули.
	StateZero uint32 = 1 << 1
)

// StateDirty — единственный бит контекста qemu:dirty-bitmap. Он означает, что диапазон
// менялся после checkpoint.
const StateDirty uint32 = 1 << 0

// Коды Errno, которые может вернуть сервер.
const (
	errPerm     uint32 = 1
	errIO       uint32 = 5
	errNoMem    uint32 = 12
	errInval    uint32 = 22
	errNoSpc    uint32 = 28
	errOverflow uint32 = 75
	errNotSup   uint32 = 95
	errShutdown uint32 = 108
)

// ServerError содержит ненулевой код ошибки, возвращённый сервером на команду.
type ServerError struct {
	Errno   uint32
	Message string
	// Для NBD_REPLY_TYPE_ERROR_OFFSET поле Offset указывает байт, который сервер не смог
	// обработать.
	Offset    int64
	HasOffset bool
}

func (e *ServerError) Error() string {
	name := errnoName(e.Errno)
	if e.Message != "" {
		if e.HasOffset {
			return fmt.Sprintf("сервер NBD вернул ошибку %s по смещению %d: %s", name, e.Offset, e.Message)
		}
		return fmt.Sprintf("сервер NBD вернул ошибку %s: %s", name, e.Message)
	}
	if e.HasOffset {
		return fmt.Sprintf("сервер NBD вернул ошибку %s по смещению %d", name, e.Offset)
	}
	return fmt.Sprintf("сервер NBD вернул ошибку %s", name)
}

func errnoName(code uint32) string {
	switch code {
	case errPerm:
		return "EPERM (нет прав)"
	case errIO:
		return "EIO (ошибка ввода-вывода)"
	case errNoMem:
		return "ENOMEM"
	case errInval:
		return "EINVAL (некорректный запрос)"
	case errNoSpc:
		return "ENOSPC"
	case errOverflow:
		return "EOVERFLOW"
	case errNotSup:
		return "ENOTSUP (не поддерживается)"
	case errShutdown:
		return "ESHUTDOWN (сервер завершает работу)"
	default:
		return fmt.Sprintf("errno=%d", code)
	}
}

// OptionError означает, что сервер отказал при согласовании параметра.
type OptionError struct {
	Option  uint32
	Reply   uint32
	Message string
}

func (e *OptionError) Error() string {
	reason := optReplyName(e.Reply)
	if e.Message != "" {
		return fmt.Sprintf("сервер NBD отклонил опцию %s: %s (%s)", optionName(e.Option), reason, e.Message)
	}
	return fmt.Sprintf("сервер NBD отклонил опцию %s: %s", optionName(e.Option), reason)
}

// Unsupported проверяет, реализует ли сервер параметр. При отсутствии необязательной
// возможности можно продолжить без неё.
func (e *OptionError) Unsupported() bool { return e.Reply == repErrUnsup }

func optionName(opt uint32) string {
	switch opt {
	case optExportName:
		return "EXPORT_NAME"
	case optAbort:
		return "ABORT"
	case optList:
		return "LIST"
	case optInfo:
		return "INFO"
	case optGo:
		return "GO"
	case optStructuredReply:
		return "STRUCTURED_REPLY"
	case optSetMetaContext:
		return "SET_META_CONTEXT"
	default:
		return fmt.Sprintf("option=%d", opt)
	}
}

func optReplyName(rep uint32) string {
	switch rep {
	case repErrUnsup:
		return "не поддерживается"
	case repErrPolicy:
		return "запрещено политикой сервера"
	case repErrInvalid:
		return "некорректный запрос"
	case repErrPlatform:
		return "не поддерживается платформой"
	case repErrTLSRequired:
		return "требуется TLS"
	case repErrUnknown:
		return "неизвестный экспорт"
	case repErrShutdown:
		return "сервер завершает работу"
	case repErrBlockSizeReqd:
		return "клиент обязан согласовать размер блока"
	case repErrTooBig:
		return "запрос слишком велик"
	default:
		return fmt.Sprintf("код %#x", rep)
	}
}
