package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/bootstrap"
	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/security"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func TestLegacySecurityBootstrapPrefersPanelPasswordAndMigratesAPIKeyHashOnly(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "bootstrap.db")
	repo := openBootstrapRepo(t, ctx, database)
	defer repo.Close()
	now := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	service := bootstrap.NewLegacySecurityService(repo, func() time.Time { return now }, bcrypt.MinCost)

	result, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{
		PanelPassword: " panel-secret ", AppKey: "app-secret", APIKey: " legacy-client-secret ",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if result.Admin != bootstrap.BootstrapCreated || result.ClientKey != bootstrap.BootstrapCreated || result.AdminSetupRequired {
		t.Fatalf("result = %+v", result)
	}
	user, found, err := repo.GetAdminUserByUsername(ctx, "admin")
	if err != nil || !found {
		t.Fatalf("admin found=%v err=%v", found, err)
	}
	if !security.VerifyAdminPassword(user.Password, "panel-secret") || security.VerifyAdminPassword(user.Password, "app-secret") {
		t.Fatal("panel_password did not take precedence over app_key")
	}
	clientHash := sha256.Sum256([]byte("legacy-client-secret"))
	credential, found, err := repo.FindClientKeyByHash(ctx, clientHash)
	if err != nil || !found {
		t.Fatalf("client key found=%v err=%v", found, err)
	}
	if credential.Key.Name != "legacy" || credential.Key.Origin != clientkey.OriginConfigAPIKey || credential.Key.ModelPolicy != clientkey.ModelPolicyAll {
		t.Fatalf("legacy key = %+v", credential.Key)
	}
	if credential.Key.RPMLimit != 0 || credential.Key.MaxConcurrent != 0 || len(credential.Scopes()) != 0 {
		t.Fatalf("legacy permissions/limits = %+v scopes=%#v", credential.Key, credential.Scopes())
	}

	raw := openBootstrapRaw(t, database)
	assertMarker(t, raw, bootstrap.AdminBootstrapMarker, "1")
	assertMarker(t, raw, bootstrap.ClientKeyBootstrapMarker, "1")
	assertMarker(t, raw, "client_auth_required", "1")
	var storedPasswordHash string
	if err := raw.QueryRow(`SELECT password_hash FROM admin_users WHERE id=?`, user.ID).Scan(&storedPasswordHash); err != nil {
		t.Fatalf("read stored password hash: %v", err)
	}
	if storedPasswordHash == "panel-secret" || storedPasswordHash == "app-secret" || storedPasswordHash != user.Password.Hash {
		t.Fatalf("stored password hash = %q", storedPasswordHash)
	}
	var rawSecretCount int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM client_keys WHERE CAST(key_hash AS TEXT) IN (?, ?)`, "legacy-client-secret", " panel-secret ").Scan(&rawSecretCount); err != nil {
		t.Fatalf("scan raw secrets: %v", err)
	}
	if rawSecretCount != 0 {
		t.Fatal("legacy plaintext was stored in client_keys")
	}

	second, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{PanelPassword: "different-admin", APIKey: "legacy-client-secret"})
	if err != nil {
		t.Fatalf("idempotent bootstrap: %v", err)
	}
	if second.Admin != bootstrap.BootstrapAlreadyCompleted || second.ClientKey != bootstrap.BootstrapAlreadyCompleted {
		t.Fatalf("second result = %+v", second)
	}
	unchanged, _, _ := repo.GetAdminUserByUsername(ctx, "admin")
	if unchanged.Password.Hash != user.Password.Hash {
		t.Fatal("completed admin bootstrap overwrote the existing password")
	}
	withoutConfigKey, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{})
	if err != nil || withoutConfigKey.AdminSetupRequired {
		t.Fatalf("bootstrap after api_key removal result=%+v err=%v", withoutConfigKey, err)
	}
	if persisted, found, err := repo.FindClientKeyByHash(ctx, clientHash); err != nil || !found || persisted.Key.ID != credential.Key.ID {
		t.Fatalf("legacy key disappeared after config removal: %+v found=%v err=%v", persisted.Key, found, err)
	}
	if required, err := repo.ClientAuthRequired(ctx); err != nil || !required {
		t.Fatalf("client auth marker lost after config removal required=%v err=%v", required, err)
	}
	if _, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{APIKey: "changed-client-secret"}); err == nil {
		t.Fatal("changed config api_key should fail instead of rotating legacy key")
	}
}

func TestLegacySecurityBootstrapOnlyAPIKeyLeavesAdminSetupRequiredAndAppKeyFallsBack(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)

	t.Run("only api key", func(t *testing.T) {
		database := filepath.Join(t.TempDir(), "api-only.db")
		repo := openBootstrapRepo(t, ctx, database)
		defer repo.Close()
		service := bootstrap.NewLegacySecurityService(repo, func() time.Time { return now }, bcrypt.MinCost)
		result, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{APIKey: "client-only"})
		if err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if result.Admin != bootstrap.BootstrapSkipped || !result.AdminSetupRequired || result.ClientKey != bootstrap.BootstrapCreated {
			t.Fatalf("result = %+v", result)
		}
		if count, err := repo.CountAdminUsers(ctx); err != nil || count != 0 {
			t.Fatalf("admin count=%d err=%v", count, err)
		}
		raw := openBootstrapRaw(t, database)
		assertMarkerMissing(t, raw, bootstrap.AdminBootstrapMarker)
		assertMarker(t, raw, bootstrap.ClientKeyBootstrapMarker, "1")
	})

	t.Run("app key fallback", func(t *testing.T) {
		repo := openBootstrapRepo(t, ctx, filepath.Join(t.TempDir(), "app-key.db"))
		defer repo.Close()
		service := bootstrap.NewLegacySecurityService(repo, func() time.Time { return now }, bcrypt.MinCost)
		result, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{AppKey: "app-admin"})
		if err != nil || result.Admin != bootstrap.BootstrapCreated || result.AdminSetupRequired {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		user, found, err := repo.GetAdminUserByUsername(ctx, "admin")
		if err != nil || !found || !security.VerifyAdminPassword(user.Password, "app-admin") {
			t.Fatalf("app fallback user=%+v found=%v err=%v", user, found, err)
		}
	})
}

func TestLegacySecurityBootstrapDoesNotOverwriteAdminOrReviveLegacyClientKey(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "existing.db")
	repo := openBootstrapRepo(t, ctx, database)
	defer repo.Close()
	now := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	originalPassword, err := security.HashAdminPassword("original", bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash original: %v", err)
	}
	original, err := adminauth.NewAdminUser("existing-admin", "owner", originalPassword, now)
	if err != nil || repo.CreateAdminUser(ctx, original) != nil {
		t.Fatalf("create original admin err=%v", err)
	}
	service := bootstrap.NewLegacySecurityService(repo, func() time.Time { return now }, bcrypt.MinCost)
	result, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{PanelPassword: "replacement", APIKey: "legacy-key"})
	if err != nil || result.Admin != bootstrap.BootstrapExisting || result.ClientKey != bootstrap.BootstrapCreated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored, _, _ := repo.GetAdminUserByID(ctx, original.ID)
	if stored.Password.Hash != original.Password.Hash || stored.Username != original.Username {
		t.Fatal("existing administrator was overwritten")
	}
	hash := sha256.Sum256([]byte("legacy-key"))
	legacy, found, err := repo.FindClientKeyByHash(ctx, hash)
	if err != nil || !found {
		t.Fatalf("legacy found=%v err=%v", found, err)
	}
	if err := repo.RevokeClientKey(ctx, legacy.Key.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("revoke legacy: %v", err)
	}
	if _, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{PanelPassword: "another", APIKey: "legacy-key"}); err != nil {
		t.Fatalf("rebootstrap revoked legacy: %v", err)
	}
	revoked, found, err := repo.FindClientKeyByHash(ctx, hash)
	if err != nil || !found || revoked.Key.RevokedAt.IsZero() {
		t.Fatalf("revoked legacy was revived: %+v found=%v err=%v", revoked.Key, found, err)
	}
}

func TestLegacySecurityBootstrapMarkersAreIndependentAndTransactionFailureDoesNotMarkComplete(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "failure.db")
	repo := openBootstrapRepo(t, ctx, database)
	if err := repo.Close(); err != nil {
		t.Fatalf("close initialized repo: %v", err)
	}
	raw := openBootstrapRaw(t, database)
	if _, err := raw.Exec(`CREATE TRIGGER reject_legacy_client BEFORE INSERT ON client_keys
		WHEN NEW.origin='config_api_key' BEGIN SELECT RAISE(ABORT, 'fixture rejection'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	repo = openBootstrapRepo(t, ctx, database)
	defer repo.Close()
	now := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	service := bootstrap.NewLegacySecurityService(repo, func() time.Time { return now }, bcrypt.MinCost)
	if _, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{PanelPassword: "admin", APIKey: "client"}); err == nil {
		t.Fatal("expected client bootstrap transaction failure")
	}
	raw = openBootstrapRaw(t, database)
	assertMarker(t, raw, bootstrap.AdminBootstrapMarker, "1")
	assertMarkerMissing(t, raw, bootstrap.ClientKeyBootstrapMarker)
	assertMarker(t, raw, "client_auth_required", "0")
	var clients int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM client_keys`).Scan(&clients); err != nil || clients != 0 {
		t.Fatalf("client count=%d err=%v", clients, err)
	}
}

func TestLegacyAdminBootstrapFailureDoesNotPersistAdminOrMarker(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "admin-failure.db")
	repo := openBootstrapRepo(t, ctx, database)
	if err := repo.Close(); err != nil {
		t.Fatalf("close initialized repo: %v", err)
	}
	raw := openBootstrapRaw(t, database)
	if _, err := raw.Exec(`CREATE TRIGGER reject_legacy_admin BEFORE INSERT ON admin_users
		BEGIN SELECT RAISE(ABORT, 'fixture rejection'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	repo = openBootstrapRepo(t, ctx, database)
	defer repo.Close()
	service := bootstrap.NewLegacySecurityService(repo, func() time.Time {
		return time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	}, bcrypt.MinCost)
	if _, err := service.Bootstrap(ctx, bootstrap.LegacySecrets{PanelPassword: "admin"}); err == nil {
		t.Fatal("expected admin bootstrap transaction failure")
	}
	raw = openBootstrapRaw(t, database)
	assertMarkerMissing(t, raw, bootstrap.AdminBootstrapMarker)
	var admins int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&admins); err != nil || admins != 0 {
		t.Fatalf("admin count=%d err=%v", admins, err)
	}
}

func openBootstrapRepo(t *testing.T, ctx context.Context, database string) *sqlite.SQLite {
	t.Helper()
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return repo
}

func openBootstrapRaw(t *testing.T, database string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertMarker(t *testing.T, db *sql.DB, key, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`SELECT value FROM app_meta WHERE key=?`, key).Scan(&got); err != nil || got != want {
		t.Fatalf("marker %s=%q want=%q err=%v", key, got, want, err)
	}
}

func assertMarkerMissing(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	var value string
	if err := db.QueryRow(`SELECT value FROM app_meta WHERE key=?`, key).Scan(&value); err == nil {
		t.Fatalf("marker %s unexpectedly exists with value %q", key, value)
	}
}
