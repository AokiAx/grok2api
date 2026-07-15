package sqlite_test

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

func TestV4SchemaUpgradesToV5WithoutLosingAccountsEventsOrMetadata(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "upgrade-v4.db")
	db := openRawSQLite(t, database)
	statements := []string{
		`CREATE TABLE app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO app_meta(key, value) VALUES('schema_version', '4'), ('custom_meta', 'preserved')`,
		`CREATE TABLE accounts (
			id TEXT PRIMARY KEY, access_token TEXT NOT NULL, refresh_token TEXT NOT NULL DEFAULT '',
			expires_at TEXT NOT NULL DEFAULT '', oidc_issuer TEXT NOT NULL DEFAULT 'https://auth.x.ai',
			oidc_client_id TEXT NOT NULL DEFAULT '', email TEXT NOT NULL DEFAULT '', user_id TEXT NOT NULL DEFAULT '',
			team_id TEXT NOT NULL DEFAULT '', pool TEXT NOT NULL, unavailable_reason TEXT NOT NULL DEFAULT '',
			retry_at TEXT NOT NULL DEFAULT '', last_error_code TEXT NOT NULL DEFAULT '', last_success_at TEXT NOT NULL DEFAULT '',
			quota_actual INTEGER NOT NULL DEFAULT 0, quota_limit INTEGER NOT NULL DEFAULT 0, request_count INTEGER NOT NULL DEFAULT 0,
			authentication_fails INTEGER NOT NULL DEFAULT 0, max_active INTEGER NOT NULL DEFAULT 1,
			priority INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE account_state_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, account_id TEXT NOT NULL, from_pool TEXT NOT NULL,
			to_pool TEXT NOT NULL, event_type TEXT NOT NULL DEFAULT 'state_transition', reason TEXT NOT NULL,
			error_code TEXT NOT NULL DEFAULT '', details_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL
		)`,
		`INSERT INTO accounts(id, access_token, pool, max_active, priority, created_at, updated_at)
		 VALUES('legacy-v4', 'token', 'ready', 2, 9, '2026-07-15T00:00:00Z', '2026-07-15T00:00:00Z')`,
		`INSERT INTO account_state_events(account_id, from_pool, to_pool, event_type, reason, error_code, details_json, created_at)
		 VALUES('legacy-v4', '', 'ready', 'state_transition', '', '', '{}', '2026-07-15T00:00:00Z')`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare v4 schema: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v4 fixture: %v", err)
	}

	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("upgrade v4: %v", err)
	}
	defer repo.Close()
	if got := repo.SchemaVersion(ctx); got != 9 {
		t.Fatalf("schema version = %d; want 9", got)
	}
	item, found, err := repo.GetAccount(ctx, "legacy-v4")
	if err != nil || !found || item.Priority != 9 || item.MaxActive != 2 {
		t.Fatalf("preserved account = %+v found=%v err=%v", item, found, err)
	}
	events, err := repo.ListAccountEvents(ctx, repository.ListAccountEventsQuery{AccountID: "legacy-v4", Page: 1, PageSize: 20})
	if err != nil || events.Total != 1 {
		t.Fatalf("preserved events = %+v err=%v", events, err)
	}

	raw := openRawSQLite(t, database)
	var custom, authRequired string
	if err := raw.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key='custom_meta'`).Scan(&custom); err != nil || custom != "preserved" {
		t.Fatalf("custom metadata = %q err=%v", custom, err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key='client_auth_required'`).Scan(&authRequired); err != nil || authRequired != "1" {
		t.Fatalf("client_auth_required = %q err=%v", authRequired, err)
	}
	for _, table := range []string{
		"admin_users", "admin_sessions", "admin_login_attempts", "client_keys",
		"client_key_model_scopes", "client_key_rate_windows",
	} {
		var count int
		if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s count=%d err=%v", table, count, err)
		}
	}
}

func TestSQLiteAdminAuthPersistenceEnforcesForeignKeysRotationAndThrottleDimensions(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "admin-auth.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer repo.Close()
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	user, err := adminauth.NewAdminUser("admin-1", "Admin", adminauth.PasswordCredential{
		Scheme: adminauth.PasswordSchemeBcryptSHA256V1, Hash: "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
	}, now)
	if err != nil {
		t.Fatalf("new user: %v", err)
	}
	if err := repo.CreateAdminUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, found, err := repo.GetAdminUserByUsername(ctx, " ADMIN "); err != nil || !found {
		t.Fatalf("lookup normalized username found=%v err=%v", found, err)
	}
	secondUser, err := adminauth.NewAdminUser("admin-2", "second", adminauth.PasswordCredential{
		Scheme: adminauth.PasswordSchemeBcryptSHA256V1, Hash: "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
	}, now)
	if err != nil {
		t.Fatalf("new second user: %v", err)
	}
	if err := repo.CreateAdminUser(ctx, secondUser); err != nil {
		t.Fatalf("create second user: %v", err)
	}

	accessHash := sha256.Sum256([]byte("access-1"))
	refreshHash := sha256.Sum256([]byte("refresh-1"))
	session, err := adminauth.NewSession("session-1", "family-1", user.ID, accessHash, refreshHash, now.Add(5*time.Minute), now.Add(30*24*time.Hour), now)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := repo.CreateAdminSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if got, found, err := repo.FindAdminSessionByAccessHash(ctx, accessHash); err != nil || !found || got.ID != session.ID {
		t.Fatalf("access lookup = %+v found=%v err=%v", got, found, err)
	}
	invalid := session
	invalid.ID = "invalid-user-session"
	invalid.AdminUserID = "missing"
	invalid.AccessTokenHash = sha256.Sum256([]byte("invalid-access"))
	invalid.RefreshSecretHash = sha256.Sum256([]byte("invalid-refresh"))
	if err := repo.CreateAdminSession(ctx, invalid); err == nil {
		t.Fatal("foreign key violation should reject session for missing admin")
	}
	expiredAccess := sha256.Sum256([]byte("expired-access"))
	expiredRefresh := sha256.Sum256([]byte("expired-refresh"))
	expired, err := adminauth.NewSession("expired-session", "expired-family", user.ID, expiredAccess, expiredRefresh, now.Add(time.Minute), now.Add(2*time.Minute), now)
	if err != nil {
		t.Fatalf("new expired session: %v", err)
	}
	if err := repo.CreateAdminSession(ctx, expired); err != nil {
		t.Fatalf("create expired session fixture: %v", err)
	}
	expiredReplacement, err := adminauth.NewSession(
		"expired-replacement", expired.FamilyID, user.ID,
		sha256.Sum256([]byte("expired-replacement-access")), sha256.Sum256([]byte("expired-replacement-refresh")),
		now.Add(7*time.Minute), now.Add(time.Hour), now.Add(2*time.Minute),
	)
	if err != nil {
		t.Fatalf("new expired replacement: %v", err)
	}
	if rotated, err := repo.RotateAdminSession(ctx, expired.ID, expiredRefresh, expiredReplacement, now.Add(2*time.Minute)); err != nil || rotated {
		t.Fatalf("expired rotation rotated=%v err=%v", rotated, err)
	}

	lineageAccess := sha256.Sum256([]byte("lineage-access"))
	lineageRefresh := sha256.Sum256([]byte("lineage-refresh"))
	lineage, err := adminauth.NewSession("lineage-session", "lineage-family", user.ID, lineageAccess, lineageRefresh, now.Add(5*time.Minute), now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("new lineage session: %v", err)
	}
	if err := repo.CreateAdminSession(ctx, lineage); err != nil {
		t.Fatalf("create lineage session: %v", err)
	}
	crossFamily, err := adminauth.NewSession(
		"cross-family", "different-family", user.ID,
		sha256.Sum256([]byte("cross-access")), sha256.Sum256([]byte("cross-refresh")),
		now.Add(6*time.Minute), now.Add(time.Hour), now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("new cross-family replacement: %v", err)
	}
	if rotated, err := repo.RotateAdminSession(ctx, lineage.ID, lineageRefresh, crossFamily, now.Add(time.Minute)); err != nil || rotated {
		t.Fatalf("cross-family rotation rotated=%v err=%v", rotated, err)
	}
	crossAdmin, err := adminauth.NewSession(
		"cross-admin", lineage.FamilyID, secondUser.ID,
		sha256.Sum256([]byte("cross-admin-access")), sha256.Sum256([]byte("cross-admin-refresh")),
		now.Add(6*time.Minute), now.Add(time.Hour), now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("new cross-admin replacement: %v", err)
	}
	if rotated, err := repo.RotateAdminSession(ctx, lineage.ID, lineageRefresh, crossAdmin, now.Add(time.Minute)); err != nil || rotated {
		t.Fatalf("cross-admin rotation rotated=%v err=%v", rotated, err)
	}

	replacementAccess := sha256.Sum256([]byte("access-2"))
	replacementRefresh := sha256.Sum256([]byte("refresh-2"))
	replacement, err := adminauth.NewSession("session-2", session.FamilyID, user.ID, replacementAccess, replacementRefresh, now.Add(6*time.Minute), now.Add(30*24*time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("new replacement: %v", err)
	}
	rotated, err := repo.RotateAdminSession(ctx, session.ID, refreshHash, replacement, now.Add(time.Minute))
	if err != nil || !rotated {
		t.Fatalf("rotate session rotated=%v err=%v", rotated, err)
	}
	rotated, err = repo.RotateAdminSession(ctx, session.ID, refreshHash, replacement, now.Add(2*time.Minute))
	if err != nil || rotated {
		t.Fatalf("rotation CAS loser rotated=%v err=%v", rotated, err)
	}
	if err := repo.RevokeAdminSession(ctx, replacement.ID, time.Time{}, adminauth.RevocationLogout); err == nil {
		t.Fatal("zero session revocation time should be rejected")
	}
	old, found, err := repo.GetAdminSession(ctx, session.ID)
	if err != nil || !found || old.ReplacedBySessionID != replacement.ID || old.RevocationReason != adminauth.RevocationRotated {
		t.Fatalf("old session lineage = %+v found=%v err=%v", old, found, err)
	}
	if err := repo.RevokeAdminSessionFamily(ctx, session.FamilyID, now.Add(3*time.Minute), adminauth.RevocationRefreshReplay); err != nil {
		t.Fatalf("revoke family: %v", err)
	}
	current, found, err := repo.GetAdminSession(ctx, replacement.ID)
	if err != nil || !found || current.RevocationReason != adminauth.RevocationRefreshReplay {
		t.Fatalf("replacement after family revoke = %+v found=%v err=%v", current, found, err)
	}

	attempts := []adminauth.LoginAttempt{
		mustLoginAttempt(t, "admin", "10.0.0.1", false, "bad_password", now),
		mustLoginAttempt(t, "admin", "10.0.0.2", false, "bad_password", now.Add(time.Minute)),
		mustLoginAttempt(t, "other", "10.0.0.1", false, "bad_password", now.Add(2*time.Minute)),
		mustLoginAttempt(t, "admin", "10.0.0.1", true, "", now.Add(3*time.Minute)),
	}
	for _, attempt := range attempts {
		if err := repo.RecordAdminLoginAttempt(ctx, attempt); err != nil {
			t.Fatalf("record attempt: %v", err)
		}
	}
	count, err := repo.CountRecentAdminLoginFailures(ctx, "ADMIN", "10.0.0.1", now.Add(-15*time.Minute))
	if err != nil || count != 0 {
		t.Fatalf("tuple failure count = %d err=%v", count, err)
	}
	if err := repo.RecordAdminLoginAttempt(ctx, mustLoginAttempt(t, "admin", "10.0.0.1", false, "bad_password", now.Add(3*time.Minute))); err != nil {
		t.Fatalf("record post-success failure: %v", err)
	}
	count, err = repo.CountRecentAdminLoginFailures(ctx, "admin", "10.0.0.1", now.Add(-15*time.Minute))
	if err != nil || count != 1 {
		t.Fatalf("post-success tuple failure count = %d err=%v", count, err)
	}
}

func TestSQLiteClientKeyPersistenceScopesAndStickyAuthMarker(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "client-keys.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	required, err := repo.ClientAuthRequired(ctx)
	if err != nil || !required {
		t.Fatalf("initial auth required=%v err=%v", required, err)
	}
	hash := sha256.Sum256([]byte("client-secret"))
	key := clientkey.ClientKey{
		ID: "key-1", Name: "Primary", Origin: clientkey.OriginManaged, KeyHash: hash,
		KeyPrefix: "g2a_abcd", ModelPolicy: clientkey.ModelPolicyAll, CreatedAt: now, UpdatedAt: now,
	}
	credential, err := clientkey.NewCredential(key, nil)
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	if err := repo.CreateClientKey(ctx, credential); err != nil {
		t.Fatalf("create key: %v", err)
	}
	required, err = repo.ClientAuthRequired(ctx)
	if err != nil || !required {
		t.Fatalf("sticky auth required=%v err=%v", required, err)
	}
	stored, found, err := repo.FindClientKeyByHash(ctx, hash)
	if err != nil || !found || stored.Key.ID != key.ID || len(stored.Scopes()) != 0 {
		t.Fatalf("find key = %+v found=%v err=%v", stored, found, err)
	}
	update := repository.ClientKeyPolicyUpdate{
		Name: "Primary", ModelPolicy: clientkey.ModelPolicyAllowlist,
		Scopes: []string{"grok-4.5", "GROK-CODE-FAST-1"}, UpdatedAt: now.Add(time.Minute),
	}
	if err := repo.UpdateClientKeyPolicy(ctx, key.ID, update); err != nil {
		t.Fatalf("update scopes: %v", err)
	}
	listed, err := repo.ListClientKeysPage(ctx, repository.ListClientKeysQuery{Q: "primary", Page: 1, PageSize: 20})
	if err != nil || listed.Total != 1 || len(listed.Items[0].Scopes()) != 2 {
		t.Fatalf("listed keys = %+v err=%v", listed, err)
	}
	if err := repo.RevokeClientKey(ctx, key.ID, time.Time{}); err == nil {
		t.Fatal("zero client key revocation time should be rejected")
	}
	if err := repo.RevokeClientKey(ctx, key.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	update.Name = "Renamed after revoke"
	update.UpdatedAt = now.Add(3 * time.Minute)
	if err := repo.UpdateClientKeyPolicy(ctx, key.ID, update); err != nil {
		t.Fatalf("policy update after revoke: %v", err)
	}
	revoked, found, err := repo.GetClientKey(ctx, key.ID)
	if err != nil || !found || revoked.Key.RevokedAt.IsZero() {
		t.Fatalf("revoked key was resurrected: %+v found=%v err=%v", revoked, found, err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close repo: %v", err)
	}
	raw := openRawSQLite(t, database)
	if _, err := raw.ExecContext(ctx, `PRAGMA foreign_keys=ON; DELETE FROM client_keys`); err != nil {
		t.Fatalf("delete all client keys: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	repo, err = sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	defer repo.Close()
	required, err = repo.ClientAuthRequired(ctx)
	if err != nil || !required {
		t.Fatalf("reopened sticky auth required=%v err=%v", required, err)
	}
}

func TestSQLiteClientKeyRPMIsAtomicAndPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "rpm.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	now := time.Date(2026, 7, 15, 1, 0, 30, 0, time.UTC)
	hash := sha256.Sum256([]byte("rpm-secret"))
	key := clientkey.ClientKey{
		ID: "rpm-key", Name: "RPM", Origin: clientkey.OriginManaged, KeyHash: hash,
		KeyPrefix: "g2a_rpm", ModelPolicy: clientkey.ModelPolicyAll, RPMLimit: 5, CreatedAt: now, UpdatedAt: now,
	}
	credential, err := clientkey.NewCredential(key, nil)
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	if err := repo.CreateClientKey(ctx, credential); err != nil {
		t.Fatalf("create key: %v", err)
	}

	var allowed atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := repo.ConsumeClientKeyRPM(ctx, key.ID, now)
			if err != nil {
				errs <- err
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("consume rpm: %v", err)
	}
	if got := allowed.Load(); got != 5 {
		t.Fatalf("allowed requests = %d; want 5", got)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close repo: %v", err)
	}

	repo, err = sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	defer repo.Close()
	decision, err := repo.ConsumeClientKeyRPM(ctx, key.ID, now)
	if err != nil || decision.Allowed || decision.Remaining != 0 {
		t.Fatalf("persisted window decision = %+v err=%v", decision, err)
	}
	nextMinute := now.Truncate(time.Minute).Add(time.Minute)
	decision, err = repo.ConsumeClientKeyRPM(ctx, key.ID, nextMinute)
	if err != nil || !decision.Allowed || decision.Remaining != 4 || !decision.ResetAt.Equal(nextMinute.Add(time.Minute)) {
		t.Fatalf("reset window decision = %+v err=%v", decision, err)
	}
}

func mustLoginAttempt(t *testing.T, username, sourceIP string, succeeded bool, code string, at time.Time) adminauth.LoginAttempt {
	t.Helper()
	attempt, err := adminauth.NewLoginAttempt(username, sourceIP, succeeded, code, at)
	if err != nil {
		t.Fatalf("new login attempt: %v", err)
	}
	return attempt
}
