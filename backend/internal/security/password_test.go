package security

import (
	"crypto/sha256"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"golang.org/x/crypto/bcrypt"
)

func TestAdminPasswordUsesBcryptOverSHA256Digest(t *testing.T) {
	credential, err := HashAdminPassword("panel-secret", bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if credential.Scheme != adminauth.PasswordSchemeBcryptSHA256V1 || credential.Hash == "" {
		t.Fatalf("credential = %+v", credential)
	}
	if cost, err := bcrypt.Cost([]byte(credential.Hash)); err != nil || cost != bcrypt.MinCost {
		t.Fatalf("bcrypt cost=%d err=%v", cost, err)
	}
	digest := sha256.Sum256([]byte("panel-secret"))
	if err := bcrypt.CompareHashAndPassword([]byte(credential.Hash), digest[:]); err != nil {
		t.Fatalf("bcrypt did not hash SHA-256 digest: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(credential.Hash), []byte("panel-secret")); err == nil {
		t.Fatal("bcrypt unexpectedly hashed the raw password")
	}
	if !VerifyAdminPassword(credential, "panel-secret") || VerifyAdminPassword(credential, "wrong") {
		t.Fatal("password verification result is incorrect")
	}
}

func TestAdminPasswordHashRejectsBlankPasswordAndInvalidCost(t *testing.T) {
	if _, err := HashAdminPassword("", bcrypt.MinCost); err == nil {
		t.Fatal("blank password should be rejected")
	}
	if _, err := HashAdminPassword("secret", bcrypt.MinCost-1); err == nil {
		t.Fatal("invalid bcrypt cost should be rejected")
	}
	if VerifyAdminPassword(adminauth.PasswordCredential{Scheme: adminauth.PasswordSchemeBcryptSHA256V1, Hash: "not-bcrypt"}, "secret") {
		t.Fatal("malformed credential should not verify")
	}
}
