package nbd

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// maxStatusQuery ограничивает объём запроса NBD_CMD_BLOCK_STATUS. Поле длины занимает 32
// бита, а серверы ограничивают ответы, поэтому большие диски опрашиваем по частям.
const maxStatusQuery int64 = 1 << 30 // 1 GiB

// Export описывает экспорт, опубликованный сервером.
type Export struct {
	Name               string
	Size               int64
	TransmissionFlags  uint16
	MinBlockSize       uint32
	PreferredBlockSize uint32
	MaxBlockSize       uint32
	Description        string
}

// ReadOnly показывает, доступен ли экспорт только для чтения.
func (e Export) ReadOnly() bool { return e.TransmissionFlags&TransmissionReadOnly != 0 }

// Options задаёт параметры согласования соединения.
type Options struct {
	// ExportName — имя экспорта. Для бэкапа libvirt это то, что задано в
	// <disk exportname='...'> в XML запроса бэкапа.
	ExportName string

	// MetaContexts — запрашиваемые контексты метаданных, например
	// base:allocation и qemu:dirty-bitmap:<имя>. Сервер может согласовать
	// не все; согласованные видны через Contexts().
	MetaContexts []string

	// RequireStructuredReplies заставляет соединение падать, если сервер не
	// умеет структурированные ответы. Без них недоступен BLOCK_STATUS, то
	// есть невозможен инкрементальный бэкап.
	RequireStructuredReplies bool

	// HandshakeTimeout ограничивает согласование; 0 — 30 секунд.
	HandshakeTimeout time.Duration
}

// Client представляет открытый сеанс NBD. Методы допускают одновременные вызовы, но запросы
// выполняются последовательно. Для параллельного чтения открываем несколько соединений.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	mu     sync.Mutex
	cookie uint64
	closed bool

	export     Export
	structured bool
	// contextByID и idByContext хранят согласованные контексты метаданных.
	contextByID map[uint32]string
	idByContext map[string]uint32
}

// Connect согласует параметры на уже открытом соединении. Его открывает вызывающий код,
// поэтому клиент одинаково работает через TCP, сокет Unix и канал SSH.
func Connect(ctx context.Context, conn net.Conn, opts Options) (*Client, error) {
	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	c := &Client{
		conn:        conn,
		r:           bufio.NewReaderSize(conn, 128<<10),
		w:           bufio.NewWriterSize(conn, 64<<10),
		contextByID: map[uint32]string{},
		idByContext: map[string]uint32{},
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.applyDeadline(handshakeCtx); err != nil {
		return nil, err
	}

	if err := c.handshake(opts); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// В фазе передачи таймаут по умолчанию отсутствует. Чтение терабайта может занять часы,
	// и фиксированный дедлайн оборвал бы его.
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}

func (c *Client) handshake(opts Options) error {
	var greeting struct {
		Magic    uint64
		IHaveOpt uint64
		Flags    uint16
	}
	if err := binary.Read(c.r, binary.BigEndian, &greeting); err != nil {
		return fmt.Errorf("чтение приветствия NBD: %w", err)
	}
	if greeting.Magic != magicNBD {
		return fmt.Errorf("это не NBD-сервер: получена сигнатура %#x", greeting.Magic)
	}
	if greeting.IHaveOpt != magicIHaveOpt {
		// Старая версия протокола передаёт здесь размер экспорта. Она не поддерживает
		// согласование необходимых клиенту параметров.
		return errors.New("сервер использует устаревший протокол NBD без согласования опций")
	}
	if greeting.Flags&flagFixedNewstyle == 0 {
		return errors.New("сервер не поддерживает fixed newstyle handshake")
	}

	if err := binary.Write(c.w, binary.BigEndian, clientFlagFixedNewstyle); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}

	// Структурированные ответы согласуются до контекстов метаданных. Состояние блоков
	// передаётся только структурированными фрагментами.
	if err := c.negotiateStructuredReplies(opts.RequireStructuredReplies); err != nil {
		return err
	}
	if len(opts.MetaContexts) > 0 {
		if !c.structured {
			return errors.New("сервер не поддерживает структурированные ответы — контексты метаданных недоступны")
		}
		if err := c.setMetaContexts(opts.ExportName, opts.MetaContexts); err != nil {
			return err
		}
	}
	return c.optionGo(opts.ExportName)
}

func (c *Client) negotiateStructuredReplies(required bool) error {
	if err := c.sendOption(optStructuredReply, nil); err != nil {
		return err
	}
	for {
		reply, err := c.readOptionReply(optStructuredReply)
		if err != nil {
			var optErr *OptionError
			if errors.As(err, &optErr) && optErr.Unsupported() && !required {
				// Обычное чтение доступно, инкрементальное копирование невозможно.
				c.structured = false
				return nil
			}
			return err
		}
		if reply.Type == repACK {
			c.structured = true
			return nil
		}
	}
}

func (c *Client) setMetaContexts(export string, queries []string) error {
	payload := make([]byte, 0, 64)
	payload = appendString32(payload, export)
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(queries)))
	for _, q := range queries {
		payload = appendString32(payload, q)
	}

	if err := c.sendOption(optSetMetaContext, payload); err != nil {
		return err
	}
	for {
		reply, err := c.readOptionReply(optSetMetaContext)
		if err != nil {
			return err
		}
		switch reply.Type {
		case repMetaContext:
			if len(reply.Data) < 4 {
				return errors.New("сервер прислал усечённое описание контекста метаданных")
			}
			id := binary.BigEndian.Uint32(reply.Data[:4])
			name := string(reply.Data[4:])
			c.contextByID[id] = name
			c.idByContext[name] = id
		case repACK:
			return nil
		}
	}
}

func (c *Client) optionGo(export string) error {
	payload := make([]byte, 0, 32)
	payload = appendString32(payload, export)
	// Явно запрашиваем границы размера блока. Сервер с обязательным согласованием размера
	// иначе отклонит NBD_OPT_GO.
	payload = binary.BigEndian.AppendUint16(payload, 1)
	payload = binary.BigEndian.AppendUint16(payload, infoBlockSize)

	if err := c.sendOption(optGo, payload); err != nil {
		return err
	}
	c.export.Name = export

	for {
		reply, err := c.readOptionReply(optGo)
		if err != nil {
			return err
		}
		switch reply.Type {
		case repInfo:
			if err := c.applyInfo(reply.Data); err != nil {
				return err
			}
		case repACK:
			if c.export.Size == 0 {
				return errors.New("сервер не сообщил размер экспорта")
			}
			return nil
		}
	}
}

func (c *Client) applyInfo(data []byte) error {
	if len(data) < 2 {
		return errors.New("сервер прислал усечённую информацию об экспорте")
	}
	kind := binary.BigEndian.Uint16(data[:2])
	body := data[2:]

	switch kind {
	case infoExport:
		if len(body) < 10 {
			return errors.New("усечённое описание экспорта")
		}
		c.export.Size = int64(binary.BigEndian.Uint64(body[:8]))
		c.export.TransmissionFlags = binary.BigEndian.Uint16(body[8:10])
	case infoBlockSize:
		if len(body) < 12 {
			return errors.New("усечённое описание размеров блока")
		}
		c.export.MinBlockSize = binary.BigEndian.Uint32(body[0:4])
		c.export.PreferredBlockSize = binary.BigEndian.Uint32(body[4:8])
		c.export.MaxBlockSize = binary.BigEndian.Uint32(body[8:12])
	case infoDescription:
		c.export.Description = string(body)
	case infoName:
		c.export.Name = string(body)
	}
	return nil
}

type optionReply struct {
	Type uint32
	Data []byte
}

func (c *Client) sendOption(option uint32, data []byte) error {
	header := make([]byte, 0, 16)
	header = binary.BigEndian.AppendUint64(header, magicIHaveOpt)
	header = binary.BigEndian.AppendUint32(header, option)
	header = binary.BigEndian.AppendUint32(header, uint32(len(data)))

	if _, err := c.w.Write(header); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := c.w.Write(data); err != nil {
			return err
		}
	}
	return c.w.Flush()
}

func (c *Client) readOptionReply(expected uint32) (*optionReply, error) {
	var head struct {
		Magic  uint64
		Option uint32
		Type   uint32
		Length uint32
	}
	if err := binary.Read(c.r, binary.BigEndian, &head); err != nil {
		return nil, fmt.Errorf("чтение ответа на опцию %s: %w", optionName(expected), err)
	}
	if head.Magic != magicOptReply {
		return nil, fmt.Errorf("нарушен формат ответа на опцию: сигнатура %#x", head.Magic)
	}
	if head.Option != expected {
		return nil, fmt.Errorf("сервер ответил на опцию %s вместо %s",
			optionName(head.Option), optionName(expected))
	}
	// Ограничиваем память, которую сервер может заставить клиент выделить при согласовании.
	if head.Length > 4<<20 {
		return nil, fmt.Errorf("ответ на опцию %s неправдоподобно велик: %d байт",
			optionName(expected), head.Length)
	}

	data := make([]byte, head.Length)
	if _, err := io.ReadFull(c.r, data); err != nil {
		return nil, err
	}

	if head.Type&0x80000000 != 0 {
		return nil, &OptionError{Option: head.Option, Reply: head.Type, Message: string(data)}
	}
	return &optionReply{Type: head.Type, Data: data}, nil
}

// Export возвращает сведения, которые сервер сообщил об открытом экспорте.
func (c *Client) Export() Export { return c.export }

// StructuredReplies показывает, согласованы ли структурированные ответы, необходимые для
// BlockStatus.
func (c *Client) StructuredReplies() bool { return c.structured }

// Contexts возвращает согласованные контексты метаданных по именам.
func (c *Client) Contexts() map[string]uint32 {
	out := make(map[string]uint32, len(c.idByContext))
	for name, id := range c.idByContext {
		out[name] = id
	}
	return out
}

// HasContext проверяет, был ли согласован указанный контекст метаданных.
func (c *Client) HasContext(name string) bool {
	_, ok := c.idByContext[name]
	return ok
}

// Close отправляет запрос на отключение и освобождает соединение.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true

	// Соединение закрываем в любом случае, даже если сервер уже недоступен.
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.sendRequest(cmdDisconnect, 0, 0, 0); err == nil {
		_ = c.w.Flush()
	}
	return c.conn.Close()
}

// ReadAt заполняет p данными экспорта со смещения off.
func (c *Client) ReadAt(ctx context.Context, p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	if off < 0 || off+int64(len(p)) > c.export.Size {
		return fmt.Errorf("чтение [%d, %d) выходит за границы экспорта размером %d",
			off, off+int64(len(p)), c.export.Size)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("соединение NBD закрыто")
	}
	if err := c.applyDeadline(ctx); err != nil {
		return err
	}

	cookie := c.nextCookie()
	if err := c.sendRequest(cmdRead, cookie, off, uint32(len(p))); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}

	if !c.structured {
		return c.readSimpleReply(cookie, p)
	}
	return c.readDataChunks(cookie, p, off)
}

// Extent описывает непрерывный диапазон в контексте метаданных.
type Extent struct {
	// Offset — смещение от начала экспорта.
	Offset int64
	Length int64
	// Flags — биты состояния: StateHole/StateZero для base:allocation,
	// StateDirty для qemu:dirty-bitmap.
	Flags uint32
}

// End возвращает смещение первого байта за диапазоном.
func (e Extent) End() int64 { return e.Offset + e.Length }

// Zero показывает, читается ли диапазон base:allocation как нули.
func (e Extent) Zero() bool { return e.Flags&StateZero != 0 }

// Hole показывает, является ли диапазон base:allocation невыделенным.
func (e Extent) Hole() bool { return e.Flags&StateHole != 0 }

// Dirty показывает, менялся ли диапазон dirty-bitmap после checkpoint.
func (e Extent) Dirty() bool { return e.Flags&StateDirty != 0 }

// BlockStatus запрашивает один контекст метаданных в диапазоне [offset, offset+length).
//
// Сервер может описать лишь часть запрошенного диапазона. ExtentMap продолжит опрос с
// места, где закончился ответ.
func (c *Client) BlockStatus(ctx context.Context, contextName string, offset, length int64) ([]Extent, error) {
	if !c.structured {
		return nil, errors.New("BLOCK_STATUS требует структурированных ответов, которых сервер не поддерживает")
	}
	wantID, ok := c.idByContext[contextName]
	if !ok {
		return nil, fmt.Errorf("контекст метаданных %q не согласован с сервером", contextName)
	}
	if offset < 0 || offset >= c.export.Size {
		return nil, fmt.Errorf("смещение %d вне экспорта размером %d", offset, c.export.Size)
	}
	if length <= 0 {
		return nil, nil
	}
	if offset+length > c.export.Size {
		length = c.export.Size - offset
	}
	if length > maxStatusQuery {
		length = maxStatusQuery
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("соединение NBD закрыто")
	}
	if err := c.applyDeadline(ctx); err != nil {
		return nil, err
	}

	cookie := c.nextCookie()
	if err := c.sendRequest(cmdBlockStatus, cookie, offset, uint32(length)); err != nil {
		return nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}

	var extents []Extent
	cursor := offset
	done := false

	// Сервер отвечает по всем согласованным контекстам в своём порядке. Флаг DONE может прийти
	// во фрагменте ненужного контекста, поэтому проверяем его даже после пропуска данных.
	for !done {
		chunk, err := c.readChunkHeader(cookie)
		if err != nil {
			return nil, err
		}
		payload, err := c.readChunkPayload(chunk)
		if err != nil {
			return nil, err
		}
		done = chunk.Flags&replyFlagDone != 0

		switch chunk.Type {
		case replyTypeBlockStatus:
			if len(payload) < 4 {
				return nil, errors.New("усечённый ответ BLOCK_STATUS")
			}
			id := binary.BigEndian.Uint32(payload[:4])
			body := payload[4:]
			if len(body) == 0 || len(body)%8 != 0 {
				return nil, fmt.Errorf("некорректная длина дескрипторов BLOCK_STATUS: %d", len(body))
			}
			if id != wantID {
				break // чужой контекст — пропускаем, но флаг DONE уже учтён
			}
			for i := 0; i+8 <= len(body); i += 8 {
				extentLen := int64(binary.BigEndian.Uint32(body[i : i+4]))
				flags := binary.BigEndian.Uint32(body[i+4 : i+8])
				if extentLen <= 0 {
					return nil, errors.New("сервер вернул экстент нулевой длины")
				}
				// Последний дескриптор может выйти за конец запрошенного диапазона, но
				// должен оставаться внутри экспорта.
				if cursor+extentLen > c.export.Size {
					extentLen = c.export.Size - cursor
					if extentLen <= 0 {
						break
					}
				}
				extents = append(extents, Extent{Offset: cursor, Length: extentLen, Flags: flags})
				cursor += extentLen
			}

		case replyTypeError, replyTypeErrorOffset:
			return nil, parseErrorChunk(chunk.Type, payload)

		case replyTypeNone:
			// Завершающий фрагмент без данных.

		default:
			return nil, fmt.Errorf("неожиданный тип чанка %#x в ответе BLOCK_STATUS", chunk.Type)
		}
	}

	if len(extents) == 0 {
		return nil, fmt.Errorf("сервер не описал ни одного экстента для контекста %q", contextName)
	}
	return extents, nil
}

// ExtentMap обходит весь экспорт, возвращает диапазоны одного контекста и объединяет
// соседние диапазоны с одинаковыми флагами.
//
// Флаг NBD_CMD_FLAG_REQ_ONE запрашивает один диапазон и подходит для проверки одного
// смещения. При обходе всей карты сервер может вернуть несколько дескрипторов сразу.
//
// QEMU описывает терабайтный диск тысячами мелких диапазонов. Объединяем соседние, чтобы
// сохранить только границы между изменёнными и неизменёнными участками.
func (c *Client) ExtentMap(ctx context.Context, contextName string) ([]Extent, error) {
	var out []Extent
	offset := int64(0)

	for offset < c.export.Size {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := c.BlockStatus(ctx, contextName, offset, c.export.Size-offset)
		if err != nil {
			return nil, err
		}

		var advanced int64
		for _, e := range batch {
			if e.Offset < offset {
				// Пересекающиеся дескрипторы исказили бы карту диапазонов.
				return nil, fmt.Errorf("сервер вернул экстент со смещением %d, ожидалось не меньше %d",
					e.Offset, offset)
			}
			if n := len(out); n > 0 && out[n-1].End() == e.Offset && out[n-1].Flags == e.Flags {
				out[n-1].Length += e.Length
			} else {
				out = append(out, e)
			}
			advanced += e.Length
		}
		if advanced <= 0 {
			return nil, errors.New("сервер не продвинулся по карте экстентов — прерываю, чтобы не зациклиться")
		}
		offset += advanced
	}
	return out, nil
}

// Flush просит сервер записать буферизированные данные. Для экспорта только на чтение не
// нужен. Метод оставлен для серверов с этой возможностью и будущего восстановления.
func (c *Client) Flush(ctx context.Context) error {
	if c.export.TransmissionFlags&TransmissionSendFlush == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.applyDeadline(ctx); err != nil {
		return err
	}

	cookie := c.nextCookie()
	if err := c.sendRequest(cmdFlush, cookie, 0, 0); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}
	if !c.structured {
		return c.readSimpleReply(cookie, nil)
	}
	return c.drainChunks(cookie)
}

func (c *Client) sendRequest(command uint16, cookie uint64, offset int64, length uint32) error {
	buf := make([]byte, 0, 28)
	buf = binary.BigEndian.AppendUint32(buf, magicRequest)
	buf = binary.BigEndian.AppendUint16(buf, 0) // флаги команды
	buf = binary.BigEndian.AppendUint16(buf, command)
	buf = binary.BigEndian.AppendUint64(buf, cookie)
	buf = binary.BigEndian.AppendUint64(buf, uint64(offset))
	buf = binary.BigEndian.AppendUint32(buf, length)

	_, err := c.w.Write(buf)
	return err
}

func (c *Client) readSimpleReply(cookie uint64, into []byte) error {
	var head struct {
		Magic  uint32
		Error  uint32
		Cookie uint64
	}
	if err := binary.Read(c.r, binary.BigEndian, &head); err != nil {
		return fmt.Errorf("чтение ответа NBD: %w", err)
	}
	if head.Magic != magicSimpleReply {
		return fmt.Errorf("нарушен формат ответа NBD: сигнатура %#x", head.Magic)
	}
	if head.Cookie != cookie {
		return fmt.Errorf("ответ NBD относится к другому запросу (%d вместо %d)", head.Cookie, cookie)
	}
	if head.Error != 0 {
		return &ServerError{Errno: head.Error}
	}
	if len(into) == 0 {
		return nil
	}
	_, err := io.ReadFull(c.r, into)
	return err
}

type chunkHeader struct {
	Flags  uint16
	Type   uint16
	Length uint32
}

func (c *Client) readChunkHeader(cookie uint64) (*chunkHeader, error) {
	var head struct {
		Magic  uint32
		Flags  uint16
		Type   uint16
		Cookie uint64
		Length uint32
	}
	if err := binary.Read(c.r, binary.BigEndian, &head); err != nil {
		return nil, fmt.Errorf("чтение структурированного ответа NBD: %w", err)
	}
	if head.Magic != magicStructuredReply {
		return nil, fmt.Errorf("нарушен формат структурированного ответа: сигнатура %#x", head.Magic)
	}
	if head.Cookie != cookie {
		return nil, fmt.Errorf("структурированный ответ относится к другому запросу (%d вместо %d)",
			head.Cookie, cookie)
	}
	if head.Length > 64<<20 {
		return nil, fmt.Errorf("чанк ответа неправдоподобно велик: %d байт", head.Length)
	}
	return &chunkHeader{Flags: head.Flags, Type: head.Type, Length: head.Length}, nil
}

func (c *Client) readChunkPayload(chunk *chunkHeader) ([]byte, error) {
	if chunk.Length == 0 {
		return nil, nil
	}
	buf := make([]byte, chunk.Length)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// readDataChunks собирает ответ на чтение. Сначала обнуляем p, чтобы невыделенные диапазоны
// уже содержали нули.
func (c *Client) readDataChunks(cookie uint64, p []byte, base int64) error {
	for i := range p {
		p[i] = 0
	}
	covered := int64(0)

	for {
		chunk, err := c.readChunkHeader(cookie)
		if err != nil {
			return err
		}

		switch chunk.Type {
		case replyTypeOffsetData:
			if chunk.Length < 8 {
				return errors.New("усечённый чанк данных")
			}
			var offset uint64
			if err := binary.Read(c.r, binary.BigEndian, &offset); err != nil {
				return err
			}
			size := int64(chunk.Length) - 8
			start := int64(offset) - base
			if start < 0 || start+size > int64(len(p)) {
				return fmt.Errorf("сервер прислал данные [%d, %d) вне запрошенного диапазона [%d, %d)",
					offset, int64(offset)+size, base, base+int64(len(p)))
			}
			if _, err := io.ReadFull(c.r, p[start:start+size]); err != nil {
				return err
			}
			covered += size

		case replyTypeOffsetHole:
			if chunk.Length != 12 {
				return fmt.Errorf("некорректная длина чанка дыры: %d", chunk.Length)
			}
			var hole struct {
				Offset uint64
				Size   uint32
			}
			if err := binary.Read(c.r, binary.BigEndian, &hole); err != nil {
				return err
			}
			start := int64(hole.Offset) - base
			if start < 0 || start+int64(hole.Size) > int64(len(p)) {
				return fmt.Errorf("сервер прислал дыру вне запрошенного диапазона")
			}
			// Данные уже нулевые. Учитываем только покрытие диапазона.
			covered += int64(hole.Size)

		case replyTypeError, replyTypeErrorOffset:
			payload, err := c.readChunkPayload(chunk)
			if err != nil {
				return err
			}
			return parseErrorChunk(chunk.Type, payload)

		case replyTypeNone:
			if chunk.Length != 0 {
				return errors.New("чанк NONE не должен нести данных")
			}

		default:
			// Неизвестные типы фрагментов пропускаем.
			if _, err := c.readChunkPayload(chunk); err != nil {
				return err
			}
		}

		if chunk.Flags&replyFlagDone != 0 {
			break
		}
	}

	if covered != int64(len(p)) {
		return fmt.Errorf("сервер описал %d байт вместо запрошенных %d", covered, len(p))
	}
	return nil
}

// drainChunks считывает структурированный ответ, данные которого не нужны.
func (c *Client) drainChunks(cookie uint64) error {
	for {
		chunk, err := c.readChunkHeader(cookie)
		if err != nil {
			return err
		}
		payload, err := c.readChunkPayload(chunk)
		if err != nil {
			return err
		}
		if chunk.Type == replyTypeError || chunk.Type == replyTypeErrorOffset {
			return parseErrorChunk(chunk.Type, payload)
		}
		if chunk.Flags&replyFlagDone != 0 {
			return nil
		}
	}
}

func parseErrorChunk(kind uint16, payload []byte) error {
	if len(payload) < 6 {
		return errors.New("усечённый чанк ошибки NBD")
	}
	serverErr := &ServerError{Errno: binary.BigEndian.Uint32(payload[:4])}
	msgLen := int(binary.BigEndian.Uint16(payload[4:6]))
	if 6+msgLen <= len(payload) {
		serverErr.Message = string(payload[6 : 6+msgLen])
	}
	if kind == replyTypeErrorOffset && len(payload) >= 6+msgLen+8 {
		serverErr.Offset = int64(binary.BigEndian.Uint64(payload[6+msgLen : 6+msgLen+8]))
		serverErr.HasOffset = true
	}
	return serverErr
}

func (c *Client) nextCookie() uint64 {
	c.cookie++
	return c.cookie
}

// applyDeadline переносит дедлайн контекста на соединение. Синхронный обмен NBD иначе
// прервать нельзя.
func (c *Client) applyDeadline(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return c.conn.SetDeadline(time.Time{})
	}
	return c.conn.SetDeadline(deadline)
}

func appendString32(dst []byte, s string) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(s)))
	return append(dst, s...)
}
