package security

import (
	"crypto/sha256"
	"errors"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"golang.org/x/crypto/bcrypt"
)

func HashAdminPassword(password string, cost int) (adminauth.PasswordCredential, error) {
	if password == "" {
		return adminauth.PasswordCredential{}, errors.New("admin password is required")
	}
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return adminauth.PasswordCredential{}, errors.New("bcrypt cost is outside the supported range")
	}
	digest := sha256.Sum256([]byte(password))
	hash, err := bcrypt.GenerateFromPassword(digest[:], cost)
	if err != nil {
		return adminauth.PasswordCredential{}, err
	}
	credential := adminauth.PasswordCredential{
		Scheme: adminauth.PasswordSchemeBcryptSHA256V1,
		Hash:   string(hash),
	}
	if err := credential.Validate(); err != nil {
		return adminauth.PasswordCredential{}, err
	}
	return credential, nil
}

func VerifyAdminPassword(credential adminauth.PasswordCredential, password string) bool {
	if password == "" || credential.Validate() != nil {
		return false
	}
	digest := sha256.Sum256([]byte(password))
	return bcrypt.CompareHashAndPassword([]byte(credential.Hash), digest[:]) == nil
}
