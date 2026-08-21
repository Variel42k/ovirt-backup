package backup

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

// compressibleChunk imitates a chunk of a real filesystem: repeating structure
// with a bit of variety. Полностью случайные байты для проверки сжатия не
// годятся — они не сжимаются, и кодек законно вернул бы их как есть, а тогда
// ветка распаковки в тесте не выполнилась бы ни разу.
func compressibleChunk(size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(i/64 + i%7)
	}
	return out
}

func TestCodecRoundTripEveryCompression(t *testing.T) {
	plain := compressibleChunk(testChunkSize)

	for _, name := range Compressions {
		for _, level := range []int{1, 3, 9} {
			t.Run(name+"/level"+string(rune('0'+level)), func(t *testing.T) {
				c, err := newCodec(name, level, nil)
				if err != nil {
					t.Fatalf("newCodec(%q): %v", name, err)
				}
				defer c.close()

				stored, digest, err := c.encode(plain)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				if name != CompressionNone && len(stored) >= len(plain) {
					t.Errorf("%s не сжал предсказуемые данные: %d байт из %d", name, len(stored), len(plain))
				}

				got, err := c.decode(stored, len(plain))
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if !bytes.Equal(got, plain) {
					t.Fatal("после распаковки данные отличаются от исходных")
				}
				if err := verifyDigest(got, digest); err != nil {
					t.Errorf("контрольная сумма: %v", err)
				}
			})
		}
	}
}

// Несжимаемый чанк хранится как есть — иначе каждый такой чанк рос бы на
// заголовок формата, а на диске с уже сжатой или шифрованной гостевой ФС такие
// чанки составляют почти всё.
func TestCodecStoresIncompressibleChunkAsIs(t *testing.T) {
	plain := make([]byte, testChunkSize)
	if _, err := rand.Read(plain); err != nil {
		t.Fatalf("rand: %v", err)
	}

	for _, name := range Compressions {
		c, err := newCodec(name, 3, nil)
		if err != nil {
			t.Fatalf("newCodec(%q): %v", name, err)
		}
		stored, _, err := c.encode(plain)
		if err != nil {
			t.Fatalf("encode(%s): %v", name, err)
		}
		if len(stored) != len(plain) {
			t.Errorf("%s: случайные данные заняли %d байт вместо %d", name, len(stored), len(plain))
		}
		got, err := c.decode(stored, len(plain))
		if err != nil {
			t.Fatalf("decode(%s): %v", name, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("%s: данные искажены", name)
		}
		c.close()
	}
}

// Неизвестный алгоритм должен приводить к отказу, а не к молчаливому «без
// сжатия»: такой кодек отдал бы сжатые байты как готовые данные, и заметить это
// удалось бы только при восстановлении.
func TestCodecRejectsUnknownCompression(t *testing.T) {
	if _, err := newCodec("lzma", 3, nil); err == nil {
		t.Fatal("неизвестный алгоритм принят молча")
	}
	if KnownCompression("lzma") {
		t.Error("KnownCompression считает lzma известным")
	}
}

func TestCodecRejectsInvalidCompressionLevel(t *testing.T) {
	for _, level := range []int{-1, 0, 10} {
		if _, err := newCodec(CompressionZstd, level, nil); err == nil {
			t.Errorf("уровень %d принят", level)
		}
	}
}

func TestCodecEncryptionAndCorruptionEveryCompression(t *testing.T) {
	cipher, err := secret.New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	plain := compressibleChunk(testChunkSize)

	for _, name := range Compressions {
		t.Run(name, func(t *testing.T) {
			c, err := newCodec(name, 3, cipher)
			if err != nil {
				t.Fatalf("new codec: %v", err)
			}
			defer c.close()
			stored, _, err := c.encode(plain)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := c.decode(stored, len(plain))
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("encrypted round trip: %v", err)
			}
			stored[len(stored)/2] ^= 0xff
			if _, err := c.decode(stored, len(plain)); err == nil {
				t.Fatal("повреждённый шифротекст принят")
			}
		})
	}
}

func TestCodecRejectsTruncatedDataEveryCompression(t *testing.T) {
	plain := compressibleChunk(testChunkSize)
	for _, name := range Compressions {
		c, err := newCodec(name, 3, nil)
		if err != nil {
			t.Fatalf("newCodec(%q): %v", name, err)
		}
		stored, _, err := c.encode(plain)
		if err != nil {
			t.Fatalf("encode(%q): %v", name, err)
		}
		if _, err := c.decode(stored[:len(stored)/2], len(plain)); err == nil {
			t.Errorf("%s принял обрезанный чанк", name)
		}
		c.close()
	}
}

// Список в конфигурации продублирован строкой (иначе пакеты замкнулись бы друг
// на друга), поэтому расхождение ловится тестом.
func TestConfigAcceptsEveryCompression(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("конфигурация по умолчанию: %v", err)
	}

	for _, name := range Compressions {
		cfg.Backup.Compression = name
		if err := cfg.Validate(); err != nil {
			t.Errorf("конфигурация не принимает %q, хотя кодек его поддерживает: %v", name, err)
		}
	}

	cfg.Backup.Compression = "lzma"
	if err := cfg.Validate(); err == nil {
		t.Error("конфигурация приняла алгоритм, которого нет в кодеке")
	}
}
