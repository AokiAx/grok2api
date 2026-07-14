package security_test

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/AokiAx/grok2api/internal/security"
)

func testKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestCipherRoundTripRawBase64Format(t *testing.T) {
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
	// Simulate legacy enc:v1: + raw Base64 body.
	body, err := c.Encrypt("legacy-token")
	if err != nil {
		t.Fatal(err)
	}
	legacy := "enc:v1:" + body
	got, err := c.NormalizeStored(legacy)
	if err != nil || got != "legacy-token" {
		t.Fatalf("legacy: %v %q", err, got)
	}
}
