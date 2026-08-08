package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/klauspost/compress/zstd"

	"adveng/jh_virt/internal/secret"
)

// codec turns a plaintext chunk into the bytes stored in the blob and back.
//
// Compression is applied per chunk rather than over the whole stream: a chunk
// has to be independently readable, because restore and verification address
// chunks by offset and must not decompress a terabyte to reach the last one.
type codec struct {
	compression string
	enc         *zstd.Encoder
	dec         *zstd.Decoder
	cipher      *secret.Cipher // nil — шифрование выключено
}

func newCodec(compression string, level int, cipher *secret.Cipher) (*codec, error) {
	c := &codec{compression: compression, cipher: cipher}
	if compression != CompressionZstd {
		return c, nil
	}

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
	c.enc, c.dec = enc, dec
	return c, nil
}

func (c *codec) close() {
	if c.enc != nil {
		_ = c.enc.Close()
	}
	if c.dec != nil {
		c.dec.Close()
	}
}

// encode returns the stored representation of a plaintext chunk and the hex
// SHA-256 of the plaintext.
func (c *codec) encode(plain []byte) (stored []byte, digest string, err error) {
	sum := sha256.Sum256(plain)
	digest = hex.EncodeToString(sum[:])

	out := plain
	if c.enc != nil {
		compressed := c.enc.EncodeAll(plain, make([]byte, 0, len(plain)/2+64))
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
	if c.dec != nil && len(buf) != wantLen {
		// Equal lengths mean the encoder decided compression was not worth it.
		plain, err := c.dec.DecodeAll(buf, make([]byte, 0, wantLen))
		if err != nil {
			return nil, fmt.Errorf("распаковка чанка: %w", err)
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
