package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// mockServer is a minimal NBD server used to prove the client speaks the
// protocol. It is deliberately written from the specification rather than by
// mirroring the client's code, so a misreading in the client does not hide
// behind an identical misreading in the test.
type mockServer struct {
	t *testing.T

	size       int64
	data       []byte
	structured bool

	// contexts — какие контексты метаданных сервер согласует.
	contexts []string
	// extents[context] — дескрипторы, которые сервер отдаст на BLOCK_STATUS.
	extents map[string][]mockExtent

	// zeroRanges заставляют сервер отвечать чанками дыр вместо данных.
	zeroRanges []rng

	// failRead заставляет сервер вернуть ошибку на команду чтения.
	failRead uint32
	// noStructuredOption заставляет сервер отклонить NBD_OPT_STRUCTURED_REPLY.
	noStructuredOption bool

	assignedIDs map[string]uint32
}

type mockExtent struct {
	Length uint32
	Flags  uint32
}

type rng struct{ start, end int64 }

func newMockServer(t *testing.T, size int64) *mockServer {
	return &mockServer{
		t:           t,
		size:        size,
		data:        make([]byte, size),
		structured:  true,
		contexts:    []string{ContextBaseAllocation},
		extents:     map[string][]mockExtent{},
		assignedIDs: map[string]uint32{},
	}
}

// start wires the server to a pipe and returns the client end.
func (s *mockServer) start() net.Conn {
	clientConn, serverConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		if err := s.serve(serverConn); err != nil && !errors.Is(err, io.EOF) &&
			!errors.Is(err, io.ErrClosedPipe) {
			// Reported through the test log rather than t.Fatal: this runs on
			// another goroutine.
			s.t.Logf("mock NBD server: %v", err)
		}
	}()
	return clientConn
}

func (s *mockServer) serve(conn net.Conn) error {
	w := conn
	r := conn

	// Greeting.
	if err := writeAll(w,
		be64(magicNBD), be64(magicIHaveOpt), be16(flagFixedNewstyle)); err != nil {
		return err
	}
	var clientFlags uint32
	if err := binary.Read(r, binary.BigEndian, &clientFlags); err != nil {
		return err
	}

	// Option haggling.
	for {
		var head struct {
			Magic  uint64
			Option uint32
			Length uint32
		}
		if err := binary.Read(r, binary.BigEndian, &head); err != nil {
			return err
		}
		if head.Magic != magicIHaveOpt {
			return errors.New("bad option magic from client")
		}
		payload := make([]byte, head.Length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}

		switch head.Option {
		case optStructuredReply:
			if s.noStructuredOption {
				if err := s.optErr(w, head.Option, repErrUnsup, "not supported here"); err != nil {
					return err
				}
				s.structured = false
				continue
			}
			if err := s.optReply(w, head.Option, repACK, nil); err != nil {
				return err
			}

		case optSetMetaContext:
			if err := s.handleSetMetaContext(w, payload); err != nil {
				return err
			}

		case optGo:
			if err := s.handleGo(w, payload); err != nil {
				return err
			}
			return s.transmission(conn)

		case optAbort:
			_ = s.optReply(w, head.Option, repACK, nil)
			return nil

		default:
			if err := s.optErr(w, head.Option, repErrUnsup, ""); err != nil {
				return err
			}
		}
	}
}

func (s *mockServer) handleSetMetaContext(w io.Writer, payload []byte) error {
	// Format: export name, then a count of queries, then the queries.
	if len(payload) < 4 {
		return errors.New("short SET_META_CONTEXT")
	}
	nameLen := binary.BigEndian.Uint32(payload[:4])
	pos := 4 + int(nameLen)
	if pos+4 > len(payload) {
		return errors.New("short SET_META_CONTEXT")
	}
	count := binary.BigEndian.Uint32(payload[pos : pos+4])
	pos += 4

	var nextID uint32 = 1
	for i := uint32(0); i < count; i++ {
		if pos+4 > len(payload) {
			return errors.New("short query")
		}
		qLen := int(binary.BigEndian.Uint32(payload[pos : pos+4]))
		pos += 4
		if pos+qLen > len(payload) {
			return errors.New("short query body")
		}
		query := string(payload[pos : pos+qLen])
		pos += qLen

		for _, supported := range s.contexts {
			if supported != query {
				continue
			}
			id := nextID
			nextID++
			s.assignedIDs[query] = id

			body := append(be32(id), []byte(query)...)
			if err := s.optReply(w, optSetMetaContext, repMetaContext, body); err != nil {
				return err
			}
		}
	}
	return s.optReply(w, optSetMetaContext, repACK, nil)
}

func (s *mockServer) handleGo(w io.Writer, payload []byte) error {
	info := append(be16(infoExport), be64(uint64(s.size))...)
	info = append(info, be16(TransmissionReadOnly)...)
	if err := s.optReply(w, optGo, repInfo, info); err != nil {
		return err
	}

	blockSize := be16(infoBlockSize)
	blockSize = append(blockSize, be32(1)...)
	blockSize = append(blockSize, be32(4096)...)
	blockSize = append(blockSize, be32(32<<20)...)
	if err := s.optReply(w, optGo, repInfo, blockSize); err != nil {
		return err
	}

	return s.optReply(w, optGo, repACK, nil)
}

func (s *mockServer) optReply(w io.Writer, option, replyType uint32, data []byte) error {
	head := be64(magicOptReply)
	head = append(head, be32(option)...)
	head = append(head, be32(replyType)...)
	head = append(head, be32(uint32(len(data)))...)
	if _, err := w.Write(append(head, data...)); err != nil {
		return err
	}
	return nil
}

func (s *mockServer) optErr(w io.Writer, option, replyType uint32, msg string) error {
	return s.optReply(w, option, replyType, []byte(msg))
}

func (s *mockServer) transmission(conn net.Conn) error {
	for {
		var req struct {
			Magic  uint32
			Flags  uint16
			Type   uint16
			Cookie uint64
			Offset uint64
			Length uint32
		}
		if err := binary.Read(conn, binary.BigEndian, &req); err != nil {
			return err
		}
		if req.Magic != magicRequest {
			return errors.New("bad request magic")
		}

		switch req.Type {
		case cmdDisconnect:
			return nil

		case cmdRead:
			if err := s.serveRead(conn, req.Cookie, int64(req.Offset), int64(req.Length)); err != nil {
				return err
			}

		case cmdBlockStatus:
			if err := s.serveBlockStatus(conn, req.Cookie, int64(req.Offset), int64(req.Length)); err != nil {
				return err
			}

		case cmdFlush:
			if err := s.chunk(conn, req.Cookie, replyFlagDone, replyTypeNone, nil); err != nil {
				return err
			}

		default:
			return errors.New("unexpected command")
		}
	}
}

func (s *mockServer) serveRead(conn net.Conn, cookie uint64, offset, length int64) error {
	if s.failRead != 0 {
		if !s.structured {
			return writeAll(conn, be32(magicSimpleReply), be32(s.failRead), be64(cookie))
		}
		body := append(be32(s.failRead), be16(4)...)
		body = append(body, []byte("boom")...)
		return s.chunk(conn, cookie, replyFlagDone, replyTypeError, body)
	}

	if !s.structured {
		if err := writeAll(conn, be32(magicSimpleReply), be32(0), be64(cookie)); err != nil {
			return err
		}
		_, err := conn.Write(s.data[offset : offset+length])
		return err
	}

	// Split the answer into data and hole chunks, which is what a real server
	// does for a sparse image and what the client must reassemble.
	type piece struct {
		start, end int64
		hole       bool
	}
	var pieces []piece
	cursor := offset
	for cursor < offset+length {
		next := offset + length
		isHole := false
		for _, z := range s.zeroRanges {
			if cursor >= z.start && cursor < z.end {
				isHole = true
				if z.end < next {
					next = z.end
				}
				break
			}
			if cursor < z.start && z.start < next {
				next = z.start
			}
		}
		pieces = append(pieces, piece{start: cursor, end: next, hole: isHole})
		cursor = next
	}

	for i, p := range pieces {
		flags := uint16(0)
		if i == len(pieces)-1 {
			flags = replyFlagDone
		}
		if p.hole {
			body := append(be64(uint64(p.start)), be32(uint32(p.end-p.start))...)
			if err := s.chunk(conn, cookie, flags, replyTypeOffsetHole, body); err != nil {
				return err
			}
			continue
		}
		body := append(be64(uint64(p.start)), s.data[p.start:p.end]...)
		if err := s.chunk(conn, cookie, flags, replyTypeOffsetData, body); err != nil {
			return err
		}
	}
	return nil
}

func (s *mockServer) serveBlockStatus(conn net.Conn, cookie uint64, offset, length int64) error {
	names := make([]string, 0, len(s.assignedIDs))
	for name := range s.assignedIDs {
		names = append(names, name)
	}
	// Deterministic order so the test is reproducible.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	for i, name := range names {
		descriptors := s.extents[name]
		body := be32(s.assignedIDs[name])
		// Answer only for the requested window, starting at offset.
		var covered int64
		var pos int64
		for _, d := range descriptors {
			end := pos + int64(d.Length)
			if end <= offset {
				pos = end
				continue
			}
			start := pos
			if start < offset {
				start = offset
			}
			take := end - start
			if covered+take > length {
				take = length - covered
			}
			if take <= 0 {
				break
			}
			body = append(body, be32(uint32(take))...)
			body = append(body, be32(d.Flags)...)
			covered += take
			pos = end
			if covered >= length {
				break
			}
		}
		flags := uint16(0)
		if i == len(names)-1 {
			flags = replyFlagDone
		}
		if err := s.chunk(conn, cookie, flags, replyTypeBlockStatus, body); err != nil {
			return err
		}
	}
	return nil
}

func (s *mockServer) chunk(conn net.Conn, cookie uint64, flags, kind uint16, body []byte) error {
	head := be32(magicStructuredReply)
	head = append(head, be16(flags)...)
	head = append(head, be16(kind)...)
	head = append(head, be64(cookie)...)
	head = append(head, be32(uint32(len(body)))...)
	_, err := conn.Write(append(head, body...))
	return err
}

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func writeAll(w io.Writer, parts ...[]byte) error {
	var buf []byte
	for _, p := range parts {
		buf = append(buf, p...)
	}
	_, err := w.Write(buf)
	return err
}

// pattern fills a buffer with recognisable content.
func pattern(size int64) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i%251) + 1
	}
	return buf
}

func connect(t *testing.T, srv *mockServer, opts Options) *Client {
	t.Helper()
	conn := srv.start()
	client, err := Connect(context.Background(), conn, opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestHandshakeReportsExport(t *testing.T) {
	srv := newMockServer(t, 4<<20)
	client := connect(t, srv, Options{ExportName: "vda"})

	export := client.Export()
	if export.Size != 4<<20 {
		t.Errorf("размер экспорта %d, ожидалось %d", export.Size, 4<<20)
	}
	if !export.ReadOnly() {
		t.Error("экспорт бэкапа должен быть только для чтения")
	}
	if export.PreferredBlockSize != 4096 {
		t.Errorf("предпочтительный размер блока %d, ожидалось 4096", export.PreferredBlockSize)
	}
	if !client.StructuredReplies() {
		t.Error("структурированные ответы должны быть согласованы")
	}
}

func TestReadAssemblesDataAndHoleChunks(t *testing.T) {
	const size = 64 << 10
	srv := newMockServer(t, size)
	copy(srv.data, pattern(size))
	// Середина образа — дыра: сервер ответит чанком дыры, а не данными.
	srv.zeroRanges = []rng{{start: 16 << 10, end: 32 << 10}}
	for i := int64(16 << 10); i < 32<<10; i++ {
		srv.data[i] = 0
	}

	client := connect(t, srv, Options{ExportName: "vda"})

	buf := make([]byte, size)
	if err := client.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, srv.data) {
		t.Error("собранный из чанков буфер не совпал с содержимым экспорта")
	}

	// Дыра должна прочитаться нулями, а не мусором из предыдущего чтения.
	for i := 16 << 10; i < 32<<10; i++ {
		if buf[i] != 0 {
			t.Fatalf("байт %d в дыре не нулевой", i)
		}
	}
}

func TestReadPartialRange(t *testing.T) {
	const size = 32 << 10
	srv := newMockServer(t, size)
	copy(srv.data, pattern(size))
	client := connect(t, srv, Options{ExportName: "vda"})

	buf := make([]byte, 4096)
	if err := client.ReadAt(context.Background(), buf, 8192); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, srv.data[8192:8192+4096]) {
		t.Error("частичное чтение вернуло не тот диапазон")
	}
}

func TestReadRejectsOutOfBounds(t *testing.T) {
	srv := newMockServer(t, 4096)
	client := connect(t, srv, Options{ExportName: "vda"})

	buf := make([]byte, 4096)
	err := client.ReadAt(context.Background(), buf, 1024)
	if err == nil {
		t.Fatal("чтение за границей экспорта должно отклоняться клиентом")
	}
	if !strings.Contains(err.Error(), "границ") {
		t.Errorf("непонятная ошибка: %v", err)
	}
}

func TestSimpleRepliesStillWork(t *testing.T) {
	const size = 8192
	srv := newMockServer(t, size)
	srv.noStructuredOption = true
	copy(srv.data, pattern(size))

	conn := srv.start()
	client, err := Connect(context.Background(), conn, Options{ExportName: "vda"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	if client.StructuredReplies() {
		t.Error("сервер отклонил структурированные ответы, клиент не должен их считать согласованными")
	}

	buf := make([]byte, size)
	if err := client.ReadAt(context.Background(), buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, srv.data) {
		t.Error("простой ответ прочитан неверно")
	}
}

func TestStructuredRepliesRequiredForMetaContexts(t *testing.T) {
	srv := newMockServer(t, 4096)
	srv.noStructuredOption = true

	conn := srv.start()
	_, err := Connect(context.Background(), conn, Options{
		ExportName:   "vda",
		MetaContexts: []string{ContextBaseAllocation},
	})
	if err == nil {
		t.Fatal("контексты метаданных без структурированных ответов невозможны — Connect должен упасть")
	}
}

func TestBlockStatusBaseAllocation(t *testing.T) {
	const size = 1 << 20
	srv := newMockServer(t, size)
	srv.contexts = []string{ContextBaseAllocation}
	srv.extents[ContextBaseAllocation] = []mockExtent{
		{Length: 256 << 10, Flags: 0},
		{Length: 512 << 10, Flags: StateZero | StateHole},
		{Length: 256 << 10, Flags: 0},
	}

	client := connect(t, srv, Options{
		ExportName:   "vda",
		MetaContexts: []string{ContextBaseAllocation},
	})
	if !client.HasContext(ContextBaseAllocation) {
		t.Fatal("контекст base:allocation не согласован")
	}

	extents, err := client.ExtentMap(context.Background(), ContextBaseAllocation)
	if err != nil {
		t.Fatalf("ExtentMap: %v", err)
	}
	if len(extents) != 3 {
		t.Fatalf("экстентов %d, ожидалось 3: %+v", len(extents), extents)
	}
	if extents[1].Offset != 256<<10 || extents[1].Length != 512<<10 {
		t.Errorf("средний экстент неверен: %+v", extents[1])
	}
	if !extents[1].Zero() || !extents[1].Hole() {
		t.Error("средний экстент должен быть дырой и нулями")
	}
	if extents[0].Zero() {
		t.Error("первый экстент содержит данные, а помечен нулями")
	}

	var total int64
	for _, e := range extents {
		total += e.Length
	}
	if total != size {
		t.Errorf("карта покрывает %d байт вместо %d", total, size)
	}
}

func TestBlockStatusDirtyBitmapSelectsRightContext(t *testing.T) {
	const size = 1 << 20
	bitmap := DirtyBitmapContext("jhv-vda")

	srv := newMockServer(t, size)
	srv.contexts = []string{ContextBaseAllocation, bitmap}
	// Карты специально разные: если клиент перепутает контексты, тест упадёт.
	srv.extents[ContextBaseAllocation] = []mockExtent{
		{Length: size, Flags: 0},
	}
	srv.extents[bitmap] = []mockExtent{
		{Length: 128 << 10, Flags: StateDirty},
		{Length: 768 << 10, Flags: 0},
		{Length: 128 << 10, Flags: StateDirty},
	}

	client := connect(t, srv, Options{
		ExportName:   "vda",
		MetaContexts: []string{ContextBaseAllocation, bitmap},
	})

	dirty, err := client.ExtentMap(context.Background(), bitmap)
	if err != nil {
		t.Fatalf("ExtentMap dirty: %v", err)
	}
	if len(dirty) != 3 {
		t.Fatalf("экстентов %d, ожидалось 3: %+v", len(dirty), dirty)
	}
	if !dirty[0].Dirty() || dirty[1].Dirty() || !dirty[2].Dirty() {
		t.Errorf("неверная разметка изменённых областей: %+v", dirty)
	}

	alloc, err := client.ExtentMap(context.Background(), ContextBaseAllocation)
	if err != nil {
		t.Fatalf("ExtentMap alloc: %v", err)
	}
	if len(alloc) != 1 || alloc[0].Length != size {
		t.Errorf("карта base:allocation не должна зависеть от карты битмапа: %+v", alloc)
	}
}

func TestExtentMapMergesNeighbours(t *testing.T) {
	const size = 1 << 20
	srv := newMockServer(t, size)
	srv.contexts = []string{ContextBaseAllocation}
	// Сервер дробит одинаковые области на мелкие дескрипторы — типичное
	// поведение qemu на разреженном образе.
	var descriptors []mockExtent
	for i := 0; i < 16; i++ {
		descriptors = append(descriptors, mockExtent{Length: 64 << 10, Flags: 0})
	}
	srv.extents[ContextBaseAllocation] = descriptors

	client := connect(t, srv, Options{
		ExportName:   "vda",
		MetaContexts: []string{ContextBaseAllocation},
	})

	extents, err := client.ExtentMap(context.Background(), ContextBaseAllocation)
	if err != nil {
		t.Fatalf("ExtentMap: %v", err)
	}
	if len(extents) != 1 {
		t.Fatalf("соседние одинаковые экстенты должны сливаться, получено %d: %+v", len(extents), extents)
	}
	if extents[0].Length != size {
		t.Errorf("слитый экстент длиной %d, ожидалось %d", extents[0].Length, size)
	}
}

func TestUnnegotiatedContextIsRejected(t *testing.T) {
	srv := newMockServer(t, 4096)
	srv.contexts = []string{ContextBaseAllocation}
	srv.extents[ContextBaseAllocation] = []mockExtent{{Length: 4096, Flags: 0}}

	client := connect(t, srv, Options{
		ExportName:   "vda",
		MetaContexts: []string{ContextBaseAllocation},
	})

	// Запрос несогласованного битмапа должен падать сразу, а не молча
	// возвращать чужую карту — иначе инкремент собрал бы не те блоки.
	_, err := client.BlockStatus(context.Background(), DirtyBitmapContext("missing"), 0, 4096)
	if err == nil {
		t.Fatal("запрос несогласованного контекста должен отклоняться")
	}
	if !strings.Contains(err.Error(), "не согласован") {
		t.Errorf("непонятная ошибка: %v", err)
	}
}

func TestServerErrorIsReported(t *testing.T) {
	srv := newMockServer(t, 4096)
	srv.failRead = 5 // EIO

	client := connect(t, srv, Options{ExportName: "vda"})

	buf := make([]byte, 4096)
	err := client.ReadAt(context.Background(), buf, 0)
	if err == nil {
		t.Fatal("ошибка сервера должна возвращаться клиенту")
	}
	var serverErr *ServerError
	if !errors.As(err, &serverErr) {
		t.Fatalf("ожидался *ServerError, получено %T: %v", err, err)
	}
	if serverErr.Errno != 5 {
		t.Errorf("errno %d, ожидалось 5", serverErr.Errno)
	}
	if !strings.Contains(serverErr.Error(), "EIO") {
		t.Errorf("сообщение не расшифровывает errno: %s", serverErr.Error())
	}
}

func TestContextCancellationStopsRead(t *testing.T) {
	srv := newMockServer(t, 1<<20)
	client := connect(t, srv, Options{ExportName: "vda"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	buf := make([]byte, 4096)
	if err := client.ReadAt(ctx, buf, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("отменённый контекст должен прерывать чтение, получено: %v", err)
	}
}

func TestDeadlinePropagates(t *testing.T) {
	srv := newMockServer(t, 1<<20)
	client := connect(t, srv, Options{ExportName: "vda"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Сервер отвечает, так что чтение должно успеть; проверяем, что установка
	// дедлайна сама по себе ничего не ломает.
	buf := make([]byte, 4096)
	if err := client.ReadAt(ctx, buf, 0); err != nil {
		t.Fatalf("чтение с дедлайном не должно падать: %v", err)
	}
}

func TestNotAnNBDServer(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("HTTP/1.1 200 OK\r\n\r\nnot nbd at all here"))
	}()

	_, err := Connect(context.Background(), clientConn, Options{ExportName: "vda"})
	if err == nil {
		t.Fatal("подключение к не-NBD серверу должно падать")
	}
	if !strings.Contains(err.Error(), "не NBD-сервер") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}
