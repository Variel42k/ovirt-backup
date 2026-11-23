// Пакет secret шифрует пароли oVirt и ключи S3 и SSH. Службе нужно использовать эти данные
// для аутентификации, поэтому их нельзя хранить только в виде хешей.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/config"
)

const keySize = 32 // AES-256

// Cipher выполняет аутентифицированное шифрование с помощью одного симметричного ключа.
type Cipher struct {
	aead cipher.AEAD
}

// New строит шифр из необработанного 32-байтного ключа.
func New(key []byte) (*Cipher, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("key must be %d bytes, got %d", keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// NewFromConfig разрешает ключ из конфигурации: встроенное значение base64 выигрывает, в
// противном случае файл ключа читается, и если он не существует, новый ключ генерируется и
// хранится с разрешениями только владельца.
func NewFromConfig(cfg config.SecretsConfig) (*Cipher, error) {
	if cfg.KeyBase64 != "" {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.KeyBase64))
		if err != nil {
			return nil, fmt.Errorf("decode secrets.key_base64: %w", err)
		}
		return New(key)
	}
	if cfg.KeyFile == "" {
		return nil, errors.New("secrets: neither key_base64 nor key_file is set")
	}

	raw, err := os.ReadFile(cfg.KeyFile)
	switch {
	case err == nil:
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("decode key file %s: %w", cfg.KeyFile, err)
		}
		return New(key)
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("read key file %s: %w", cfg.KeyFile, err)
	}

	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if dir := filepath.Dir(cfg.KeyFile); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create key dir: %w", err)
		}
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(cfg.KeyFile, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write key file %s: %w", cfg.KeyFile, err)
	}
	return New(key)
}

// Encrypt возвращает nonce||ciphertext в base64. Пустой ввод остаётся пустым, чтобы
// сохранить признак отсутствующего секрета.
func (c *Cipher) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt перешифровывается.
func (c *Cipher) Decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("secret blob is too short")
	}
	plain, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		// Почти всегда означает, что ключевой файл был заменен или восстановлен с другого
		// хоста; Скажите так, вместо того, чтобы слить ошибку auth.
		return "", fmt.Errorf("decrypt secret (ключ шифрования не совпадает с тем, которым данные были записаны): %w", err)
	}
	return string(plain), nil
}

// EncryptBytes запечатывает произвольные двоичные данные, используемые для резервного
// шифрования блоков.
func (c *Cipher) EncryptBytes(plain []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, nil), nil
}

// DecryptBytes открывает данные, произведенные EncryptBytes.
func (c *Cipher) DecryptBytes(sealed []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("sealed blob is too short")
	}
	return c.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
}

// Overhead сообщает, сколько байтов шифрования добавляется к каждому блоку.
func (c *Cipher) Overhead() int { return c.aead.NonceSize() + c.aead.Overhead() }
