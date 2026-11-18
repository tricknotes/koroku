// Package secrets encrypts environment variable values at rest and masks
// secret values in anything that is written to logs.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the length of the master key in bytes (AES-256).
const KeySize = 32

// Box encrypts and decrypts values with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// NewBox builds a Box from a 32-byte key.
func NewBox(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init GCM: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext. The associated data binds the ciphertext to its
// owner (app and key name) so that rows cannot be swapped inside the database.
func (b *Box) Seal(plaintext []byte, associated string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, []byte(associated)), nil
}

// Open decrypts a value produced by Seal with the same associated data.
func (b *Box) Open(sealed []byte, associated string) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("decrypt: ciphertext too short")
	}
	plaintext, err := b.aead.Open(nil, sealed[:n], sealed[n:], []byte(associated))
	if err != nil {
		// The underlying error carries no detail worth exposing.
		return nil, errors.New("decrypt: authentication failed (wrong master key or corrupted value)")
	}
	return plaintext, nil
}

// LoadKey reads the master key file, which holds 64 hexadecimal characters.
// The file must not be readable by group or others.
func LoadKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("master key %s: permissions %04o are too open, want 0600", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != KeySize {
		return nil, fmt.Errorf("master key %s: want %d hex characters", path, KeySize*2)
	}
	return key, nil
}

// LoadOrCreateKey reads the master key, generating a new one (mode 0600) when
// the file does not exist yet.
func LoadOrCreateKey(path string) (key []byte, created bool, err error) {
	key, err = LoadKey(path)
	if err == nil {
		return key, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	key = make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, false, fmt.Errorf("generate master key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create master key directory: %w", err)
	}
	// O_EXCL: never overwrite a key that appeared in the meantime.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("create master key: %w", err)
	}
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("write master key: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, false, fmt.Errorf("write master key: %w", err)
	}
	return key, true, nil
}

// EnvVarContext is the associated data that binds an encrypted environment
// variable value to its row.
func EnvVarContext(appID int64, key string) string {
	return fmt.Sprintf("env:%d:%s", appID, key)
}
