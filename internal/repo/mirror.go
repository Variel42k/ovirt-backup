package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Mirror пишет одни и те же объекты в основное хранилище и в зеркала за один
// проход.
//
// Зачем это вместо копирования из основного: копия из основного читает
// сохранённый бэкап целиком второй раз, то есть удваивает нагрузку на
// хранилище и растягивает окно. А запуск отдельного бэкапа на каждое
// хранилище — вариант ещё хуже: диск заново читается с гипервизора, и это
// платят продуктивные ВМ.
//
// Чтение всегда идёт из основного: зеркало для того и заведено, чтобы там
// лежала копия, а не чтобы её оттуда брали в обычной работе.
//
// Отказ зеркала не роняет бэкап. Основное хранилище — источник истины: пока
// оно принимает данные, копия состоялась. Отвалившееся зеркало запоминается, и
// вызывающий доложит о нём — дальше точку дошлёт очередь репликации. Иначе
// недоступная вторая площадка означала бы, что этой ночью бэкапов нет вовсе.
type Mirror struct {
	primary Backend
	mirrors []Backend

	mu     sync.Mutex
	failed map[string]error
}

// NewMirror собирает запись в основное хранилище и зеркала. Без зеркал
// возвращает основное хранилище как есть: обёртка без работы только мешала бы
// читать журнал и стек.
func NewMirror(primary Backend, mirrors ...Backend) Backend {
	live := make([]Backend, 0, len(mirrors))
	for _, m := range mirrors {
		if m != nil {
			live = append(live, m)
		}
	}
	if len(live) == 0 {
		return primary
	}
	return &Mirror{primary: primary, mirrors: live, failed: map[string]error{}}
}

// Failed возвращает зеркала, запись в которые не удалась, с первой ошибкой
// каждого.
func (m *Mirror) Failed() map[string]error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]error, len(m.failed))
	for name, err := range m.failed {
		out[name] = err
	}
	return out
}

func (m *Mirror) note(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, seen := m.failed[name]; !seen {
		m.failed[name] = err
	}
}

// healthy возвращает зеркала, которые ещё не отваливались. Однажды упавшее
// зеркало не тревожим до конца запуска: сеть, которая только что порвалась,
// редко чинится к следующему чанку, а каждая новая попытка стоит таймаута.
func (m *Mirror) healthy() []Backend {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Backend, 0, len(m.mirrors))
	for _, mirror := range m.mirrors {
		if _, broken := m.failed[mirror.Name()]; !broken {
			out = append(out, mirror)
		}
	}
	return out
}

func (m *Mirror) Kind() model.StorageKind { return m.primary.Kind() }
func (m *Mirror) Name() string            { return m.primary.Name() }

// Put пишет объект во все хранилища одновременно, читая источник один раз.
//
// Источник — поток с гипервизора, второй раз его не прочитать, поэтому данные
// расходятся через каналы: на каждое хранилище своя труба и своя горутина.
// Скорость при этом равна скорости самого медленного из них — это плата за то,
// что второго чтения не будет.
func (m *Mirror) Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	targets := append([]Backend{m.primary}, m.healthy()...)
	if len(targets) == 1 {
		return m.primary.Put(ctx, key, r, size)
	}

	type result struct {
		backend Backend
		written int64
		err     error
	}

	type pipeTarget struct {
		backend Backend
		writer  *io.PipeWriter
		active  bool
	}
	pipes := make([]*pipeTarget, 0, len(targets))
	results := make(chan result, len(targets))

	for _, backend := range targets {
		pr, pw := io.Pipe()
		pipes = append(pipes, &pipeTarget{backend: backend, writer: pw, active: true})

		go func(b Backend, source *io.PipeReader) {
			written, err := b.Put(ctx, key, source, size)
			// Читателя закрываем всегда: если хранилище оборвало запись на
			// середине, пишущая сторона иначе повиснет на трубе навсегда.
			_ = source.CloseWithError(err)
			results <- result{backend: b, written: written, err: err}
		}(backend, pr)
	}

	// io.MultiWriter cannot be used here: it stops at the first failed mirror
	// and would truncate the primary as well. Feed each pipe explicitly and
	// retire only the failed mirror; a primary failure still aborts the source.
	buf := make([]byte, 256<<10)
	var copyErr error
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			for _, pipe := range pipes {
				if !pipe.active {
					continue
				}
				written, writeErr := pipe.writer.Write(buf[:n])
				if writeErr == nil && written == n {
					continue
				}
				if writeErr == nil {
					writeErr = io.ErrShortWrite
				}
				pipe.active = false
				_ = pipe.writer.CloseWithError(writeErr)
				if pipe.backend == m.primary {
					copyErr = writeErr
					break
				}
				m.note(pipe.backend.Name(), writeErr)
			}
		}
		if copyErr != nil {
			break
		}
		if readErr != nil {
			if readErr != io.EOF {
				copyErr = readErr
			}
			break
		}
	}
	for _, pipe := range pipes {
		if pipe.active {
			_ = pipe.writer.CloseWithError(copyErr)
		}
	}

	var (
		primaryWritten int64
		primaryErr     error
	)
	for range targets {
		res := <-results
		if res.backend == m.primary {
			primaryWritten, primaryErr = res.written, res.err
			continue
		}
		if res.err != nil {
			m.note(res.backend.Name(), res.err)
		}
	}

	if primaryErr != nil {
		return primaryWritten, primaryErr
	}
	// Ошибка чтения источника — это провал запуска целиком, независимо от
	// того, что успели принять хранилища.
	if copyErr != nil {
		return primaryWritten, copyErr
	}
	return primaryWritten, nil
}

// Delete удаляет объект везде. Ошибка основного хранилища возвращается,
// зеркала лишь запоминаются: мусор в зеркале хуже, чем ошибка, но несравнимо
// лучше, чем непройденная ретенция в основном.
func (m *Mirror) Delete(ctx context.Context, key string) error {
	for _, mirror := range m.healthy() {
		if err := mirror.Delete(ctx, key); err != nil {
			m.note(mirror.Name(), err)
		}
	}
	return m.primary.Delete(ctx, key)
}

func (m *Mirror) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	for _, mirror := range m.healthy() {
		if _, err := mirror.DeletePrefix(ctx, prefix); err != nil {
			m.note(mirror.Name(), err)
		}
	}
	return m.primary.DeletePrefix(ctx, prefix)
}

// Чтение и сведения — только из основного.
func (m *Mirror) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return m.primary.Get(ctx, key)
}

func (m *Mirror) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	return m.primary.GetRange(ctx, key, offset, length)
}

func (m *Mirror) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	return m.primary.Stat(ctx, key)
}

func (m *Mirror) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	return m.primary.List(ctx, prefix)
}

func (m *Mirror) Usage(ctx context.Context) (int64, int64, error) {
	return m.primary.Usage(ctx)
}

// Check проверяет все хранилища: зеркало, недоступное на проверке, лучше
// увидеть до запуска, а не в середине ночного окна.
func (m *Mirror) Check(ctx context.Context) error {
	if err := m.primary.Check(ctx); err != nil {
		return err
	}
	var problems []error
	for _, mirror := range m.mirrors {
		if err := mirror.Check(ctx); err != nil {
			problems = append(problems, fmt.Errorf("зеркало %s: %w", mirror.Name(), err))
		}
	}
	return errors.Join(problems...)
}

func (m *Mirror) Close() error {
	var problems []error
	for _, mirror := range m.mirrors {
		if err := mirror.Close(); err != nil {
			problems = append(problems, err)
		}
	}
	if err := m.primary.Close(); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}
