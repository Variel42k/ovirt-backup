package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"

	"github.com/Variel42k/ovirt-backup/internal/secret"
)

// codec turns a plaintext chunk into the bytes stored in the blob and back.
//
// Compression is applied per chunk rather than over the whole stream: a chunk
// has to be independently readable, because restore and verification address
// chunks by offset and must not decompress a terabyte to reach the last one.
type codec struct {
	compression string
	algo        compressor     // nil — сжатие выключено
	cipher      *secret.Cipher // nil — шифрование выключено
}

// compressor is one algorithm.
//
// Отдельный интерфейс, а не ветвления по имени в encode/decode: алгоритм
// выбирается один раз при создании кодека, и добавление ещё одного не трогает
// путь данных, где ошибка стоит дороже всего — испорченный чанк заметен только
// при восстановлении.
//
// Реализации рассчитаны на последовательное использование: чанки одного диска
// пишутся строго по возрастанию индекса, у каждого диска свой кодек. Там, где
// алгоритм требует состояния (gzip), оно берётся из пула — так безопасно и при
// параллельном чтении цепочки.
type compressor interface {
	compress(plain []byte) ([]byte, error)
	decompress(stored []byte, wantLen int) ([]byte, error)
	close()
}

func newCodec(compression string, level int, cipher *secret.Cipher) (*codec, error) {
	c := &codec{compression: compression, cipher: cipher}
	if compression != "" && compression != CompressionNone && (level < 1 || level > 9) {
		return nil, fmt.Errorf("уровень сжатия должен быть от 1 до 9, получено %d", level)
	}
	switch compression {
	case "", CompressionNone:
		return c, nil
	case CompressionZstd:
		algo, err := newZstdCompressor(level)
		if err != nil {
			return nil, err
		}
		c.algo = algo
	case CompressionGzip:
		c.algo = newGzipCompressor(level)
	case CompressionS2:
		c.algo = newS2Compressor(level)
	default:
		// Раньше неизвестное имя молча означало «без сжатия», и такой кодек
		// отдавал бы сжатые байты как готовые данные. Отказ громче и дешевле.
		return nil, fmt.Errorf("неизвестный алгоритм сжатия %q (доступны: %s)",
			compression, strings.Join(Compressions, ", "))
	}
	return c, nil
}

func (c *codec) close() {
	if c.algo != nil {
		c.algo.close()
	}
}

// encode returns the stored representation of a plaintext chunk and the hex
// SHA-256 of the plaintext.
func (c *codec) encode(plain []byte) (stored []byte, digest string, err error) {
	sum := sha256.Sum256(plain)
	digest = hex.EncodeToString(sum[:])

	out := plain
	if c.algo != nil {
		compressed, err := c.algo.compress(plain)
		if err != nil {
			return nil, "", fmt.Errorf("сжатие чанка (%s): %w", c.compression, err)
		}
		// Incompressible data (already-compressed guest filesystems, encrypted
		// volumes) would otherwise grow by the frame header on every chunk.
		if len(compressed) < len(plain) {
			out = compressed
		} else {
			out = plain
		}
	}
	if c.cipher != nil {
		sealed, err := c.cipher.EncryptBytes(out)
		if err != nil {
			return nil, "", fmt.Errorf("шифрование чанка: %w", err)
		}
		out = sealed
	}
	return out, digest, nil
}

// decode reverses encode. wantLen is the plaintext length recorded in the
// manifest and is used to tell a stored-as-is chunk from a compressed one.
func (c *codec) decode(stored []byte, wantLen int) ([]byte, error) {
	buf := stored
	if c.cipher != nil {
		plain, err := c.cipher.DecryptBytes(buf)
		if err != nil {
			return nil, fmt.Errorf("расшифровка чанка: %w "+
				"(вероятно, ключ шифрования не тот, которым делался бэкап)", err)
		}
		buf = plain
	}
	if c.algo != nil && len(buf) != wantLen {
		// Equal lengths mean the encoder decided compression was not worth it.
		plain, err := c.algo.decompress(buf, wantLen)
		if err != nil {
			return nil, fmt.Errorf("распаковка чанка (%s): %w", c.compression, err)
		}
		buf = plain
	}
	if len(buf) != wantLen {
		return nil, fmt.Errorf("длина чанка после распаковки %d, ожидалось %d", len(buf), wantLen)
	}
	return buf, nil
}

// verifyDigest recomputes the plaintext hash and compares it with the manifest.
func verifyDigest(plain []byte, want string) error {
	sum := sha256.Sum256(plain)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("контрольная сумма чанка не совпала: в хранилище %s, в манифесте %s", got, want)
	}
	return nil
}

// zstd — плотное сжатие при разумной скорости, выбор по умолчанию.
type zstdCompressor struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

func newZstdCompressor(level int) (*zstdCompressor, error) {
	encLevel := zstd.SpeedDefault
	switch {
	case level <= 1:
		encLevel = zstd.SpeedFastest
	case level >= 9:
		encLevel = zstd.SpeedBestCompression
	case level >= 6:
		encLevel = zstd.SpeedBetterCompression
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(encLevel), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		enc.Close()
		return nil, err
	}
	return &zstdCompressor{enc: enc, dec: dec}, nil
}

func (z *zstdCompressor) compress(plain []byte) ([]byte, error) {
	return z.enc.EncodeAll(plain, make([]byte, 0, len(plain)/2+64)), nil
}

func (z *zstdCompressor) decompress(stored []byte, wantLen int) ([]byte, error) {
	return z.dec.DecodeAll(stored, make([]byte, 0, wantLen))
}

func (z *zstdCompressor) close() {
	_ = z.enc.Close()
	z.dec.Close()
}

// gzip — на копию уходит больше процессора, чем на zstd, при худшей плотности.
// Смысл в другом: получившийся чанк распакует что угодно, вплоть до gunzip в
// busybox на аварийном загрузочном носителе, где ни этой программы, ни zstd не
// будет.
type gzipCompressor struct {
	level int
	// Writer у gzip — с состоянием, и один на кодек не годится: цепочку при
	// восстановлении читают параллельно. Пул даёт и безопасность, и повторное
	// использование буферов.
	writers sync.Pool
	readers sync.Pool
}

func newGzipCompressor(level int) *gzipCompressor {
	switch {
	case level <= 0:
		level = gzip.DefaultCompression
	case level > gzip.BestCompression:
		level = gzip.BestCompression
	}
	return &gzipCompressor{level: level}
}

func (g *gzipCompressor) compress(plain []byte) ([]byte, error) {
	buf := bytes.NewBuffer(make([]byte, 0, len(plain)/2+64))

	zw, _ := g.writers.Get().(*gzip.Writer)
	if zw == nil {
		var err error
		if zw, err = gzip.NewWriterLevel(buf, g.level); err != nil {
			return nil, err
		}
	} else {
		zw.Reset(buf)
	}

	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	g.writers.Put(zw)
	return buf.Bytes(), nil
}

func (g *gzipCompressor) decompress(stored []byte, wantLen int) ([]byte, error) {
	src := bytes.NewReader(stored)

	zr, _ := g.readers.Get().(*gzip.Reader)
	var err error
	if zr == nil {
		zr, err = gzip.NewReader(src)
	} else {
		err = zr.Reset(src)
	}
	if err != nil {
		return nil, err
	}

	// Читаем не больше чанка плюс байт: длину проверит вызывающий, а предел
	// не даёт подсунутому объекту хранилища развернуться в память целиком.
	out := bytes.NewBuffer(make([]byte, 0, wantLen))
	if _, err := io.Copy(out, io.LimitReader(zr, int64(wantLen)+1)); err != nil {
		return nil, err
	}
	if err := zr.Close(); err != nil {
		return nil, err
	}
	g.readers.Put(zr)
	return out.Bytes(), nil
}

func (g *gzipCompressor) close() {}

// s2 — сжатие для случая, когда узкое место процессор, а не диск: раза в три
// быстрее zstd на запись и заметно быстрее на чтение, плотность ниже. Уместно
// на гипервизоре, который и так занят, или когда канал до хранилища быстрый.
type s2Compressor struct{ level int }

func newS2Compressor(level int) *s2Compressor { return &s2Compressor{level: level} }

func (s *s2Compressor) compress(plain []byte) ([]byte, error) {
	dst := make([]byte, 0, s2.MaxEncodedLen(len(plain)))
	switch {
	case s.level >= 9:
		return s2.EncodeBest(dst, plain), nil
	case s.level >= 6:
		return s2.EncodeBetter(dst, plain), nil
	default:
		return s2.Encode(dst, plain), nil
	}
}

func (s *s2Compressor) decompress(stored []byte, wantLen int) ([]byte, error) {
	return s2.Decode(make([]byte, 0, wantLen), stored)
}

func (s *s2Compressor) close() {}
