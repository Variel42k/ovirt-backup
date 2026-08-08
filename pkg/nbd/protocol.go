// Package nbd is a client for the Network Block Device protocol, limited to
// what a backup tool needs: the fixed newstyle handshake, structured replies,
// metadata contexts, ranged reads and block status queries.
//
// It exists instead of a binding to libnbd because that would mean cgo, and
// cgo would cost the single static binary this project is built around. The
// subset of the protocol involved is small and well specified, so the trade is
// a few hundred lines of wire format against a runtime dependency on every
// host the tool is copied to.
//
// The client speaks to whatever QEMU exposes: a pull-mode backup export from
// libvirt, a qemu-nbd server over a snapshot, or a plain qemu-nbd read-only
// export. It never writes.
package nbd

import "fmt"

// Handshake magic numbers, big-endian on the wire throughout the protocol.
const (
	magicNBD      uint64 = 0x4e42444d41474943 // "NBDMAGIC"
	magicIHaveOpt uint64 = 0x49484156454F5054 // "IHAVEOPT"
	magicOptReply uint64 = 0x3e889045565a9

	magicRequest         uint32 = 0x25609513
	magicSimpleReply     uint32 = 0x67446698
	magicStructuredReply uint32 = 0x668e33ef
)

// Handshake flags sent by the server.
const (
	flagFixedNewstyle uint16 = 1 << 0
	flagNoZeroes      uint16 = 1 << 1
)

// Handshake flags sent by the client in reply.
const (
	clientFlagFixedNewstyle uint32 = 1 << 0
	clientFlagNoZeroes      uint32 = 1 << 1
)

// Options exchanged during handshake.
const (
	optExportName      uint32 = 1
	optAbort           uint32 = 2
	optList            uint32 = 3
	optInfo            uint32 = 6
	optGo              uint32 = 7
	optStructuredReply uint32 = 8
	optSetMetaContext  uint32 = 10
)

// Option reply types. Values with the high bit set are errors.
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

// Information types requested through NBD_OPT_GO.
const (
	infoExport      uint16 = 0
	infoName        uint16 = 1
	infoDescription uint16 = 2
	infoBlockSize   uint16 = 3
)

// Transmission commands. Only the read-side ones are implemented: a backup
// tool that can write to the source is a backup tool that can destroy it.
const (
	cmdRead        uint16 = 0
	cmdDisconnect  uint16 = 2
	cmdFlush       uint16 = 3
	cmdBlockStatus uint16 = 7
)

// Structured reply chunk types.
const (
	replyTypeNone        uint16 = 0
	replyTypeOffsetData  uint16 = 1
	replyTypeOffsetHole  uint16 = 2
	replyTypeBlockStatus uint16 = 5
	replyTypeError       uint16 = 0x8001
	replyTypeErrorOffset uint16 = 0x8002
)

// replyFlagDone marks the last chunk of a structured reply.
const replyFlagDone uint16 = 1 << 0

// Transmission flags reported by the server for an export.
const (
	// TransmissionReadOnly means the export refuses writes. Backup exports are
	// always read-only, and a writable one is a sign the wrong export was
	// opened.
	TransmissionReadOnly     uint16 = 1 << 1
	TransmissionSendFlush    uint16 = 1 << 2
	TransmissionSendTrim     uint16 = 1 << 5
	TransmissionCanMultiConn uint16 = 1 << 8
)

// Standard metadata context names.
const (
	// ContextBaseAllocation describes which parts of the export are allocated
	// and which read as zero. It is what a full backup uses to skip holes.
	ContextBaseAllocation = "base:allocation"

	// ContextDirtyBitmapPrefix + bitmap name describes which parts changed
	// since the checkpoint that bitmap belongs to. This is the whole point of
	// incremental backup.
	ContextDirtyBitmapPrefix = "qemu:dirty-bitmap:"

	// ContextAllocationDepth tells which layer of a backing chain a block
	// comes from. Not needed for backup, listed for completeness.
	ContextAllocationDepth = "qemu:allocation-depth"
)

// DirtyBitmapContext builds the metadata context name for a bitmap.
func DirtyBitmapContext(bitmap string) string {
	return ContextDirtyBitmapPrefix + bitmap
}

// Status bits inside a base:allocation extent.
const (
	// StateHole — область не выделена в образе.
	StateHole uint32 = 1 << 0
	// StateZero — область гарантированно читается как нули.
	StateZero uint32 = 1 << 1
)

// StateDirty is the only bit defined for a qemu:dirty-bitmap context: the
// range changed since the checkpoint.
const StateDirty uint32 = 1 << 0

// Errno values the server may return.
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

// ServerError is a non-zero error code returned by the server for a command.
type ServerError struct {
	Errno   uint32
	Message string
	// Offset is set for NBD_REPLY_TYPE_ERROR_OFFSET, pointing at the byte the
	// server could not serve.
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

// OptionError is a refusal during handshake.
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

// Unsupported reports whether the server simply does not implement the option,
// which for optional features is a reason to fall back rather than to fail.
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
