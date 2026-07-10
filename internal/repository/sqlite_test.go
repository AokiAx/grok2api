package repository_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
