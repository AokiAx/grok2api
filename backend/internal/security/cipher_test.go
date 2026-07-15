package security_test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/security"
)

func testKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestCipherRoundTripUsesExplicitEnvelope(t *testing.T) {
	key := testKey(t)
	c, err := security.NewCipher(key)
	if err != nil || c == nil {
		t.Fatalf("cipher: %v %#v", err, c)
	}
	plain := "sk-access-token-example"
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if enc == plain {
		t.Fatal("ciphertext equals plaintext")
	}
	if !strings.HasPrefix(enc, security.EnvelopePrefix) {
		t.Fatalf("missing envelope prefix: %q", enc)
	}
	if !security.IsEncrypted(enc) {
		t.Fatalf("IsEncrypted false for %q", enc)
	}
	// Idempotent encrypt.
	enc2, err := c.Encrypt(enc)
	if err != nil || enc2 != enc {
		t.Fatalf("re-encrypt: %v %q", err, enc2)
	}
	got, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plain {
		t.Fatalf("got %q", got)
	}
}

func TestIsEncryptedRejectsBareBase64Tokens(t *testing.T) {
	// OAuth-shaped values are often long Base64; they must never look encrypted.
	token := base64.RawStdEncoding.EncodeToString(bytesRepeat(0x41, 64))
	if security.IsEncrypted(token) {
		t.Fatalf("plain base64 token treated as ciphertext: %q", token)
	}
	if security.IsEncrypted("sk-not-base64-but-long-enough-to-trick-old-heuristic-xx") {
		t.Fatal("non-envelope value treated as ciphertext")
	}
}

func TestNewCipherRequiresBase64Key(t *testing.T) {
	if _, err := security.NewCipher("not-base64-32"); err == nil {
		t.Fatal("expected error for non-base64 key")
	}
	// 16-byte key rejected.
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := security.NewCipher(short); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestNilCipherPassthrough(t *testing.T) {
	var c *security.Cipher
	enc, err := c.Encrypt("plain")
	if err != nil || enc != "plain" {
		t.Fatalf("nil encrypt: %v %q", err, enc)
	}
	dec, err := c.Decrypt("plain")
	if err != nil || dec != "plain" {
		t.Fatalf("nil decrypt: %v %q", err, dec)
	}
}

func TestEmptyPassphrase(t *testing.T) {
	c, err := security.NewCipher("  ")
	if err != nil || c != nil {
		t.Fatalf("want nil cipher, got %#v %v", c, err)
	}
}

func TestLegacyPrefixDecrypt(t *testing.T) {
	key := testKey(t)
	c, err := security.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.Encrypt("legacy-token")
	if err != nil {
		t.Fatal(err)
	}
	// Encrypt already writes enc:v1:...; also accept double-prefix free form used
	// by older tests that stored enc:v1: + body where body itself is enveloped.
	got, err := c.NormalizeStored(body)
	if err != nil || got != "legacy-token" {
		t.Fatalf("enveloped: %v %q", err, got)
	}
}

func TestLegacyBareCiphertextNormalizeAndUpgrade(t *testing.T) {
	rawKey := bytesRepeat(0x3c, 32)
	key := base64.StdEncoding.EncodeToString(rawKey)
	c, err := security.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := sealBare(rawKey, "legacy-bare-token")
	if err != nil {
		t.Fatal(err)
	}
	if security.IsEncrypted(bare) {
		t.Fatal("bare ciphertext must not report as enveloped")
	}
	got, err := c.NormalizeStored(bare)
	if err != nil || got != "legacy-bare-token" {
		t.Fatalf("normalize bare: %v %q", err, got)
	}
	upgraded, err := c.Encrypt(bare)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(upgraded, security.EnvelopePrefix) {
		t.Fatalf("upgrade missing envelope: %q", upgraded)
	}
	got, err = c.Decrypt(upgraded)
	if err != nil || got != "legacy-bare-token" {
		t.Fatalf("decrypt upgraded: %v %q", err, got)
	}
}

func TestCipherRejectsCiphertextFromDifferentKey(t *testing.T) {
	first, err := security.NewCipher(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := security.NewCipher(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := first.Encrypt("credential-owned-by-first-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Decrypt(ciphertext); err == nil || !strings.Contains(err.Error(), "decrypt credential") {
		t.Fatalf("wrong-key decrypt error = %v", err)
	}
}

func TestNilCipherRejectsExplicitLegacyEnvelope(t *testing.T) {
	var c *security.Cipher
	if _, err := c.NormalizeStored("enc:v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); err == nil {
		t.Fatal("expected missing-key error")
	}
}

func TestCipherEmptyCredentialRoundTrip(t *testing.T) {
	c, err := security.NewCipher(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := c.Encrypt("")
	if err != nil || encrypted != "" {
		t.Fatalf("encrypt empty = %q, %v", encrypted, err)
	}
	decrypted, err := c.Decrypt("")
	if err != nil || decrypted != "" {
		t.Fatalf("decrypt empty = %q, %v", decrypted, err)
	}
}

func TestLongBase64PlaintextStillEncryptsAsEnvelope(t *testing.T) {
	c, err := security.NewCipher(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	// Looks like ciphertext under the old length heuristic, but is plaintext.
	plain := base64.RawStdEncoding.EncodeToString(bytesRepeat(0x7e, 48))
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, security.EnvelopePrefix) {
		t.Fatalf("want envelope, got %q", enc)
	}
	got, err := c.Decrypt(enc)
	if err != nil || got != plain {
		t.Fatalf("round-trip long base64: %v %q", err, got)
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// sealBare reproduces the pre-envelope on-disk format: RawStdEncoding(nonce||ct).
func sealBare(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}
