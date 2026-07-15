package adminauth

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"
)

const validBcryptFixture = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func TestNewAdminUserNormalizesAndValidatesCredential(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.FixedZone("HKT", 8*60*60))
	user, err := NewAdminUser(
		" admin-1 ",
		" Admin ",
		PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1, Hash: validBcryptFixture},
		now,
	)
	if err != nil {
		t.Fatalf("new admin user: %v", err)
	}
	if user.ID != "admin-1" || user.Username != "admin" || user.Role != RoleAdministrator || !user.Enabled {
		t.Fatalf("normalized user = %+v", user)
	}
	if user.Password.Scheme != PasswordSchemeBcryptSHA256V1 || user.Password.Hash == "" {
		t.Fatalf("credential = %+v", user.Password)
	}
	if !user.CreatedAt.Equal(now.UTC()) || !user.UpdatedAt.Equal(now.UTC()) {
		t.Fatalf("timestamps = %v/%v; want %v", user.CreatedAt, user.UpdatedAt, now.UTC())
	}

	invalid := []struct {
		name       string
		id         string
		username   string
		credential PasswordCredential
	}{
		{name: "blank id", username: "admin", credential: PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1, Hash: validBcryptFixture}},
		{name: "blank username", id: "admin-1", credential: PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1, Hash: validBcryptFixture}},
		{name: "unsupported scheme", id: "admin-1", username: "admin", credential: PasswordCredential{Scheme: "plain", Hash: "secret"}},
		{name: "blank hash", id: "admin-1", username: "admin", credential: PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1}},
		{name: "malformed bcrypt hash", id: "admin-1", username: "admin", credential: PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1, Hash: "hash"}},
	}
	if _, err := NewAdminUser("admin-1", "admin", PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1, Hash: validBcryptFixture}, time.Time{}); err == nil {
		t.Fatal("zero creation time should be rejected")
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewAdminUser(tt.id, tt.username, tt.credential, now); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestAdminSessionLifecycleAndLoginAttemptValidation(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	accessHash := sha256.Sum256([]byte("access-secret"))
	refreshHash := sha256.Sum256([]byte("refresh-secret"))
	session, err := NewSession(
		" session-1 ",
		" family-1 ",
		" admin-1 ",
		accessHash,
		refreshHash,
		now.Add(5*time.Minute),
		now.Add(30*24*time.Hour),
		now,
	)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if session.ID != "session-1" || session.FamilyID != "family-1" || session.AdminUserID != "admin-1" {
		t.Fatalf("normalized session identity = %+v", session)
	}
	if !session.Active(now) || session.Active(now.Add(30*24*time.Hour)) {
		t.Fatalf("unexpected refresh activity at expiry boundary: %+v", session)
	}
	if !session.AccessActive(now) || session.AccessActive(now.Add(5*time.Minute)) {
		t.Fatalf("unexpected access activity at expiry boundary: %+v", session)
	}
	if !session.MatchesAccessTokenHash(accessHash) || !session.MatchesRefreshSecretHash(refreshHash) {
		t.Fatal("session hash comparison rejected matching hashes")
	}
	if session.MatchesRefreshSecretHash(sha256.Sum256([]byte("wrong"))) {
		t.Fatal("session hash comparison accepted a different secret hash")
	}

	if err := session.Revoke(now.Add(time.Minute), RevocationLogout); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if session.Active(now.Add(2*time.Minute)) || session.AccessActive(now.Add(2*time.Minute)) || session.RevokedAt.IsZero() {
		t.Fatalf("revoked session remained active: %+v", session)
	}
	if session.RevocationReason != RevocationLogout {
		t.Fatalf("revocation reason = %q; want logout", session.RevocationReason)
	}

	zeroRevoke, err := NewSession("zero-revoke", "family-2", "admin-1", accessHash, refreshHash, now.Add(5*time.Minute), now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("new zero-revoke fixture: %v", err)
	}
	if err := zeroRevoke.Revoke(time.Time{}, RevocationLogout); err == nil {
		t.Fatal("zero revocation time should be rejected")
	}
	if !zeroRevoke.RevokedAt.IsZero() || !zeroRevoke.Active(now) {
		t.Fatalf("failed revoke changed session: %+v", zeroRevoke)
	}

	failed, err := NewLoginAttempt(" Admin ", " 127.0.0.1 ", false, "bad_password", now)
	if err != nil || failed.Username != "admin" || failed.Succeeded || failed.FailureCode != "bad_password" {
		t.Fatalf("failed login attempt = %+v err=%v", failed, err)
	}
	if _, err := NewLoginAttempt("admin", "127.0.0.1", false, "", now); err == nil {
		t.Fatal("failed attempt without failure code should be rejected")
	}
	if _, err := NewLoginAttempt("admin", "127.0.0.1", true, "bad_password", now); err == nil {
		t.Fatal("successful attempt with failure code should be rejected")
	}
	if _, err := NewLoginAttempt("admin", "", false, "bad_password", now); err == nil {
		t.Fatal("empty source IP should be rejected")
	}
	if _, err := NewLoginAttempt("admin", "not-an-ip", false, "bad_password", now); err == nil {
		t.Fatal("invalid source IP should be rejected")
	}
	if _, err := NewLoginAttempt("admin", "127.0.0.1", false, "bad_password", time.Time{}); err == nil {
		t.Fatal("zero login attempt time should be rejected")
	}
}

func TestAdminSessionRotationRecordsLineage(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte("secret"))
	session, err := NewSession("session-1", "family-1", "admin-1", hash, hash, now.Add(5*time.Minute), now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	replacement, err := NewSession("session-2", session.FamilyID, session.AdminUserID, sha256.Sum256([]byte("access-2")), sha256.Sum256([]byte("refresh-2")), now.Add(6*time.Minute), now.Add(time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("new replacement: %v", err)
	}
	if err := session.Rotate(now.Add(time.Minute), replacement); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if session.RotatedAt.IsZero() || session.RevokedAt.IsZero() || session.ReplacedBySessionID != "session-2" {
		t.Fatalf("rotation lineage = %+v", session)
	}
	if session.RevocationReason != RevocationRotated || session.Active(now.Add(2*time.Minute)) {
		t.Fatalf("rotated session lifecycle = %+v", session)
	}
	rotatedAt := session.RotatedAt
	if err := session.Rotate(now.Add(2*time.Minute), replacement); err == nil {
		t.Fatal("second rotation should be rejected")
	}
	if !session.RotatedAt.Equal(rotatedAt) || session.ReplacedBySessionID != "session-2" {
		t.Fatalf("failed rotation mutated lineage: %+v", session)
	}

	expired, err := NewSession("expired", "family-2", "admin-1", hash, sha256.Sum256([]byte("refresh")), now.Add(time.Minute), now.Add(2*time.Minute), now)
	if err != nil {
		t.Fatalf("new expired fixture: %v", err)
	}
	expiredReplacement, err := NewSession("replacement", expired.FamilyID, expired.AdminUserID, sha256.Sum256([]byte("access-3")), sha256.Sum256([]byte("refresh-3")), now.Add(3*time.Minute), now.Add(time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("new expired replacement: %v", err)
	}
	if err := expired.Rotate(now.Add(2*time.Minute), expiredReplacement); err == nil {
		t.Fatal("expired refresh session should not rotate")
	}
	crossFamily := replacement
	crossFamily.ID = "cross-family"
	crossFamily.FamilyID = "different-family"
	fresh, _ := NewSession("fresh", "family-1", "admin-1", hash, sha256.Sum256([]byte("fresh-refresh")), now.Add(5*time.Minute), now.Add(time.Hour), now)
	if err := fresh.Rotate(now.Add(time.Minute), crossFamily); err == nil {
		t.Fatal("cross-family replacement should be rejected")
	}
	crossAdmin := replacement
	crossAdmin.ID = "cross-admin"
	crossAdmin.AdminUserID = "admin-2"
	if err := fresh.Rotate(now.Add(time.Minute), crossAdmin); err == nil {
		t.Fatal("cross-admin replacement should be rejected")
	}
}

func TestAdminAuthJSONNeverSerializesCredentialHashes(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte("secret"))
	user, err := NewAdminUser("admin-1", "admin", PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1, Hash: validBcryptFixture}, now)
	if err != nil {
		t.Fatalf("new user: %v", err)
	}
	session, err := NewSession("session-1", "family-1", user.ID, hash, hash, now.Add(time.Minute), now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	assertJSONFieldAbsent(t, user, "Password", "Hash")
	assertJSONFieldAbsent(t, session, "AccessTokenHash")
	assertJSONFieldAbsent(t, session, "RefreshSecretHash")
}

func assertJSONFieldAbsent(t *testing.T, value any, path ...string) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var current any
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for index, field := range path {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("path %v is not an object in %s", path[:index], data)
		}
		next, found := object[field]
		if !found {
			return
		}
		current = next
	}
	t.Fatalf("sensitive JSON field %v was serialized: %s", path, data)
}
