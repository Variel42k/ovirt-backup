package dispatch

import (
	"bytes"
	"context"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// Краевые случаи сериализации образа.
//
// Каждый случай может исказить размер или содержимое образа без ошибки чтения.
// Тогда проверка сообщит, что гость не загрузился, хотя дефект в читателе образа.

// writeSized сохраняет запуск на диске, размер которого не является целым числом чанков.
func writeSized(t *testing.T, backend repo.Backend, key string, virtual int64,
	chunks map[int64][]byte) *backup.DiskManifest {
	t.Helper()

	m := backup.DiskManifest{DiskID: "vm:vda", Alias: "vda", VirtualSize: virtual}
	w, err := backup.NewDiskWriter(context.Background(), &m, backup.WriterOptions{
		Backend: backend, DataKey: key, ChunkSize: chunkSize,
	})
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	grid := (virtual + chunkSize - 1) / chunkSize
	for i := int64(0); i < grid; i++ {
		if data, ok := chunks[i]; ok {
			if err := w.WriteChunk(i, data); err != nil {
				t.Fatalf("chunk %d: %v", i, err)
			}
		}
	}
	final, err := w.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	return final
}

// Диск, размер которого не кратен размеру чанка, имеет короткий последний чанк. Написание
// полного чанка там сделало бы образ длиннее диска.
func TestWriteImageHandlesShortTailChunk(t *testing.T) {
	backend := testBackend(t)
	const virtual = chunkSize*5 + chunkSize/2 // 5,5 чанка

	tail := fill(0xEE)[:chunkSize/2]
	manifest := writeSized(t, backend, "runs/a/vda.data", virtual, map[int64][]byte{
		0: fill(0xAA),
		5: tail,
	})

	reader, err := backup.NewChainReader(backend, testCipher(t), []*backup.DiskManifest{manifest})
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer reader.Close()

	var buf bytes.Buffer
	written, err := writeImage(context.Background(), reader, &buf, nil)
	if err != nil {
		t.Fatalf("writeImage: %v", err)
	}

	if written != virtual {
		t.Errorf("записано %d байт при логическом размере %d", written, virtual)
	}
	if int64(buf.Len()) != virtual {
		t.Fatalf("длина образа %d, ожидалось %d", buf.Len(), virtual)
	}
	if !bytes.Equal(buf.Bytes()[5*chunkSize:], tail) {
		t.Error("короткий хвостовой чанк записан неверно")
	}
}

// Если хвост образа разрежен, передаём нули и для него. Иначе образ окажется короче, а
// таблица разделов в конце диска GPT пропадёт.
func TestWriteImageHandlesShortTailHole(t *testing.T) {
	backend := testBackend(t)
	const virtual = chunkSize*3 + 1024

	manifest := writeSized(t, backend, "runs/a/vda.data", virtual, map[int64][]byte{
		0: fill(0xAA),
	})

	reader, err := backup.NewChainReader(backend, testCipher(t), []*backup.DiskManifest{manifest})
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer reader.Close()

	var buf bytes.Buffer
	written, err := writeImage(context.Background(), reader, &buf, nil)
	if err != nil {
		t.Fatalf("writeImage: %v", err)
	}
	if written != virtual || int64(buf.Len()) != virtual {
		t.Fatalf("образ %d байт (записано %d), ожидалось %d", buf.Len(), written, virtual)
	}
	if !bytes.Equal(buf.Bytes()[chunkSize:], make([]byte, virtual-chunkSize)) {
		t.Error("хвостовая дыра заполнена не нулями")
	}
}

// Между запусками диск был увеличен. Используем новый размер, а добавленную область считаем
// невыделенной. Обрезка до старого размера потеряла бы данные, записанные гостем после
// расширения.
func TestWriteImageCoversExtendedDisk(t *testing.T) {
	backend := testBackend(t)

	small := writeSized(t, backend, "runs/a/vda.data", chunkSize*2, map[int64][]byte{
		0: fill(0x11),
	})
	grown := writeSized(t, backend, "runs/b/vda.data", chunkSize*4, map[int64][]byte{
		3: fill(0x44),
	})

	reader, err := backup.NewChainReader(backend, testCipher(t), []*backup.DiskManifest{small, grown})
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer reader.Close()

	var buf bytes.Buffer
	if _, err := writeImage(context.Background(), reader, &buf, nil); err != nil {
		t.Fatalf("writeImage: %v", err)
	}

	if int64(buf.Len()) != chunkSize*4 {
		t.Fatalf("длина образа %d, ожидалось %d", buf.Len(), chunkSize*4)
	}
	img := buf.Bytes()
	if !bytes.Equal(img[:chunkSize], fill(0x11)) {
		t.Error("данные из первого запуска потеряны")
	}
	if !bytes.Equal(img[3*chunkSize:4*chunkSize], fill(0x44)) {
		t.Error("данные в выросшей области потеряны")
	}
}

// Резервная копия полностью пустого диска является законным случаем (новосозданный том). Он
// должен по-прежнему создавать полноразмерное образ нулей.
func TestWriteImageOfEmptyDisk(t *testing.T) {
	backend := testBackend(t)
	manifest := writeSized(t, backend, "runs/a/vda.data", chunkSize*3, map[int64][]byte{})

	reader, err := backup.NewChainReader(backend, testCipher(t), []*backup.DiskManifest{manifest})
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer reader.Close()

	var buf bytes.Buffer
	written, err := writeImage(context.Background(), reader, &buf, nil)
	if err != nil {
		t.Fatalf("writeImage: %v", err)
	}
	if written != chunkSize*3 {
		t.Errorf("записано %d, ожидалось %d", written, chunkSize*3)
	}
	if !bytes.Equal(buf.Bytes(), make([]byte, chunkSize*3)) {
		t.Error("образ пустого диска должен быть нулями нужной длины")
	}
}

// Отмена проверки должна остановить передачу, а не продолжать нажимать терабайт на
// гипервизор, которого никто больше не ждет.
func TestWriteImageStopsOnCancel(t *testing.T) {
	backend := testBackend(t)
	manifest := writeSized(t, backend, "runs/a/vda.data", chunkSize*8, map[int64][]byte{
		0: fill(0x11), 1: fill(0x22), 2: fill(0x33),
	})

	reader, err := backup.NewChainReader(backend, testCipher(t), []*backup.DiskManifest{manifest})
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer reader.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	if _, err := writeImage(ctx, reader, &buf, nil); err == nil {
		t.Fatal("отменённая передача должна возвращать ошибку")
	}
}
