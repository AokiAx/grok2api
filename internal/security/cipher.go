// Package security provides credential encryption for at-rest tokens.
// Cipher uses AES-256-GCM with a raw Base64 wire format.
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Cipher encrypts OAuth credentials at rest with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher creates a cipher from a Base64-encoded 32-byte key.
// Empty key returns (nil, nil) so single-node deployments can keep plaintext.
func NewCipher(encodedKey string) (*Cipher, error) {
	encodedKey = strings.TrimSpace(encodedKey)
	if encodedKey == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("parse credential encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("credential encryption key must be a Base64-encoded 32-byte key")
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

// Encrypt encrypts sensitive plaintext and returns a Base64 string.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if c == nil || c.aead == nil {
		return plaintext, nil
	}
	if IsEncrypted(plaintext) {
		// Already ciphertext — do not double-encrypt.
		return plaintext, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt decrypts a Base64 ciphertext.
// Plaintext values (legacy rows) are returned as-is when they do not look encrypted.
func (c *Cipher) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	if c == nil || c.aead == nil {
		if IsEncrypted(encoded) {
			return "", errors.New("encrypted credential present but credential_key is not configured")
		}
		return encoded, nil
	}
	if !IsEncrypted(encoded) {
		return encoded, nil
	}
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("parse encrypted credential: %w", err)
	}
	if len(data) < c.aead.NonceSize() {
		return "", fmt.Errorf("encrypted credential length invalid")
	}
	nonce, ciphertext := data[:c.aead.NonceSize()], data[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt credential: %w", err)
	}
	return string(plain), nil
}

// IsEncrypted reports whether value looks like AES-GCM ciphertext we produce
// (raw Base64 of nonce||ciphertext). Heuristic for migration / double-encrypt guard.
func IsEncrypted(value string) bool {
	if value == "" {
		return false
	}
	// Legacy prefix from earlier builds — still treated as encrypted payload after strip.
	if strings.HasPrefix(value, "enc:v1:") {
		return true
	}
	data, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return false
	}
	// GCM nonce is typically 12; tag 16 → ciphertext at least 28 bytes for empty plain.
	return len(data) >= 28
}

// NormalizeStored decrypts both raw Base64 and legacy enc:v1: rows.
func (c *Cipher) NormalizeStored(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "enc:v1:") {
		// Legacy format: enc:v1: + raw base64 payload.
		return c.Decrypt(strings.TrimPrefix(value, "enc:v1:"))
	}
	return c.Decrypt(value)
}
