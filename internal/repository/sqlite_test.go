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

	if got := repo.SchemaVersion(ctx); got != 2 {
		t.Fatalf("schema version = %d; want 2", got)
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
		t.Fatalf("account count = %d; want 2", len(accounts))
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
