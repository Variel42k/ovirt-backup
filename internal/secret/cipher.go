// Package secret encrypts the credentials the service must keep in a usable
// form: oVirt passwords, S3 keys and SSH keys. Hashing is not an option here —
// the service has to replay these secrets to authenticate.
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

	"adveng/jh_virt/internal/config"
)

const keySize = 32 // AES-256

// Cipher performs authenticated encryption with a single symmetric key.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from a raw 32-byte key.
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

// NewFromConfig resolves the key from the config: an inline base64 value wins,
// otherwise the key file is read, and if it does not exist a fresh key is
// generated and stored with owner-only permissions.
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

// Encrypt returns a base64 blob of nonce||ciphertext. An empty input stays
// empty so that "no secret stored" survives a round trip.
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

// Decrypt reverses Encrypt.
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
		// Almost always means the key file was replaced or restored from a
		// different host; say so rather than leaking a generic auth error.
		return "", fmt.Errorf("decrypt secret (ключ шифрования не совпадает с тем, которым данные были записаны): %w", err)
	}
	return string(plain), nil
}

// EncryptBytes seals arbitrary binary data, used for backup chunk encryption.
func (c *Cipher) EncryptBytes(plain []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, nil), nil
}

// DecryptBytes opens data produced by EncryptBytes.
func (c *Cipher) DecryptBytes(sealed []byte) ([]byte, error) {
	ns := c.aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("sealed blob is too short")
	}
	return c.aead.Open(nil, sealed[:ns], sealed[ns:], nil)
}

// Overhead reports how many bytes encryption adds to each chunk.
func (c *Cipher) Overhead() int { return c.aead.NonceSize() + c.aead.Overhead() }
