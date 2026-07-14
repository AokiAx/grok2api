package sqlite_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	_ "modernc.org/sqlite"
)

func TestPythonV1FixtureMigratesAllSupportedFieldsAndStates(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "python-v1.db")
	applySQLFixture(t, database, filepath.Join("testdata", "python_v1.sql"))

	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if got := repo.SchemaVersion(ctx); got != 4 {
		t.Fatalf("schema version = %d; want 4", got)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list migrated accounts: %v", err)
	}
	if len(accounts) != 4 {
		t.Fatalf("account count = %d; want 4", len(accounts))
	}
	byID := accountsByID(accounts)

	ready := byID["ready-fixture"]
	if ready.AccessToken != "access-ready-fixture" || ready.RefreshToken != "refresh-ready-fixture" {
		t.Fatalf("ready credentials = %#v", ready)
	}
	if ready.OIDCIssuer != "https://issuer.example.test" || ready.OIDCClientID != "client-ready" {
		t.Fatalf("ready OIDC fields = %#v", ready)
	}
	if ready.Email != "ready@example.test" || ready.UserID != "user-ready" {
		t.Fatalf("ready identity fields = %#v", ready)
	}
	if ready.Pool != account.PoolReady || ready.UnavailableReason != "" || !ready.RetryAt.IsZero() {
		t.Fatalf("ready state = %#v", ready)
	}
	if ready.RequestCount != 17 || ready.AuthenticationFails != 2 || ready.MaxActive != 1 {
		t.Fatalf("ready counters = %#v", ready)
	}
	assertTimeEqual(t, ready.ExpiresAt, time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC), "expires_at")
	assertTimeEqual(t, ready.CreatedAt, time.Unix(1700000000, 125000000).UTC(), "created_at")
	assertTimeEqual(t, ready.UpdatedAt, time.Unix(1700000060, 500000000).UTC(), "updated_at")

	quota := byID["quota-fixture"]
	if quota.Pool != account.PoolUnavailable || quota.UnavailableReason != account.ReasonQuota || quota.RetryAt.IsZero() {
		t.Fatalf("quota state = %#v", quota)
	}
	if quota.LastErrorCode != "subscription:free-usage-exhausted" || quota.RequestCount != 23 || quota.AuthenticationFails != 5 {
		t.Fatalf("quota fields = %#v", quota)
	}

	auth := byID["auth-fixture"]
	if auth.Pool != account.PoolUnavailable || auth.UnavailableReason != account.ReasonAuth || !auth.RetryAt.IsZero() {
		t.Fatalf("auth state = %#v", auth)
	}
	if auth.LastErrorCode != "invalid-token" || auth.RequestCount != 31 || auth.AuthenticationFails != 7 {
		t.Fatalf("auth fields = %#v", auth)
	}

	cooldown := byID["cooldown-fixture"]
	if cooldown.Pool != account.PoolUnavailable || cooldown.UnavailableReason != account.ReasonCooldown {
		t.Fatalf("cooldown state = %#v", cooldown)
	}
	assertTimeEqual(t, cooldown.RetryAt, time.Unix(4070995200, 0).UTC(), "cooldown retry_at")

	db := openRawSQLite(t, database)
	var migrated string
	if err := db.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key='python_v1_migrated'`).Scan(&migrated); err != nil {
		t.Fatalf("read migration marker: %v", err)
	}
	if migrated != "4" {
		t.Fatalf("python_v1_migrated = %q; want 4", migrated)
	}
	var events int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_state_events`).Scan(&events); err != nil {
		t.Fatalf("count migration events: %v", err)
	}
	if events != 0 {
		t.Fatalf("migration event count = %d; want 0", events)
	}
}

func TestLegacyJSONFixtureImportsAllSupportedFieldsAndStates(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	count, err := repo.ImportLegacyJSON(ctx, filepath.Join("testdata", "legacy_accounts.json"))
	if err != nil {
		t.Fatalf("import fixture: %v", err)
	}
	if count != 3 {
		t.Fatalf("import count = %d; want 3", count)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list imported accounts: %v", err)
	}
	byID := accountsByID(accounts)
	if len(byID) != 3 {
		t.Fatalf("stored account count = %d; want 3", len(byID))
	}

	ready := byID["legacy-ready"]
	if ready.AccessToken != "access-legacy-ready" || ready.RefreshToken != "refresh-legacy-ready" {
		t.Fatalf("ready credentials = %#v", ready)
	}
	if ready.OIDCIssuer != "https://issuer.example.test" || ready.OIDCClientID != "legacy-client" {
		t.Fatalf("ready OIDC = %#v", ready)
	}
	if ready.Email != "legacy@example.test" || ready.UserID != "legacy-user" || ready.TeamID != "legacy-team" {
		t.Fatalf("ready identity = %#v", ready)
	}
	if ready.Pool != account.PoolReady || ready.RequestCount != 73 || ready.AuthenticationFails != 4 {
		t.Fatalf("ready state and counters = %#v", ready)
	}
	if ready.LastErrorCode != "historical-warning" || ready.MaxActive != 1 {
		t.Fatalf("ready operational fields = %#v", ready)
	}
	assertTimeEqual(t, ready.ExpiresAt, time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC), "legacy expires_at")

	quota := byID["legacy-quota"]
	if quota.Pool != account.PoolUnavailable || quota.UnavailableReason != account.ReasonQuota || quota.RetryAt.IsZero() {
		t.Fatalf("quota state = %#v", quota)
	}
	if quota.AccessToken != "access-legacy-quota" || quota.RequestCount != 79 || quota.AuthenticationFails != 5 {
		t.Fatalf("quota fields = %#v", quota)
	}

	auth := byID["legacy-auth"]
	if auth.Pool != account.PoolUnavailable || auth.UnavailableReason != account.ReasonAuth || !auth.RetryAt.IsZero() {
		t.Fatalf("auth state = %#v", auth)
	}
	if auth.RequestCount != 83 || auth.AuthenticationFails != 6 || auth.LastErrorCode != "invalid-token" {
		t.Fatalf("auth fields = %#v", auth)
	}
}

func TestOpenV4DatabaseIsIdempotentAndPreservesRowsEventsAndMetadata(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "v3.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	now := time.Date(2026, 7, 15, 12, 30, 0, 123000000, time.UTC)
	item := fullAccount(now)
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save ready account: %v", err)
	}
	item.Pool = account.PoolUnavailable
	item.UnavailableReason = account.ReasonValidating
	item.RetryAt = now.Add(10 * time.Minute)
	item.LastErrorCode = "validation-pending"
	item.UpdatedAt = now.Add(time.Minute)
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save transition: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close initial database: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		repo, err = sqlite.OpenSQLite(ctx, database)
		if err != nil {
			t.Fatalf("reopen attempt %d: %v", attempt, err)
		}
		if got := repo.SchemaVersion(ctx); got != 4 {
			t.Fatalf("attempt %d schema version = %d; want 4", attempt, got)
		}
		accounts, err := repo.ListAccounts(ctx)
		if err != nil {
			t.Fatalf("attempt %d list: %v", attempt, err)
		}
		if len(accounts) != 1 {
			t.Fatalf("attempt %d account count = %d; want 1", attempt, len(accounts))
		}
		assertFullAccount(t, accounts[0], item)
		if err := repo.Close(); err != nil {
			t.Fatalf("close attempt %d: %v", attempt, err)
		}
	}

	db := openRawSQLite(t, database)
	var eventCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_state_events`).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 2 {
		t.Fatalf("event count = %d; want 2", eventCount)
	}
	rows, err := db.QueryContext(ctx, `SELECT from_pool, to_pool, reason, error_code FROM account_state_events ORDER BY id`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	wantEvents := [][4]string{
		{"", "ready", "", ""},
		{"ready", "unavailable", "validating", "validation-pending"},
	}
	for index, want := range wantEvents {
		if !rows.Next() {
			t.Fatalf("missing event %d", index)
		}
		var got [4]string
		if err := rows.Scan(&got[0], &got[1], &got[2], &got[3]); err != nil {
			t.Fatalf("scan event %d: %v", index, err)
		}
		if got != want {
			t.Fatalf("event %d = %#v; want %#v", index, got, want)
		}
	}
	if rows.Next() {
		t.Fatal("unexpected additional event")
	}
}

func TestLegacyJSONImportRollsBackAllRowsWhenOneUpsertFails(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "rollback.db")
	repo, err := sqlite.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	db := openRawSQLite(t, database)
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER reject_legacy_quota
		BEFORE INSERT ON accounts
		WHEN NEW.id = 'legacy-quota'
		BEGIN
			SELECT RAISE(ABORT, 'fixture rejection');
		END;
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	if _, err := repo.ImportLegacyJSON(ctx, filepath.Join("testdata", "legacy_accounts.json")); err == nil {
		t.Fatal("expected import error")
	}
	count, err := repo.AccountCount(ctx)
	if err != nil {
		t.Fatalf("count after failed import: %v", err)
	}
	if count != 0 {
		t.Fatalf("account count after rollback = %d; want 0", count)
	}
}

func TestMalformedLegacyJSONDoesNotModifyExistingDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(dir, "malformed.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	if err := repo.SaveAccount(ctx, account.Account{
		ID: "preserved", AccessToken: "preserved-token", Pool: account.PoolReady,
		MaxActive: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save existing account: %v", err)
	}
	legacy := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(legacy, []byte(`{"accounts":[`), 0o600); err != nil {
		t.Fatalf("write malformed fixture: %v", err)
	}
	if _, err := repo.ImportLegacyJSON(ctx, legacy); err == nil {
		t.Fatal("expected malformed JSON error")
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list after malformed import: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "preserved" || accounts[0].AccessToken != "preserved-token" {
		t.Fatalf("accounts changed after malformed import: %#v", accounts)
	}
}

func applySQLFixture(t *testing.T, database, fixture string) {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read SQL fixture: %v", err)
	}
	db := openRawSQLite(t, database)
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("apply SQL fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}
}

func openRawSQLite(t *testing.T, database string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func accountsByID(accounts []account.Account) map[string]account.Account {
	result := make(map[string]account.Account, len(accounts))
	for _, item := range accounts {
		result[item.ID] = item
	}
	return result
}

func fullAccount(now time.Time) account.Account {
	return account.Account{
		ID:                  "full-v3-account",
		AccessToken:         "full-access-token",
		RefreshToken:        "full-refresh-token",
		ExpiresAt:           now.Add(2 * time.Hour),
		OIDCIssuer:          "https://issuer.example.test",
		OIDCClientID:        "full-client",
		Email:               "full@example.test",
		UserID:              "full-user",
		TeamID:              "full-team",
		Pool:                account.PoolReady,
		LastErrorCode:       "",
		LastSuccessAt:       now.Add(-time.Minute),
		QuotaActual:         123456,
		QuotaLimit:          1000000,
		RequestCount:        987,
		AuthenticationFails: 3,
		MaxActive:           7,
		CreatedAt:           now.Add(-24 * time.Hour),
		UpdatedAt:           now,
	}
}

func assertFullAccount(t *testing.T, got, want account.Account) {
	t.Helper()
	if got.ID != want.ID || got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Fatalf("credentials mismatch: got %#v want %#v", got, want)
	}
	if got.OIDCIssuer != want.OIDCIssuer || got.OIDCClientID != want.OIDCClientID || got.Email != want.Email || got.UserID != want.UserID || got.TeamID != want.TeamID {
		t.Fatalf("identity mismatch: got %#v want %#v", got, want)
	}
	if got.Pool != want.Pool || got.UnavailableReason != want.UnavailableReason || got.LastErrorCode != want.LastErrorCode {
		t.Fatalf("state mismatch: got %#v want %#v", got, want)
	}
	if got.QuotaActual != want.QuotaActual || got.QuotaLimit != want.QuotaLimit || got.RequestCount != want.RequestCount || got.AuthenticationFails != want.AuthenticationFails || got.MaxActive != want.MaxActive {
		t.Fatalf("counter mismatch: got %#v want %#v", got, want)
	}
	assertTimeEqual(t, got.ExpiresAt, want.ExpiresAt, "expires_at")
	assertTimeEqual(t, got.RetryAt, want.RetryAt, "retry_at")
	assertTimeEqual(t, got.LastSuccessAt, want.LastSuccessAt, "last_success_at")
	assertTimeEqual(t, got.CreatedAt, want.CreatedAt, "created_at")
	assertTimeEqual(t, got.UpdatedAt, want.UpdatedAt, "updated_at")
}

func assertTimeEqual(t *testing.T, got, want time.Time, field string) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s = %s; want %s", field, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}
