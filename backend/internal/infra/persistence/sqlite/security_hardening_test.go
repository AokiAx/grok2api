package sqlite_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
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

const validStoredBcrypt = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func TestSQLiteV5ForeignKeysCascadeSecurityChildrenAndPreserveStickyMarker(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "cascade.db")
	repo := openSecurityRepo(t, ctx, database)
	if enabled, err := repo.ForeignKeysEnabled(ctx); err != nil || !enabled {
		t.Fatalf("foreign_keys enabled=%v err=%v", enabled, err)
	}
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	user := createStoredAdmin(t, ctx, repo, "cascade-admin", "cascade", now)
	session := newStoredSession(t, "cascade-session", "cascade-family", user.ID, "cascade", now)
	if err := repo.CreateAdminSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	credential := newStoredCredential(t, "cascade-key", "Cascade", clientkey.ModelPolicyAllowlist, 2, now, []string{"grok-4.5"})
	if err := repo.CreateClientKey(ctx, credential); err != nil {
		t.Fatalf("create client key: %v", err)
	}
	if _, err := repo.ConsumeClientKeyRPM(ctx, credential.Key.ID, now); err != nil {
		t.Fatalf("create rate window: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close repo: %v", err)
	}

	raw := openRawSQLite(t, database)
	if _, err := raw.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("enable raw foreign keys: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM admin_users WHERE id=?`, user.ID); err != nil {
		t.Fatalf("delete admin user: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM client_keys WHERE id=?`, credential.Key.ID); err != nil {
		t.Fatalf("delete client key: %v", err)
	}
	for table, want := range map[string]int{
		"admin_sessions":          0,
		"client_keys":             0,
		"client_key_model_scopes": 0,
		"client_key_rate_windows": 0,
	} {
		var got int
		if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s count=%d want=%d err=%v", table, got, want, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	repo = openSecurityRepo(t, ctx, database)
	defer repo.Close()
	if required, err := repo.ClientAuthRequired(ctx); err != nil || !required {
		t.Fatalf("sticky marker required=%v err=%v", required, err)
	}
	if _, found, err := repo.GetClientKey(ctx, credential.Key.ID); err != nil || found {
		t.Fatalf("deleted key found=%v err=%v", found, err)
	}
}

func TestSQLiteAdminRotationRollsBackInsertFailureAndAllowsOneConcurrentWinner(t *testing.T) {
	ctx := context.Background()
	repo := openSecurityRepo(t, ctx, filepath.Join(t.TempDir(), "rotation.db"))
	defer repo.Close()
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	user := createStoredAdmin(t, ctx, repo, "rotation-admin", "rotation", now)
	if duplicate, err := adminauth.NewAdminUser("rotation-admin-2", "ROTATION", adminauth.PasswordCredential{
		Scheme: adminauth.PasswordSchemeBcryptSHA256V1, Hash: validStoredBcrypt,
	}, now); err != nil {
		t.Fatalf("new duplicate username: %v", err)
	} else if err := repo.CreateAdminUser(ctx, duplicate); err == nil {
		t.Fatal("case-insensitive duplicate username should fail")
	}

	blocker := newStoredSession(t, "replacement-collision", "blocker-family", user.ID, "blocker", now)
	if err := repo.CreateAdminSession(ctx, blocker); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	rollbackOld := newStoredSession(t, "rollback-old", "rollback-family", user.ID, "rollback-old", now)
	if err := repo.CreateAdminSession(ctx, rollbackOld); err != nil {
		t.Fatalf("create rollback old: %v", err)
	}
	rollbackReplacement := newStoredSession(t, blocker.ID, rollbackOld.FamilyID, user.ID, "rollback-replacement", now.Add(time.Minute))
	if rotated, err := repo.RotateAdminSession(ctx, rollbackOld.ID, rollbackOld.RefreshSecretHash, rollbackReplacement, now.Add(time.Minute)); err == nil || rotated {
		t.Fatalf("collision rotation rotated=%v err=%v", rotated, err)
	}
	storedOld, found, err := repo.GetAdminSession(ctx, rollbackOld.ID)
	if err != nil || !found || !storedOld.RevokedAt.IsZero() || storedOld.ReplacedBySessionID != "" {
		t.Fatalf("rollback old mutated after failed insert: %+v found=%v err=%v", storedOld, found, err)
	}

	old := newStoredSession(t, "concurrent-old", "concurrent-family", user.ID, "concurrent-old", now)
	if err := repo.CreateAdminSession(ctx, old); err != nil {
		t.Fatalf("create concurrent old: %v", err)
	}
	const contenders = 12
	start := make(chan struct{})
	var winners atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, contenders)
	replacements := make([]adminauth.Session, contenders)
	for i := range contenders {
		replacements[i] = newStoredSession(t, fmt.Sprintf("replacement-%02d", i), old.FamilyID, user.ID, fmt.Sprintf("replacement-%02d", i), now.Add(time.Minute))
		wg.Add(1)
		go func(replacement adminauth.Session) {
			defer wg.Done()
			<-start
			rotated, err := repo.RotateAdminSession(ctx, old.ID, old.RefreshSecretHash, replacement, now.Add(time.Minute))
			if err != nil {
				errs <- err
				return
			}
			if rotated {
				winners.Add(1)
			}
		}(replacements[i])
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent rotate: %v", err)
	}
	if winners.Load() != 1 {
		t.Fatalf("rotation winners=%d; want 1", winners.Load())
	}
	rotatedOld, found, err := repo.GetAdminSession(ctx, old.ID)
	if err != nil || !found || rotatedOld.ReplacedBySessionID == "" {
		t.Fatalf("rotated old=%+v found=%v err=%v", rotatedOld, found, err)
	}
	inserted := 0
	for _, replacement := range replacements {
		_, found, err := repo.GetAdminSession(ctx, replacement.ID)
		if err != nil {
			t.Fatalf("lookup replacement: %v", err)
		}
		if found {
			inserted++
		}
	}
	if inserted != 1 {
		t.Fatalf("inserted replacements=%d; want 1", inserted)
	}
}

func TestSQLitePersistenceRejectsInvalidSecurityFactsAndEmptyRevocationTargets(t *testing.T) {
	ctx := context.Background()
	repo := openSecurityRepo(t, ctx, filepath.Join(t.TempDir(), "validation.db"))
	defer repo.Close()
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	invalidAttempts := []adminauth.LoginAttempt{
		{Username: "", SourceIP: "127.0.0.1", FailureCode: "bad_password", CreatedAt: now},
		{Username: "admin", SourceIP: "not-an-ip", FailureCode: "bad_password", CreatedAt: now},
		{Username: "admin", SourceIP: "127.0.0.1", Succeeded: true, FailureCode: "bad_password", CreatedAt: now},
	}
	for _, attempt := range invalidAttempts {
		if err := repo.RecordAdminLoginAttempt(ctx, attempt); err == nil {
			t.Fatalf("invalid attempt was persisted: %+v", attempt)
		}
	}
	if err := repo.RevokeAdminSession(ctx, "", now, adminauth.RevocationLogout); err == nil {
		t.Fatal("empty session id should be rejected")
	}
	if err := repo.RevokeAdminSessionFamily(ctx, "", now, adminauth.RevocationRefreshReplay); err == nil {
		t.Fatal("empty family id should be rejected")
	}
	if err := repo.RevokeClientKey(ctx, "", now); err == nil {
		t.Fatal("empty client key id should be rejected")
	}
	if err := repo.RevokeAdminSession(ctx, "missing", now, adminauth.RevocationLogout); err != nil {
		t.Fatalf("missing session revoke should be idempotent: %v", err)
	}
	if err := repo.RevokeClientKey(ctx, "missing", now); err != nil {
		t.Fatalf("missing key revoke should be idempotent: %v", err)
	}
}

func TestSQLiteSecurityHashesStayBinaryAndPolicyUpdateCannotRewriteIdentity(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "hashes.db")
	repo := openSecurityRepo(t, ctx, database)
	defer repo.Close()
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	user := createStoredAdmin(t, ctx, repo, "hash-admin", "hash-admin", now)
	session := newStoredSession(t, "hash-session", "hash-family", user.ID, "hash-session", now)
	if err := repo.CreateAdminSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	credential := newStoredCredential(t, "hash-key", "Hash Key", clientkey.ModelPolicyAll, 1, now, nil)
	if err := repo.CreateClientKey(ctx, credential); err != nil {
		t.Fatalf("create client key: %v", err)
	}
	if err := repo.RevokeClientKey(ctx, credential.Key.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	originalHash := credential.Key.KeyHash
	if err := repo.UpdateClientKeyPolicy(ctx, credential.Key.ID, repository.ClientKeyPolicyUpdate{
		Name: "Renamed", ModelPolicy: clientkey.ModelPolicyAll, RPMLimit: 9, UpdatedAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("update policy: %v", err)
	}
	stored, found, err := repo.GetClientKey(ctx, credential.Key.ID)
	if err != nil || !found {
		t.Fatalf("get updated key found=%v err=%v", found, err)
	}
	if stored.Key.KeyHash != originalHash || stored.Key.Origin != clientkey.OriginManaged || stored.Key.RevokedAt.IsZero() {
		t.Fatalf("immutable key identity changed: %+v", stored.Key)
	}

	raw := openRawSQLite(t, database)
	var accessLength, refreshLength, keyLength int
	var passwordHash string
	var storedKeyHash []byte
	if err := raw.QueryRowContext(ctx, `SELECT password_hash FROM admin_users WHERE id=?`, user.ID).Scan(&passwordHash); err != nil {
		t.Fatalf("read password hash: %v", err)
	}
	if passwordHash == "hash-admin" || passwordHash != validStoredBcrypt {
		t.Fatalf("password hash storage=%q", passwordHash)
	}
	if err := raw.QueryRowContext(ctx, `SELECT length(access_token_hash), length(refresh_secret_hash) FROM admin_sessions WHERE id=?`, session.ID).Scan(&accessLength, &refreshLength); err != nil {
		t.Fatalf("read session hash lengths: %v", err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT length(key_hash), key_hash FROM client_keys WHERE id=?`, credential.Key.ID).Scan(&keyLength, &storedKeyHash); err != nil {
		t.Fatalf("read client key hash: %v", err)
	}
	if accessLength != 32 || refreshLength != 32 || keyLength != 32 || !bytes.Equal(storedKeyHash, originalHash[:]) {
		t.Fatalf("hash storage lengths access=%d refresh=%d key=%d", accessLength, refreshLength, keyLength)
	}
}

func TestSQLiteLoginFailureTuplePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "login-reopen.db")
	repo := openSecurityRepo(t, ctx, database)
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	for _, attempt := range []adminauth.LoginAttempt{
		mustLoginAttempt(t, "admin", "10.0.0.1", false, "bad_password", now),
		mustLoginAttempt(t, "admin", "10.0.0.2", false, "bad_password", now),
		mustLoginAttempt(t, "other", "10.0.0.1", false, "bad_password", now),
	} {
		if err := repo.RecordAdminLoginAttempt(ctx, attempt); err != nil {
			t.Fatalf("record attempt: %v", err)
		}
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close repo: %v", err)
	}
	repo = openSecurityRepo(t, ctx, database)
	defer repo.Close()
	count, err := repo.CountRecentAdminLoginFailures(ctx, "ADMIN", "10.0.0.1", now.Add(-15*time.Minute))
	if err != nil || count != 1 {
		t.Fatalf("reopened tuple count=%d err=%v", count, err)
	}
}

func TestSQLiteRPMReadsPersistedPolicyAndInactiveFailuresDoNotCreateWindows(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "rpm-policy.db")
	repo := openSecurityRepo(t, ctx, database)
	defer repo.Close()
	now := time.Date(2026, 7, 15, 1, 0, 30, 0, time.UTC)
	limited := newStoredCredential(t, "limited", "Limited", clientkey.ModelPolicyAll, 2, now, nil)
	if err := repo.CreateClientKey(ctx, limited); err != nil {
		t.Fatalf("create limited key: %v", err)
	}
	for i := 0; i < 2; i++ {
		if decision, err := repo.ConsumeClientKeyRPM(ctx, limited.Key.ID, now); err != nil || !decision.Allowed {
			t.Fatalf("initial consume %d decision=%+v err=%v", i, decision, err)
		}
	}
	if decision, err := repo.ConsumeClientKeyRPM(ctx, limited.Key.ID, now); err != nil || decision.Allowed {
		t.Fatalf("limit decision=%+v err=%v", decision, err)
	}
	if err := repo.UpdateClientKeyPolicy(ctx, limited.Key.ID, repository.ClientKeyPolicyUpdate{
		Name: limited.Key.Name, ModelPolicy: clientkey.ModelPolicyAll, RPMLimit: 4, UpdatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("raise persisted limit: %v", err)
	}
	if decision, err := repo.ConsumeClientKeyRPM(ctx, limited.Key.ID, now); err != nil || !decision.Allowed || decision.Limit != 4 || decision.Remaining != 1 {
		t.Fatalf("raised limit decision=%+v err=%v", decision, err)
	}

	unlimited := newStoredCredential(t, "unlimited", "Unlimited", clientkey.ModelPolicyAll, 0, now, nil)
	if err := repo.CreateClientKey(ctx, unlimited); err != nil {
		t.Fatalf("create unlimited key: %v", err)
	}
	if decision, err := repo.ConsumeClientKeyRPM(ctx, unlimited.Key.ID, now); err != nil || !decision.Allowed || decision.Limit != 0 {
		t.Fatalf("unlimited decision=%+v err=%v", decision, err)
	}

	expired := newStoredCredential(t, "expired", "Expired", clientkey.ModelPolicyAll, 5, now, nil)
	expired.Key.ExpiresAt = now
	expired, err := clientkey.NewCredential(expired.Key, nil)
	if err != nil {
		t.Fatalf("rebuild expired credential: %v", err)
	}
	if err := repo.CreateClientKey(ctx, expired); err != nil {
		t.Fatalf("create expired key: %v", err)
	}
	revoked := newStoredCredential(t, "revoked", "Revoked", clientkey.ModelPolicyAll, 5, now, nil)
	if err := repo.CreateClientKey(ctx, revoked); err != nil {
		t.Fatalf("create revoked key: %v", err)
	}
	if err := repo.RevokeClientKey(ctx, revoked.Key.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("revoke rpm key: %v", err)
	}
	for _, id := range []string{expired.Key.ID, revoked.Key.ID, "missing"} {
		if _, err := repo.ConsumeClientKeyRPM(ctx, id, now.Add(2*time.Second)); err == nil {
			t.Fatalf("inactive/missing key %s should fail", id)
		}
	}
	raw := openRawSQLite(t, database)
	for _, id := range []string{unlimited.Key.ID, expired.Key.ID, revoked.Key.ID} {
		var count int
		if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_key_rate_windows WHERE client_key_id=?`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unexpected rate window for %s count=%d err=%v", id, count, err)
		}
	}
}

func openSecurityRepo(t *testing.T, ctx context.Context, database string) *sqlite.SQLite {
	t.Helper()
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return repo
}

func createStoredAdmin(t *testing.T, ctx context.Context, repo *sqlite.SQLite, id, username string, now time.Time) adminauth.AdminUser {
	t.Helper()
	item, err := adminauth.NewAdminUser(id, username, adminauth.PasswordCredential{
		Scheme: adminauth.PasswordSchemeBcryptSHA256V1, Hash: validStoredBcrypt,
	}, now)
	if err != nil {
		t.Fatalf("new admin: %v", err)
	}
	if err := repo.CreateAdminUser(ctx, item); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	return item
}

func newStoredSession(t *testing.T, id, familyID, adminID, secret string, now time.Time) adminauth.Session {
	t.Helper()
	access := sha256.Sum256([]byte(secret + "-access"))
	refresh := sha256.Sum256([]byte(secret + "-refresh"))
	item, err := adminauth.NewSession(id, familyID, adminID, access, refresh, now.Add(5*time.Minute), now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	return item
}

func newStoredCredential(
	t *testing.T,
	id, name string,
	policy clientkey.ModelPolicy,
	rpm int,
	now time.Time,
	scopes []string,
) clientkey.Credential {
	t.Helper()
	hash := sha256.Sum256([]byte(id + "-secret"))
	item, err := clientkey.NewCredential(clientkey.ClientKey{
		ID: id, Name: name, Origin: clientkey.OriginManaged, KeyHash: hash,
		KeyPrefix: "g2a_" + id, ModelPolicy: policy, RPMLimit: rpm,
		CreatedAt: now, UpdatedAt: now,
	}, scopes)
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	return item
}
