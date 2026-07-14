package adminauth

import (
	"testing"
	"time"
)

func TestNewAdminUserNormalizesAndValidatesCredential(t *testing.T) {
	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.FixedZone("HKT", 8*60*60))
	user, err := NewAdminUser(
		" admin-1 ",
		" Admin ",
		PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1, Hash: "$2a$12$fixture"},
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
		{name: "blank id", username: "admin", credential: PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1, Hash: "hash"}},
		{name: "blank username", id: "admin-1", credential: PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1, Hash: "hash"}},
		{name: "unsupported scheme", id: "admin-1", username: "admin", credential: PasswordCredential{Scheme: "plain", Hash: "secret"}},
		{name: "blank hash", id: "admin-1", username: "admin", credential: PasswordCredential{Scheme: PasswordSchemeBcryptSHA256V1}},
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
	session, err := NewSession(" session-1 ", " admin-1 ", " refresh-hash ", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if !session.Active(now) || session.Active(now.Add(time.Hour)) {
		t.Fatalf("unexpected session activity at expiry boundary: %+v", session)
	}
	session.Revoke(now.Add(time.Minute))
	if session.Active(now.Add(2 * time.Minute)) || session.RevokedAt.IsZero() {
		t.Fatalf("revoked session remained active: %+v", session)
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
}
