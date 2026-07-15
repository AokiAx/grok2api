// Package security provides credential encryption for at-rest tokens.
// Cipher uses AES-256-GCM with an explicit enc:v1: envelope.
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

// EnvelopePrefix marks values sealed by this package. All new writes use it so
// ciphertext is never confused with OAuth tokens that also look like Base64.
const EnvelopePrefix = "enc:v1:"

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

// Encrypt seals plaintext and returns EnvelopePrefix + Base64(nonce||ciphertext).
// Already-enveloped values are returned unchanged. Legacy bare ciphertext from
// earlier builds is re-sealed under the envelope so storage is unambiguous.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if c == nil || c.aead == nil {
		return plaintext, nil
	}
	if IsEncrypted(plaintext) {
		return plaintext, nil
	}
	// Upgrade path: earlier builds stored raw Base64(nonce||ct) without a prefix.
	if plain, err := c.openRaw(plaintext); err == nil {
		return c.seal(plain)
	}
	return c.seal(plaintext)
}

// Decrypt opens an enveloped credential. Plaintext (no envelope) is returned
// as-is so unencrypted rows still load. Bare legacy ciphertext is not accepted
// here; use NormalizeStored when reading mixed storage formats.
func (c *Cipher) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	if !IsEncrypted(encoded) {
		if c == nil || c.aead == nil {
			return encoded, nil
		}
		return encoded, nil
	}
	if c == nil || c.aead == nil {
		return "", errors.New("encrypted credential present but credential_key is not configured")
	}
	return c.openRaw(strings.TrimPrefix(encoded, EnvelopePrefix))
}

// IsEncrypted reports whether value uses the explicit enc:v1: envelope.
// Length/Base64 heuristics are intentionally avoided: OAuth tokens are often
// long Base64 and must never be treated as ciphertext.
func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, EnvelopePrefix)
}

// NormalizeStored decrypts current envelopes, upgrades/opens legacy bare
// ciphertext when a key is configured, and otherwise returns plaintext.
func (c *Cipher) NormalizeStored(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if IsEncrypted(value) {
		return c.Decrypt(value)
	}
	if c == nil || c.aead == nil {
		return value, nil
	}
	// Legacy bare ciphertext (pre-envelope). GCM authentication rejects random
	// Base64 OAuth tokens, so failure means "treat as plaintext".
	if plain, err := c.openRaw(value); err == nil {
		return plain, nil
	}
	return value, nil
}

func (c *Cipher) seal(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return EnvelopePrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (c *Cipher) openRaw(encoded string) (string, error) {
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		// Some writers may have used padded StdEncoding; accept both.
		data, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("parse encrypted credential: %w", err)
		}
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
