package repository_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/repository"
)

func TestSQLiteMigrationCreatesTwoPoolSchema(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "grok2api.db")
	repo, err := repository.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if got := repo.SchemaVersion(ctx); got != 3 {
		t.Fatalf("schema version = %d; want 3", got)
	}
}

func TestLegacyDisabledAccountsMigrateByFailureReason(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database := filepath.Join(dir, "grok2api.db")
	legacy := filepath.Join(dir, "cli_accounts.json")
	payload := map[string]any{
		"accounts": []map[string]any{
			{
				"id":              "quota-account",
				"key":             "quota-token",
				"refresh_token":   "quota-refresh",
				"enabled":         false,
				"fail_count":      5,
				"last_error_code": "subscription:free-usage-exhausted",
			},
			{
				"id":              "auth-account",
				"key":             "auth-token",
				"refresh_token":   "auth-refresh",
				"enabled":         false,
				"fail_count":      5,
				"last_error_code": "invalid-token",
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	repo, err := repository.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if _, err := repo.ImportLegacyJSON(ctx, legacy); err != nil {
		t.Fatalf("import legacy: %v", err)
	}

	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("account count = %d; want 3", len(accounts))
	}
	byID := map[string]account.Account{}
	for _, item := range accounts {
		byID[item.ID] = item
	}
	if byID["quota-account"].UnavailableReason != account.ReasonQuota {
		t.Fatalf("quota reason = %q", byID["quota-account"].UnavailableReason)
	}
	if byID["auth-account"].UnavailableReason != account.ReasonAuth {
		t.Fatalf("auth reason = %q", byID["auth-account"].UnavailableReason)
	}
}

func TestLegacyDisabledHeuristicSeparatesExpiredAuthAndQuota(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database := filepath.Join(dir, "grok2api.db")
	legacy := filepath.Join(dir, "cli_accounts.json")
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	payload := map[string]any{
		"accounts": []map[string]any{
			{"id": "expired", "key": "token-expired", "enabled": false, "fail_count": 5, "expires_at": past},
			{"id": "quota", "key": "token-quota", "enabled": false, "fail_count": 5, "expires_at": future},
		},
	}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	repo, err := repository.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if _, err := repo.ImportLegacyJSON(ctx, legacy); err != nil {
		t.Fatalf("import legacy: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	byID := map[string]account.Account{}
	for _, item := range accounts {
		byID[item.ID] = item
	}
	if byID["expired"].UnavailableReason != account.ReasonAuth {
		t.Fatalf("expired reason = %q; want auth", byID["expired"].UnavailableReason)
	}
	if byID["quota"].UnavailableReason != account.ReasonQuota {
		t.Fatalf("quota reason = %q; want quota", byID["quota"].UnavailableReason)
	}
	if byID["quota"].RetryAt.IsZero() {
		t.Fatal("quota account must receive a staggered retry time")
	}
}

func TestSaveAccountPersistsPoolTransitionAndEvent(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "grok2api.db")
	repo, err := repository.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	item := account.Account{
		ID:                "quota-account",
		AccessToken:       "token",
		Pool:              account.PoolUnavailable,
		UnavailableReason: account.ReasonQuota,
		RetryAt:           now.Add(time.Hour),
		LastErrorCode:     "subscription:free-usage-exhausted",
		QuotaActual:       1_023_321,
		QuotaLimit:        1_000_000,
		MaxActive:         1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save account: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].QuotaActual != 1_023_321 {
		t.Fatalf("saved accounts = %#v", accounts)
	}

	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer db.Close()
	var events int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_state_events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("event count = %d; want 1", events)
	}
}

func TestAccountCount(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "grok2api.db")
	repo, err := repository.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if got, err := repo.AccountCount(ctx); err != nil || got != 0 {
		t.Fatalf("empty count = %d, %v", got, err)
	}
	now := time.Now().UTC()
	if err := repo.SaveAccount(ctx, account.Account{
		ID:          "ready",
		AccessToken: "token",
		Pool:        account.PoolReady,
		MaxActive:   1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	if got, err := repo.AccountCount(ctx); err != nil || got != 1 {
		t.Fatalf("count = %d, %v; want 1", got, err)
	}
}

func TestDeleteAccountRemovesStoredCredential(t *testing.T) {
	ctx := context.Background()
	repo, err := repository.OpenSQLite(ctx, filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	now := time.Now().UTC()
	item := account.Account{
		ID: "delete-me", AccessToken: "token", Pool: account.PoolReady,
		CreatedAt: now, UpdatedAt: now, MaxActive: 1,
	}
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := repo.DeleteAccount(ctx, item.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func TestOpenSQLiteMigratesPythonV1AccountTable(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "python-v1.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatalf("open fixture sqlite: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO app_meta(key, value) VALUES('schema_version', '1');
		CREATE TABLE cli_accounts (
			id TEXT PRIMARY KEY,
			identity_key TEXT NOT NULL UNIQUE,
			key TEXT NOT NULL,
			refresh_token TEXT,
			expires_at TEXT,
			oidc_issuer TEXT NOT NULL,
			oidc_client_id TEXT NOT NULL,
			email TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			request_count INTEGER NOT NULL DEFAULT 0,
			fail_count INTEGER NOT NULL DEFAULT 0,
			cooldown_until REAL,
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL,
			disabled_reason TEXT NOT NULL DEFAULT ''
		);
	`)
	if err != nil {
		t.Fatalf("create Python v1 schema: %v", err)
	}
	now := time.Now().UTC()
	rows := []struct {
		id             string
		enabled        int
		failCount      int
		cooldownUntil  any
		disabledReason string
	}{
		{id: "ready", enabled: 1},
		{id: "quota", enabled: 0, failCount: 5, disabledReason: "subscription:free-usage-exhausted"},
		{id: "auth", enabled: 0, failCount: 5, disabledReason: "invalid-token"},
		{id: "cooldown", enabled: 1, cooldownUntil: now.Add(10 * time.Minute).Unix()},
	}
	for _, row := range rows {
		_, err := db.Exec(`INSERT INTO cli_accounts (
			id, identity_key, key, refresh_token, expires_at, oidc_issuer,
			oidc_client_id, email, user_id, enabled, request_count, fail_count,
			cooldown_until, created_at, updated_at, disabled_reason
		) VALUES (?, ?, ?, '', '', 'https://auth.x.ai', 'client', ?, '', ?, 0, ?, ?, ?, ?, ?)`,
			row.id,
			"id:"+row.id,
			"token-"+row.id,
			row.id+"@example.com",
			row.enabled,
			row.failCount,
			row.cooldownUntil,
			now.Add(-time.Hour).Unix(),
			now.Unix(),
			row.disabledReason,
		)
		if err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture sqlite: %v", err)
	}

	repo, err := repository.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open migrated sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list migrated accounts: %v", err)
	}
	if len(accounts) != len(rows) {
		t.Fatalf("account count = %d; want %d", len(accounts), len(rows))
	}
	byID := make(map[string]account.Account, len(accounts))
	for _, item := range accounts {
		byID[item.ID] = item
	}
	if byID["ready"].Pool != account.PoolReady {
		t.Fatalf("ready = %#v", byID["ready"])
	}
	if byID["quota"].UnavailableReason != account.ReasonQuota || byID["quota"].RetryAt.IsZero() {
		t.Fatalf("quota = %#v", byID["quota"])
	}
	if byID["auth"].UnavailableReason != account.ReasonAuth {
		t.Fatalf("auth = %#v", byID["auth"])
	}
	if byID["cooldown"].UnavailableReason != account.ReasonCooldown || byID["cooldown"].RetryAt.IsZero() {
		t.Fatalf("cooldown = %#v", byID["cooldown"])
	}
	if repo.SchemaVersion(ctx) != 3 {
		t.Fatalf("schema version = %d", repo.SchemaVersion(ctx))
	}
}

func TestLegacyEnabledExpiredCredentialMigratesToAuth(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy := filepath.Join(dir, "cli_accounts.json")
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	data, err := json.Marshal(map[string]any{"accounts": []map[string]any{{
		"id": "expired", "key": "token", "refresh_token": "refresh",
		"enabled": true, "expires_at": past,
	}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	repo, err := repository.OpenSQLite(ctx, filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if _, err := repo.ImportLegacyJSON(ctx, legacy); err != nil {
		t.Fatalf("import: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 || accounts[0].UnavailableReason != account.ReasonAuth {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func TestSaveAccountPersistsQuotaAndLastSuccess(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "quota.db")
	repo, err := repository.OpenSQLite(ctx, database)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC().Truncate(time.Millisecond)
	item := account.Account{
		ID:            "free-1",
		AccessToken:   "token",
		Pool:          account.PoolReady,
		QuotaActual:   250_000,
		QuotaLimit:    1_000_000,
		LastSuccessAt: now,
		MaxActive:     1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save: %v", err)
	}
	// update again to ensure ON CONFLICT persists quota fields
	item.QuotaActual = 300_000
	item.LastSuccessAt = now.Add(time.Minute)
	item.UpdatedAt = now.Add(time.Minute)
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("update: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("count = %d", len(accounts))
	}
	got := accounts[0]
	if got.QuotaActual != 300_000 || got.QuotaLimit != 1_000_000 {
		t.Fatalf("quota = %d/%d", got.QuotaActual, got.QuotaLimit)
	}
	if got.LastSuccessAt.IsZero() {
		t.Fatal("last_success_at not persisted")
	}
}

func TestSaveAccountPersistsTeamID(t *testing.T) {
	ctx := context.Background()
	repo, err := repository.OpenSQLite(ctx, filepath.Join(t.TempDir(), "team.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	item := account.Account{
		ID:          "team-acc",
		AccessToken: "token",
		Email:       "a@example.com",
		UserID:      "user-1",
		TeamID:      "team-9",
		Pool:        account.PoolReady,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		MaxActive:   1,
	}
	if err := repo.SaveAccount(ctx, item); err != nil {
		t.Fatalf("save: %v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accounts) != 1 || accounts[0].TeamID != "team-9" {
		t.Fatalf("accounts = %#v", accounts)
	}
}
