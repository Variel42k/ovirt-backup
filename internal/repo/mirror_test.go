package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// memBackend — хранилище в памяти: зеркалирование проверяется на поведении, а
// не на файловой системе.
type memBackend struct {
	name string
	mu   sync.Mutex
	objs map[string][]byte
	// failOn — ключ, на котором хранилище отказывает.
	failOn string
	// stallReads считает, сколько раз из хранилища читали.
	reads int
}

func newMem(name string) *memBackend {
	return &memBackend{name: name, objs: map[string][]byte{}}
}

func (m *memBackend) Kind() model.StorageKind { return model.StorageLocal }
func (m *memBackend) Name() string            { return m.name }

func (m *memBackend) Put(_ context.Context, key string, r io.Reader, _ int64) (int64, error) {
	if m.failOn != "" && strings.Contains(key, m.failOn) {
		// Читаем до конца и только потом отказываем: так ведёт себя настоящее
		// хранилище, которое приняло тело и не смогло его записать.
		_, _ = io.Copy(io.Discard, r)
		return 0, errors.New("хранилище " + m.name + " отказало")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = data
	return int64(len(data)), nil
}

func (m *memBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	data, ok := m.objs[key]
	if !ok {
		return nil, ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memBackend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	body, err := m.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	data, _ := io.ReadAll(body)
	if offset > int64(len(data)) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	data = data[offset:]
	if length >= 0 && length < int64(len(data)) {
		data = data[:length]
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memBackend) Stat(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objs[key]
	if !ok {
		return ObjectInfo{}, ErrNotExist
	}
	return ObjectInfo{Key: key, Size: int64(len(data))}, nil
}

func (m *memBackend) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

func (m *memBackend) DeletePrefix(_ context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for key := range m.objs {
		if strings.HasPrefix(key, prefix) {
			delete(m.objs, key)
			n++
		}
	}
	return n, nil
}

func (m *memBackend) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ObjectInfo
	for key, data := range m.objs {
		if strings.HasPrefix(key, prefix) {
			out = append(out, ObjectInfo{Key: key, Size: int64(len(data))})
		}
	}
	return out, nil
}

func (m *memBackend) Usage(context.Context) (int64, int64, error) { return 0, 0, nil }
func (m *memBackend) Check(context.Context) error                 { return nil }
func (m *memBackend) Close() error                                { return nil }

func (m *memBackend) get(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objs[key]
}

// Данные доходят до всех хранилищ, а источник читается ровно один раз: ради
// этого зеркало и делалось. Копия из основного прочитала бы сохранённое
// второй раз, а отдельный бэкап на каждое хранилище — заново снял бы диск с
// гипервизора, и платят за это продуктивные ВМ.
func TestMirrorWritesEverywhereReadingSourceOnce(t *testing.T) {
	primary, first, second := newMem("основное"), newMem("зеркало-1"), newMem("зеркало-2")
	mirror := NewMirror(primary, first, second)

	payload := bytes.Repeat([]byte("данные"), 5000)
	source := &countingReader{Reader: bytes.NewReader(payload)}

	written, err := mirror.Put(context.Background(), "chunk-1", source, int64(len(payload)))
	if err != nil {
		t.Fatalf("запись: %v", err)
	}
	if written != int64(len(payload)) {
		t.Errorf("записано %d байт, ожидалось %d", written, len(payload))
	}
	for _, backend := range []*memBackend{primary, first, second} {
		if !bytes.Equal(backend.get("chunk-1"), payload) {
			t.Errorf("в %s легло не то", backend.Name())
		}
	}
	if source.calls == 0 {
		t.Fatal("источник не читался вовсе")
	}
	if source.bytes != int64(len(payload)) {
		t.Errorf("из источника прочитано %d байт вместо %d — значит, читали повторно",
			source.bytes, len(payload))
	}
}

// Отвалившееся зеркало не роняет бэкап: основное хранилище — источник истины,
// и пока оно принимает данные, копия состоялась. Иначе недоступная вторая
// площадка означала бы, что этой ночью бэкапов нет вовсе.
func TestMirrorFailureDoesNotFailTheBackup(t *testing.T) {
	primary, broken := newMem("основное"), newMem("зеркало")
	broken.failOn = "chunk"
	mirror := NewMirror(primary, broken).(*Mirror)

	payload := []byte("важные данные")
	if _, err := mirror.Put(context.Background(), "chunk-1", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("отказ зеркала уронил запись: %v", err)
	}
	if !bytes.Equal(primary.get("chunk-1"), payload) {
		t.Error("в основное хранилище данные не легли")
	}

	failed := mirror.Failed()
	if len(failed) != 1 || failed["зеркало"] == nil {
		t.Fatalf("отказ зеркала не запомнен: %v", failed)
	}

	// Упавшее зеркало больше не тревожим: сеть, которая только что порвалась,
	// редко чинится к следующему чанку, а каждая попытка стоит таймаута.
	broken.failOn = ""
	if _, err := mirror.Put(context.Background(), "chunk-2", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("вторая запись: %v", err)
	}
	if broken.get("chunk-2") != nil {
		t.Error("в упавшее зеркало продолжили писать в том же запуске")
	}
}

// Отказ основного хранилища — это отказ запуска, и молчать о нём нельзя.
func TestMirrorReportsPrimaryFailure(t *testing.T) {
	primary, healthy := newMem("основное"), newMem("зеркало")
	primary.failOn = "chunk"
	mirror := NewMirror(primary, healthy)

	if _, err := mirror.Put(context.Background(), "chunk-1", strings.NewReader("данные"), 6); err == nil {
		t.Fatal("отказ основного хранилища принят за успех")
	}
}

// Чтение идёт из основного: зеркало заведено, чтобы там лежала копия, а не
// чтобы её оттуда брали в обычной работе.
func TestMirrorReadsFromPrimaryOnly(t *testing.T) {
	primary, second := newMem("основное"), newMem("зеркало")
	mirror := NewMirror(primary, second)

	if _, err := mirror.Put(context.Background(), "obj", strings.NewReader("данные"), 6); err != nil {
		t.Fatalf("запись: %v", err)
	}
	body, err := mirror.Get(context.Background(), "obj")
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	_ = body.Close()

	if primary.reads != 1 || second.reads != 0 {
		t.Errorf("чтений из основного %d, из зеркала %d", primary.reads, second.reads)
	}
}

// Без зеркал обёртки быть не должно: она ничего не делает, но путает журнал и
// стек вызовов.
func TestMirrorWithoutMirrorsReturnsPrimary(t *testing.T) {
	primary := newMem("основное")
	if got := NewMirror(primary); got != Backend(primary) {
		t.Errorf("без зеркал вернулась обёртка %T", got)
	}
	if got := NewMirror(primary, nil, nil); got != Backend(primary) {
		t.Errorf("пустые зеркала не отсеялись: %T", got)
	}
}

// countingReader считает, сколько байт и сколько раз читали из источника.
type countingReader struct {
	io.Reader
	calls int
	bytes int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.calls++
	c.bytes += int64(n)
	return n, err
}
