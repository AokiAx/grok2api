// Package adminauth owns administrator credentials, refresh sessions, and
// login-attempt facts without depending on HTTP or a persistence technology.
package adminauth

import (
	"crypto/subtle"
	"errors"
	"strings"
	"time"
)

type PasswordScheme string

const PasswordSchemeBcryptSHA256V1 PasswordScheme = "bcrypt_sha256_v1"

type Role string

const RoleAdministrator Role = "administrator"

type PasswordCredential struct {
	Scheme PasswordScheme
	Hash   string
}

func (c PasswordCredential) Validate() error {
	if c.Scheme != PasswordSchemeBcryptSHA256V1 {
		return errors.New("unsupported password scheme")
	}
	if strings.TrimSpace(c.Hash) == "" {
		return errors.New("password hash is required")
	}
	return nil
}

type AdminUser struct {
	ID          string
	Username    string
	Password    PasswordCredential
	Role        Role
	Enabled     bool
	LastLoginAt time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func NewAdminUser(id, username string, credential PasswordCredential, at time.Time) (AdminUser, error) {
	item := AdminUser{
		ID:        strings.TrimSpace(id),
		Username:  normalizeUsername(username),
		Password:  credential,
		Role:      RoleAdministrator,
		Enabled:   true,
		CreatedAt: normalizeTime(at),
		UpdatedAt: normalizeTime(at),
	}
	if err := item.Validate(); err != nil {
		return AdminUser{}, err
	}
	return item, nil
}

func (u *AdminUser) Validate() error {
	if u == nil {
		return errors.New("admin user is required")
	}
	u.ID = strings.TrimSpace(u.ID)
	u.Username = normalizeUsername(u.Username)
	if u.ID == "" {
		return errors.New("admin user id is required")
	}
	if u.Username == "" {
		return errors.New("admin username is required")
	}
	if u.Role == "" {
		u.Role = RoleAdministrator
	}
	if u.Role != RoleAdministrator {
		return errors.New("unsupported admin role")
	}
	return u.Password.Validate()
}

func (u AdminUser) CanAuthenticate() bool {
	return u.Enabled && u.Password.Validate() == nil
}

type Session struct {
	ID                  string
	FamilyID            string
	AdminUserID         string
	AccessTokenHash     [32]byte
	RefreshSecretHash   [32]byte
	SourceIP            string
	UserAgent           string
	CreatedAt           time.Time
	AccessExpiresAt     time.Time
	ExpiresAt           time.Time
	LastSeenAt          time.Time
	RevokedAt           time.Time
	RotatedAt           time.Time
	ReplacedBySessionID string
	RevocationReason    RevocationReason
}

type RevocationReason string

const (
	RevocationLogout        RevocationReason = "logout"
	RevocationRotated       RevocationReason = "rotated"
	RevocationRefreshReplay RevocationReason = "refresh_replayed"
	RevocationAdminDisabled RevocationReason = "admin_disabled"
)

func NewSession(
	id, familyID, adminUserID string,
	accessTokenHash, refreshSecretHash [32]byte,
	accessExpiresAt, expiresAt, at time.Time,
) (Session, error) {
	at = normalizeTime(at)
	item := Session{
		ID:                strings.TrimSpace(id),
		FamilyID:          strings.TrimSpace(familyID),
		AdminUserID:       strings.TrimSpace(adminUserID),
		AccessTokenHash:   accessTokenHash,
		RefreshSecretHash: refreshSecretHash,
		CreatedAt:         at,
		LastSeenAt:        at,
		AccessExpiresAt:   accessExpiresAt.UTC(),
		ExpiresAt:         expiresAt.UTC(),
	}
	if item.ID == "" || item.FamilyID == "" || item.AdminUserID == "" {
		return Session{}, errors.New("session id, family id, and admin user id are required")
	}
	if item.AccessTokenHash == ([32]byte{}) || item.RefreshSecretHash == ([32]byte{}) {
		return Session{}, errors.New("access and refresh hashes are required")
	}
	if item.AccessExpiresAt.IsZero() || !item.AccessExpiresAt.After(item.CreatedAt) {
		return Session{}, errors.New("access expiry must be after creation")
	}
	if item.ExpiresAt.IsZero() || !item.ExpiresAt.After(item.CreatedAt) {
		return Session{}, errors.New("session expiry must be after creation")
	}
	if item.AccessExpiresAt.After(item.ExpiresAt) {
		return Session{}, errors.New("access expiry cannot exceed refresh session expiry")
	}
	return item, nil
}

func (s Session) Active(at time.Time) bool {
	at = normalizeTime(at)
	return s.RevokedAt.IsZero() && !s.ExpiresAt.IsZero() && at.Before(s.ExpiresAt)
}

func (s Session) AccessActive(at time.Time) bool {
	at = normalizeTime(at)
	return s.Active(at) && !s.AccessExpiresAt.IsZero() && at.Before(s.AccessExpiresAt)
}

func (s Session) MatchesAccessTokenHash(candidate [32]byte) bool {
	return subtle.ConstantTimeCompare(s.AccessTokenHash[:], candidate[:]) == 1
}

func (s Session) MatchesRefreshSecretHash(candidate [32]byte) bool {
	return subtle.ConstantTimeCompare(s.RefreshSecretHash[:], candidate[:]) == 1
}

func (s *Session) Revoke(at time.Time, reason RevocationReason) {
	if s == nil || !s.RevokedAt.IsZero() {
		return
	}
	s.RevokedAt = normalizeTime(at)
	s.RevocationReason = reason
}

func (s *Session) Rotate(at time.Time, replacementSessionID string) error {
	if s == nil {
		return errors.New("session is required")
	}
	replacementSessionID = strings.TrimSpace(replacementSessionID)
	if replacementSessionID == "" || replacementSessionID == s.ID {
		return errors.New("replacement session id is invalid")
	}
	if !s.RotatedAt.IsZero() || !s.RevokedAt.IsZero() {
		return errors.New("session is no longer rotatable")
	}
	at = normalizeTime(at)
	s.RotatedAt = at
	s.ReplacedBySessionID = replacementSessionID
	s.Revoke(at, RevocationRotated)
	return nil
}

type LoginAttempt struct {
	ID          int64
	Username    string
	SourceIP    string
	Succeeded   bool
	FailureCode string
	CreatedAt   time.Time
}

func NewLoginAttempt(username, sourceIP string, succeeded bool, failureCode string, at time.Time) (LoginAttempt, error) {
	item := LoginAttempt{
		Username:    normalizeUsername(username),
		SourceIP:    strings.TrimSpace(sourceIP),
		Succeeded:   succeeded,
		FailureCode: strings.TrimSpace(failureCode),
		CreatedAt:   normalizeTime(at),
	}
	if item.Username == "" {
		return LoginAttempt{}, errors.New("login username is required")
	}
	if succeeded && item.FailureCode != "" {
		return LoginAttempt{}, errors.New("successful login attempt cannot have a failure code")
	}
	if !succeeded && item.FailureCode == "" {
		return LoginAttempt{}, errors.New("failed login attempt requires a failure code")
	}
	return item, nil
}

func normalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}
